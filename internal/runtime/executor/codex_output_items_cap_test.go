package executor

import (
	"fmt"
	"strings"
	"testing"
)

func TestCollectCodexOutputItemDoneReportsRetainedBytes(t *testing.T) {
	byIndex := make(map[int64][]byte)
	var fallback [][]byte

	indexed := []byte(`{"type":"response.output_item.done","output_index":2,"item":{"type":"message","id":"m1"}}`)
	if got := collectCodexOutputItemDone(indexed, byIndex, &fallback); got <= 0 {
		t.Fatalf("indexed item should report retained bytes, got %d", got)
	}
	if len(byIndex) != 1 || len(fallback) != 0 {
		t.Fatalf("indexed item should land in byIndex, got byIndex=%d fallback=%d", len(byIndex), len(fallback))
	}

	plain := []byte(`{"type":"response.output_item.done","item":{"type":"message","id":"m2"}}`)
	if got := collectCodexOutputItemDone(plain, byIndex, &fallback); got <= 0 {
		t.Fatalf("fallback item should report retained bytes, got %d", got)
	}
	if len(byIndex) != 1 || len(fallback) != 1 {
		t.Fatalf("plain item should land in fallback, got byIndex=%d fallback=%d", len(byIndex), len(fallback))
	}

	if got := collectCodexOutputItemDone([]byte(`{"type":"response.output_item.done"}`), byIndex, &fallback); got != 0 {
		t.Fatalf("item-less event should report 0, got %d", got)
	}
}

func TestCodexOutputItemPatchBufferCapsRetainedBytes(t *testing.T) {
	buf := newCodexOutputItemPatchBuffer()

	small := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"small"}}`)
	buf.collect(small)
	if buf.dropped {
		t.Fatal("buffer dropped before reaching the limit")
	}

	big := []byte(`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"` + strings.Repeat("x", codexOutputItemsRetainLimit) + `"}}`)
	buf.collect(big)
	if !buf.dropped {
		t.Fatal("buffer should drop once retained bytes exceed the limit")
	}
	if buf.count() != 0 {
		t.Fatalf("dropped buffer should release retained items, got count=%d", buf.count())
	}

	buf.collect(small)
	if buf.count() != 0 {
		t.Fatalf("dropped buffer should keep ignoring events, got count=%d", buf.count())
	}
	completed := []byte(`{"type":"response.completed","response":{"output":[]}}`)
	if got := buf.patch(completed); string(got) != string(completed) {
		t.Fatalf("dropped buffer patch should pass through, got %s", got)
	}
}

func TestCodexOutputItemPatchBufferRepeatedIndexStaysBounded(t *testing.T) {
	buf := newCodexOutputItemPatchBuffer()
	item := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"` + strings.Repeat("x", codexOutputItemsRetainLimit/2) + `"}}`)
	for i := 0; i < 3 && !buf.dropped; i++ {
		buf.collect(item)
	}
	if !buf.dropped {
		t.Fatal(fmt.Sprintf("buffer should drop after repeated oversized items, retained=%d", buf.retained))
	}
}
