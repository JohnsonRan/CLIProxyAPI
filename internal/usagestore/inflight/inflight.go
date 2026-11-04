// Package inflight tracks the downstream calls the server is serving right now so the usage
// realtime view can list them before their usage record exists. It depends only on gin so
// executor helpers can report upstream attempts without importing the usage store.
package inflight

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const ginKey = "__usage_inflight_request__"

// Upstream describes the latest upstream attempt of a call; empty until an executor starts one.
type Upstream struct {
	Provider  string
	Model     string
	AuthIndex string
	APIKey    string
	Stream    bool
}

// Request is a copy of one call being served.
type Request struct {
	ID        uint64
	RequestID string
	StartedAt time.Time
	Endpoint  string
	WebSocket bool
	Upstream
}

type entry struct {
	Request
	mu sync.Mutex
}

var (
	mu     sync.Mutex
	nextID uint64
	active = map[uint64]*entry{}
)

// Begin registers a call and binds it to c so executors can attach upstream details.
// The returned function removes it and must be called when the handler returns.
func Begin(c *gin.Context, requestID, endpoint string, webSocket bool) (end func()) {
	e := &entry{Request: Request{RequestID: requestID, StartedAt: time.Now(), Endpoint: endpoint, WebSocket: webSocket}}
	mu.Lock()
	nextID++
	e.ID = nextID
	active[e.ID] = e
	mu.Unlock()
	if c != nil {
		c.Set(ginKey, e)
	}
	return func() {
		mu.Lock()
		delete(active, e.ID)
		mu.Unlock()
	}
}

// Count returns the number of calls being served.
func Count() int {
	mu.Lock()
	defer mu.Unlock()
	return len(active)
}

// Attach records the upstream attempt for the call whose gin context ctx carries (the
// handlers store it under "gin"). Retries overwrite earlier attempts. No-op outside a call.
func Attach(ctx context.Context, u Upstream) {
	if ctx == nil {
		return
	}
	c, ok := ctx.Value("gin").(*gin.Context)
	if !ok || c == nil {
		return
	}
	v, ok := c.Get(ginKey)
	if !ok {
		return
	}
	e, ok := v.(*entry)
	if !ok || e == nil {
		return
	}
	e.mu.Lock()
	e.Upstream = u
	e.mu.Unlock()
}

// List returns the calls being served, newest first.
func List() []Request {
	mu.Lock()
	out := make([]Request, 0, len(active))
	for _, e := range active {
		e.mu.Lock()
		out = append(out, e.Request)
		e.mu.Unlock()
	}
	mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}
