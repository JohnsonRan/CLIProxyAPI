package openai

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type httpIncrementalCaptureExecutor struct {
	mu       sync.Mutex
	payloads [][]byte
}

func (e *httpIncrementalCaptureExecutor) Identifier() string { return "codex" }

func (e *httpIncrementalCaptureExecutor) PrepareRequest(*http.Request, *coreauth.Auth) error {
	return nil
}

func (e *httpIncrementalCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *httpIncrementalCaptureExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (e *httpIncrementalCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *httpIncrementalCaptureExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	e.payloads = append(e.payloads, bytes.Clone(req.Payload))
	turn := len(e.payloads)
	e.mu.Unlock()

	if turn == 1 {
		return coreexecutor.Response{
			Payload: []byte(`{"id":"resp_http_turn_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}]}`),
		}, nil
	}
	return coreexecutor.Response{
		Payload: []byte(`{"id":"resp_http_turn_2","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_out_2","role":"assistant","content":[{"type":"output_text","text":"Sunny in Tokyo"}]}]}`),
	}, nil
}

func (e *httpIncrementalCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.payloads = append(e.payloads, bytes.Clone(req.Payload))
	turn := len(e.payloads)
	e.mu.Unlock()

	ch := make(chan coreexecutor.StreamChunk, 3)
	if turn == 1 {
		ch <- coreexecutor.StreamChunk{Payload: []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_sse_turn_1\",\"status\":\"in_progress\",\"output\":[]}}\n\n")}
		ch <- coreexecutor.StreamChunk{Payload: []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_sse_1\",\"call_id\":\"call_sse_1\",\"name\":\"lookup\",\"arguments\":\"{}\"}}\n\n")}
		ch <- coreexecutor.StreamChunk{Payload: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_sse_turn_1\",\"status\":\"completed\",\"output\":[]}}\n\n")}
	} else {
		ch <- coreexecutor.StreamChunk{Payload: []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_sse_turn_2\",\"status\":\"in_progress\",\"output\":[]}}\n\n")}
		ch <- coreexecutor.StreamChunk{Payload: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_sse_turn_2\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_sse_2\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}]}}\n\n")}
	}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func TestHTTPResponsesIncrementalSSEAndNonStreamContinuation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const model = "gpt-5.4-http-incremental-test"
	const authID = "http-incremental-auth"

	for _, stream := range []bool{true, false} {
		name := "non_stream"
		if stream {
			name = "sse_stream"
		}
		t.Run(name, func(t *testing.T) {
			exec := &httpIncrementalCaptureExecutor{}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetConfig(&config.Config{})
			manager.RegisterExecutor(exec)
			if _, err := manager.Register(context.Background(), &coreauth.Auth{
				ID:       authID,
				Provider: "codex",
				Status:   coreauth.StatusActive,
			}); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

			handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager))
			router := gin.New()
			router.POST("/v1/responses", handler.Responses)

			turn1Body := `{"model":"` + model + `","stream":` + boolString(stream) + `,"instructions":"Be concise","tools":[{"type":"function","name":"lookup"}],"input":[{"type":"message","id":"u1","role":"user","content":"check weather"}]}`
			req1 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(turn1Body))
			req1.Header.Set("Content-Type", "application/json")
			rec1 := httptest.NewRecorder()
			router.ServeHTTP(rec1, req1)
			if rec1.Code != http.StatusOK {
				t.Fatalf("turn 1 status = %d, body = %s", rec1.Code, rec1.Body.String())
			}

			prevID := "resp_http_turn_1"
			callID := "call_1"
			if stream {
				prevID = "resp_sse_turn_1"
				callID = "call_sse_1"
			}

			// Turn 2 sends ONLY previous_response_id + incremental function_call_output, omitting instructions and tools.
			turn2Body := `{"previous_response_id":"` + prevID + `","stream":` + boolString(stream) + `,"input":[{"type":"function_call_output","call_id":"` + callID + `","output":"22C"}]}`
			req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(turn2Body))
			req2.Header.Set("Content-Type", "application/json")
			rec2 := httptest.NewRecorder()
			router.ServeHTTP(rec2, req2)
			if rec2.Code != http.StatusOK {
				t.Fatalf("turn 2 status = %d, body = %s", rec2.Code, rec2.Body.String())
			}

			exec.mu.Lock()
			if len(exec.payloads) != 2 {
				exec.mu.Unlock()
				t.Fatalf("captured payloads = %d, want 2", len(exec.payloads))
			}
			secondUpstream := exec.payloads[1]
			exec.mu.Unlock()

			if gjson.GetBytes(secondUpstream, "previous_response_id").Exists() {
				t.Fatalf("expanded HTTP payload must strip previous_response_id for stateless upstream: %s", secondUpstream)
			}
			if got := gjson.GetBytes(secondUpstream, "instructions").String(); got != "Be concise" {
				t.Fatalf("inherited instructions = %q, want 'Be concise': %s", got, secondUpstream)
			}
			if got := gjson.GetBytes(secondUpstream, "tools.0.name").String(); got != "lookup" {
				t.Fatalf("inherited tool = %q, want 'lookup': %s", got, secondUpstream)
			}
			items := gjson.GetBytes(secondUpstream, "input").Array()
			if len(items) != 3 {
				t.Fatalf("merged input items = %d, want 3 (user message + function_call + function_call_output): %s", len(items), secondUpstream)
			}
			if items[0].Get("id").String() != "u1" || items[1].Get("call_id").String() != callID || items[2].Get("call_id").String() != callID {
				t.Fatalf("unexpected merged input items: %s", secondUpstream)
			}
			if !strings.Contains(rec2.Body.String(), prevID) {
				_, _ = io.Copy(io.Discard, rec2.Body)
				t.Fatalf("turn 2 response did not preserve previous_response_id %q: %s", prevID, rec2.Body.String())
			}
		})
	}
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func TestNormalizeResponsesWebsocketStandardPayloadAndToolInheritance(t *testing.T) {
	t.Run("standard payload without type and with string input", func(t *testing.T) {
		raw := []byte(`{"model":"gpt-5.4","input":"hello standard ws"}`)
		normalized, last, errMsg := normalizeResponsesWebsocketRequest(raw, nil, nil)
		if errMsg != nil {
			t.Fatalf("normalizeResponsesWebsocketRequest() error = %v", errMsg.Error)
		}
		if got := gjson.GetBytes(normalized, "model").String(); got != "gpt-5.4" {
			t.Fatalf("model = %q, want gpt-5.4", got)
		}
		if got := gjson.GetBytes(normalized, "input.0.content.0.text").String(); got != "hello standard ws" {
			t.Fatalf("coerced input text = %q, want 'hello standard ws': %s", got, normalized)
		}
		if !bytes.Equal(normalized, last) {
			t.Fatal("last snapshot mismatch")
		}
	})

	t.Run("incremental turn inherits top-level tools and prewarm additional_tools on replacement", func(t *testing.T) {
		lastReq := []byte(`{"model":"gpt-5.4","stream":true,"instructions":"sys","tools":[{"type":"function","name":"bash"}],"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"exec"}]},{"type":"message","id":"m1","role":"user","content":"hi"}]}`)
		lastOut := []byte(`[{"type":"function_call","id":"fc1","call_id":"c1","name":"bash","arguments":"{}"}]`)
		delta := []byte(`{"type":"response.create","previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)

		normalized, _, errMsg := normalizeResponsesWebsocketRequestWithMode(delta, lastReq, lastOut, false, true)
		if errMsg != nil {
			t.Fatalf("normalize error: %v", errMsg.Error)
		}
		if got := gjson.GetBytes(normalized, "tools.0.name").String(); got != "bash" {
			t.Fatalf("inherited top-level tool = %q, want bash: %s", got, normalized)
		}

		replacement := []byte(`{"type":"response.create","input":[{"type":"message","role":"assistant","id":"a1","content":"prev"},{"type":"message","role":"user","id":"u2","content":"next"}]}`)
		repNorm, _, errRep := normalizeResponsesWebsocketRequestWithMode(replacement, lastReq, lastOut, false, true)
		if errRep != nil {
			t.Fatalf("replacement error: %v", errRep.Error)
		}
		if got := gjson.GetBytes(repNorm, "input.0.type").String(); got != "additional_tools" {
			t.Fatalf("input[0].type = %q, want additional_tools: %s", got, repNorm)
		}
		if got := gjson.GetBytes(repNorm, "tools.0.name").String(); got != "bash" {
			t.Fatalf("replacement inherited top-level tool = %q, want bash: %s", got, repNorm)
		}
	})
}

func newHTTPIncrementalTestRouter(t *testing.T, exec coreauth.ProviderExecutor, provider, authID, model string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(&config.Config{})
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: provider, Status: coreauth.StatusActive}); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/responses", handler.Responses)
	router.GET("/v1/responses", handler.ResponsesWebsocket)
	return router
}

func TestHTTPResponsesIncrementalUnknownPreviousResponseIsRejected(t *testing.T) {
	const model = "claude-http-incremental-miss-test"
	exec := &httpIncrementalCaptureExecutor{}
	router := newHTTPIncrementalTestRouter(t, exec, "codex", "http-incremental-miss-auth", model)

	for _, stream := range []bool{true, false} {
		body := `{"model":"` + model + `","stream":` + boolString(stream) + `,"previous_response_id":"resp_unknown_miss","input":[{"type":"message","role":"user","content":"continue"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("stream=%t status = %d, want 400, body = %s", stream, rec.Code, rec.Body.String())
		}
		if got := gjson.Get(rec.Body.String(), "error.code").String(); got != "previous_response_not_found" {
			t.Fatalf("stream=%t error.code = %q, want previous_response_not_found, body = %s", stream, got, rec.Body.String())
		}
		if got := gjson.Get(rec.Body.String(), "error.param").String(); got != "previous_response_id" {
			t.Fatalf("stream=%t error.param = %q, want previous_response_id, body = %s", stream, got, rec.Body.String())
		}
	}
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.payloads) != 0 {
		t.Fatalf("a delta without its history must not reach the upstream, got %d payloads", len(exec.payloads))
	}
}

// websocketReconnectExecutor answers every turn with a unique response id.
type websocketReconnectExecutor struct {
	httpIncrementalCaptureExecutor
	prefix string
}

func (e *websocketReconnectExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.payloads = append(e.payloads, bytes.Clone(req.Payload))
	id := e.prefix + "_" + strconv.Itoa(len(e.payloads))
	e.mu.Unlock()

	ch := make(chan coreexecutor.StreamChunk, 1)
	ch <- coreexecutor.StreamChunk{Payload: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"" + id + "\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_" + id + "\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"" + id + "\"}]}]}}\n\n")}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func TestResponsesWebsocketContinuesPreviousResponseAcrossConnections(t *testing.T) {
	const model = "claude-ws-reconnect-test"
	exec := &websocketReconnectExecutor{prefix: "resp_ws_reconnect"}
	router := newHTTPIncrementalTestRouter(t, exec, "codex", "ws-reconnect-auth", model)
	server := httptest.NewServer(router)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"

	turn := func(request string) []byte {
		t.Helper()
		conn, _, errDial := websocket.DefaultDialer.Dial(wsURL, nil)
		if errDial != nil {
			t.Fatalf("dial websocket: %v", errDial)
		}
		defer func() { _ = conn.Close() }()
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(request)); errWrite != nil {
			t.Fatalf("write websocket request: %v", errWrite)
		}
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Fatalf("read websocket response: %v", errRead)
		}
		return payload
	}

	// Turn 1 on its own socket, turn 2 on a fresh socket, turn 3 over HTTP:
	// every hop continues from the previous id with only the delta.
	first := turn(`{"type":"response.create","model":"` + model + `","instructions":"Be concise","input":[{"type":"message","id":"u1","role":"user","content":"hi"}]}`)
	if got := gjson.GetBytes(first, "response.id").String(); got != "resp_ws_reconnect_1" {
		t.Fatalf("turn 1 response = %s", first)
	}
	second := turn(`{"type":"response.create","previous_response_id":"resp_ws_reconnect_1","input":[{"type":"message","id":"u2","role":"user","content":"again"}]}`)
	if got := gjson.GetBytes(second, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("turn 2 on a new socket = %s, want %s", second, wsEventTypeCompleted)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"previous_response_id":"resp_ws_reconnect_2","input":[{"type":"message","id":"u3","role":"user","content":"last"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP continuation of a websocket turn status = %d, body = %s", rec.Code, rec.Body.String())
	}

	exec.mu.Lock()
	payloads := append([][]byte(nil), exec.payloads...)
	exec.mu.Unlock()
	if len(payloads) != 3 {
		t.Fatalf("upstream payloads = %d, want 3", len(payloads))
	}
	wantIDs := [][]string{
		{"u1", "msg_resp_ws_reconnect_1", "u2"},
		{"u1", "msg_resp_ws_reconnect_1", "u2", "msg_resp_ws_reconnect_2", "u3"},
	}
	for i, want := range wantIDs {
		payload := payloads[i+1]
		if gjson.GetBytes(payload, "previous_response_id").Exists() {
			t.Fatalf("turn %d kept previous_response_id: %s", i+2, payload)
		}
		if got := gjson.GetBytes(payload, "model").String(); got != model {
			t.Fatalf("turn %d model = %q, want inherited %q: %s", i+2, got, model, payload)
		}
		if got := gjson.GetBytes(payload, "instructions").String(); got != "Be concise" {
			t.Fatalf("turn %d instructions = %q, want inherited: %s", i+2, got, payload)
		}
		var got []string
		for _, item := range gjson.GetBytes(payload, "input").Array() {
			got = append(got, item.Get("id").String())
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("turn %d input ids = %v, want %v: %s", i+2, got, want, payload)
		}
	}
}
