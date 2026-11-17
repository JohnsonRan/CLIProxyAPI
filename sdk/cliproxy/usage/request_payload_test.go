package usage

import (
	"context"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"
)

type blockingCapturePlugin struct {
	received chan context.Context
}

func (p *blockingCapturePlugin) HandleUsage(ctx context.Context, _ Record) {
	select {
	case p.received <- ctx:
	default:
	}
}

type blockingReachabilityPlugin struct {
	unblock  chan struct{}
	mu       sync.Mutex
	payloads [][]byte
	done     chan struct{}
	expected int
}

func (p *blockingReachabilityPlugin) HandleUsage(ctx context.Context, _ Record) {
	<-p.unblock
	p.mu.Lock()
	defer p.mu.Unlock()
	p.payloads = append(p.payloads, RequestPayloadFromContext(ctx))
	if len(p.payloads) == p.expected {
		close(p.done)
	}
}

func TestRequestPayloadQueueReachabilityAndCleanup(t *testing.T) {
	SetRequestPayloadCapture(true)
	defer SetRequestPayloadCapture(false)

	mgr := NewManager(32)
	defer mgr.Stop()

	plugin := &blockingReachabilityPlugin{
		unblock:  make(chan struct{}),
		done:     make(chan struct{}),
		expected: 17,
	}
	mgr.Register(plugin)
	mgr.Start(context.Background())

	// Publish 16 unflagged requests with 4-MiB bodies each (64 MiB total).
	const bodySize = 4 * 1024 * 1024
	publishUnflagged := func(id byte) weak.Pointer[byte] {
		body := make([]byte, bodySize)
		body[0] = id
		wp := weak.Make(&body[0])
		ctx := WithRequestPayload(context.Background(), body)
		mgr.Publish(ctx, Record{
			Model:         "gpt-4",
			ResponseModel: "gpt-4",
		})
		return wp
	}

	var m0 runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m0)

	weakPointers := make([]weak.Pointer[byte], 16)
	for i := 0; i < 16; i++ {
		weakPointers[i] = publishUnflagged(byte(i + 1))
	}

	// Publish 1 flagged record with a separate body.
	publishFlagged := func() {
		flaggedBody := []byte("flagged prompt content")
		ctx := WithRequestPayload(context.Background(), flaggedBody)
		mgr.Publish(ctx, Record{
			Model:        "gpt-4",
			UpstreamRisk: "cyber_policy",
		})
	}
	publishFlagged()

	// While the plugin is blocked and all 17 records sit in the usage queue,
	// run GC and verify that all 16 unflagged bodies were collected because
	// the queued contexts held only weak pointers.
	for i := 0; i < 5; i++ {
		runtime.GC()
	}

	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	t.Logf("HeapAlloc delta after 16x 4MiB unflagged publishes + GC: %d KiB (m0=%d KiB, m1=%d KiB)",
		(int64(m1.HeapAlloc)-int64(m0.HeapAlloc))/1024, m0.HeapAlloc/1024, m1.HeapAlloc/1024)

	for i, wp := range weakPointers {
		if val := wp.Value(); val != nil {
			t.Fatalf("unflagged body %d still strongly reachable in usage queue", i)
		}
	}

	// Unblock plugin and verify queued items.
	close(plugin.unblock)

	select {
	case <-plugin.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for plugin to process queued records")
	}

	plugin.mu.Lock()
	defer plugin.mu.Unlock()

	// First 16 unflagged items must have nil payload.
	for i := 0; i < 16; i++ {
		if plugin.payloads[i] != nil {
			t.Fatalf("unflagged record %d unexpectedly retained payload: %q", i, plugin.payloads[i])
		}
	}

	// 17th item was flagged, so Manager.Publish snapshotted it strongly.
	if got := string(plugin.payloads[16]); got != "flagged prompt content" {
		t.Fatalf("flagged record expected 'flagged prompt content', got %q", got)
	}
}

func TestRequestPayloadReloadShadowing(t *testing.T) {
	SetRequestPayloadCapture(true)
	defer SetRequestPayloadCapture(false)

	ctx1 := WithRequestPayload(context.Background(), []byte("payloadA"))
	if got := RequestPayloadFromContext(ctx1); string(got) != "payloadA" {
		t.Fatalf("expected payloadA, got %q", got)
	}

	// Flip capture off: WithRequestPayload must shadow outer payload rather than inheriting it.
	SetRequestPayloadCapture(false)
	ctx2 := WithRequestPayload(ctx1, []byte("payloadB"))

	// Re-enable capture: ctx2 must still return nil (not payloadA).
	SetRequestPayloadCapture(true)
	if got := RequestPayloadFromContext(ctx2); got != nil {
		t.Fatalf("expected nil when attached while disabled, got %q", got)
	}
	if got := RequestPayloadFromContext(ctx1); string(got) != "payloadA" {
		t.Fatalf("ctx1 must still have payloadA, got %q", got)
	}
}

func TestRequestPayloadGating(t *testing.T) {
	SetRequestPayloadCapture(false)
	ctx := context.Background()
	payload := []byte(`{"input":"test payload"}`)
	ctx = WithRequestPayload(ctx, payload)
	if got := RequestPayloadFromContext(ctx); got != nil {
		t.Fatalf("expected nil payload when capture is disabled, got %q", got)
	}

	SetRequestPayloadCapture(true)
	defer SetRequestPayloadCapture(false)

	ctx = WithRequestPayload(context.Background(), payload)
	if got := RequestPayloadFromContext(ctx); string(got) != string(payload) {
		t.Fatalf("expected attached payload, got %q", got)
	}

	mgr := NewManager(16)
	defer mgr.Stop()

	plugin := &blockingCapturePlugin{received: make(chan context.Context, 4)}
	mgr.Register(plugin)
	mgr.Start(context.Background())

	// Flagged record (UpstreamRisk set) must retain its payload in queued ctx.
	flagged := Record{
		Model:        "gpt-4",
		UpstreamRisk: "cyber_policy",
	}
	mgr.Publish(ctx, flagged)

	select {
	case queuedCtx := <-plugin.received:
		if got := RequestPayloadFromContext(queuedCtx); string(got) != string(payload) {
			t.Fatalf("flagged record must retain payload in queued ctx, got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for flagged record dispatch")
	}

	// Policy failure record must retain payload in queued ctx.
	policyFail := Record{
		Model:  "gpt-4",
		Failed: true,
		Fail: Failure{
			StatusCode: http.StatusBadRequest,
			Body:       `{"error":{"code":"cyber_policy"}}`,
		},
	}
	mgr.Publish(ctx, policyFail)

	select {
	case queuedCtx := <-plugin.received:
		if got := RequestPayloadFromContext(queuedCtx); string(got) != string(payload) {
			t.Fatalf("policy failure record must retain payload in queued ctx, got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for policy failure record dispatch")
	}

	// Substituted model record must retain payload in queued ctx.
	sub := Record{
		Model:         "gpt-6-astra",
		ResponseModel: "gpt-5.6-luna",
	}
	mgr.Publish(ctx, sub)

	select {
	case queuedCtx := <-plugin.received:
		if got := RequestPayloadFromContext(queuedCtx); string(got) != string(payload) {
			t.Fatalf("substituted record must retain payload in queued ctx, got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for substituted record dispatch")
	}
}
