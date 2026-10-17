package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// A failing stream buffers its terminal error and then closes both channels,
// which leaves select a random choice between them. The closed data channel
// must not win: that reported "stream closed before response.completed" (408)
// and dropped the socket without a close frame, so the client saw 1006 instead
// of the error event for a request fault (or the 1012 replay close the caller
// sends for a pinned credential failure). Each iteration is a fresh coin flip,
// so 64 passes rule out the old behavior with near certainty.
func TestForwardResponsesWebsocketPrefersBufferedTerminalError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

	for i := 0; i < 64; i++ {
		forwarded := make(chan *interfaces.ErrorMessage, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
			if err != nil {
				forwarded <- nil
				return
			}
			defer func() { _ = conn.Close() }()
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = r

			data := make(chan []byte)
			errs := make(chan *interfaces.ErrorMessage, 1)
			errs <- &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errors.New("upstream rejected the request")}
			close(errs)
			close(data)

			_, _, _, errMsg, _ := h.forwardResponsesWebsocket(ctx, newResponsesWebsocketWriter(conn), func(...interface{}) {}, data, errs, newInMemoryWebsocketTimelineLog(), "session-1")
			forwarded <- errMsg
		}))

		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
		if err != nil {
			server.Close()
			t.Fatalf("dial websocket: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, payload, errRead := conn.ReadMessage()
		errMsg := <-forwarded
		_ = conn.Close()
		server.Close()

		if errMsg == nil || errMsg.StatusCode != http.StatusBadRequest {
			t.Fatalf("iteration %d: forward error = %+v, want the buffered 400", i, errMsg)
		}
		// The client gets the error event, not an abnormal closure.
		if errRead != nil || !strings.Contains(string(payload), `"error"`) {
			t.Fatalf("iteration %d: client read %q, err=%v, want the error event", i, payload, errRead)
		}
	}
}
