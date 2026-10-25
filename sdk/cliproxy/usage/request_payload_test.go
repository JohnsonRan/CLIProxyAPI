package usage

import (
	"context"
	"net/http"
	"testing"
	"time"
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

func TestRequestPayloadQueueRetentionAndGating(t *testing.T) {
	// 1. With capture off, WithRequestPayload is a no-op.
	SetRequestPayloadCapture(false)
	ctx := context.Background()
	payload := []byte(`{"input":"test payload"}`)
	ctx = WithRequestPayload(ctx, payload)
	if got := RequestPayloadFromContext(ctx); got != nil {
		t.Fatalf("expected nil payload when capture is disabled, got %q", got)
	}

	// 2. With capture on, WithRequestPayload attaches payload.
	SetRequestPayloadCapture(true)
	defer SetRequestPayloadCapture(false)

	ctx = WithRequestPayload(context.Background(), payload)
	if got := RequestPayloadFromContext(ctx); string(got) != string(payload) {
		t.Fatalf("expected attached payload, got %q", got)
	}

	// Test Manager.Publish gating before asynchronous queue.
	mgr := NewManager(16)
	defer mgr.Stop()

	plugin := &blockingCapturePlugin{received: make(chan context.Context, 4)}
	mgr.Register(plugin)
	mgr.Start(context.Background())

	// 2a. An unflagged, successful record must have its payload dropped from queued ctx.
	unflagged := Record{
		Model:         "gpt-4",
		ResponseModel: "gpt-4",
	}
	mgr.Publish(ctx, unflagged)

	select {
	case queuedCtx := <-plugin.received:
		if got := RequestPayloadFromContext(queuedCtx); got != nil {
			t.Fatalf("unflagged record must drop payload from queued ctx, got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for unflagged record dispatch")
	}

	// 2b. A flagged record (e.g. UpstreamRisk set) must retain its payload in queued ctx.
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

	// 2c. A failure record with policy code in body must retain payload in queued ctx.
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

	// 2d. Substituted model record must retain payload in queued ctx.
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

func TestDropRequestPayload(t *testing.T) {
	SetRequestPayloadCapture(true)
	defer SetRequestPayloadCapture(false)

	payload := []byte("secret")
	ctx := WithRequestPayload(context.Background(), payload)
	if got := RequestPayloadFromContext(ctx); string(got) != string(payload) {
		t.Fatalf("expected payload, got %q", got)
	}

	droppedCtx := DropRequestPayload(ctx)
	if got := RequestPayloadFromContext(droppedCtx); got != nil {
		t.Fatalf("expected nil after drop, got %q", got)
	}

	// Dropping on nil or empty ctx is safe.
	if DropRequestPayload(nil) != nil {
		t.Fatal("expected nil for DropRequestPayload(nil)")
	}
	if DropRequestPayload(context.Background()) != context.Background() {
		t.Fatal("expected unchanged ctx for DropRequestPayload on ctx without payload")
	}
}
