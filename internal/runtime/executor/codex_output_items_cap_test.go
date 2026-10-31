package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
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
	if len(buf.byIndex) != 0 || len(buf.fallback) != 0 || buf.retained != 0 {
		t.Fatalf("dropped buffer should release retained items, got byIndex=%d fallback=%d retained=%d", len(buf.byIndex), len(buf.fallback), buf.retained)
	}

	buf.collect(small)
	if len(buf.byIndex) != 0 {
		t.Fatalf("dropped buffer should keep ignoring events, got byIndex=%d", len(buf.byIndex))
	}
	// Output was still produced, so the empty-incomplete check must not fire.
	if buf.count() != 3 {
		t.Fatalf("count() = %d, want 3 observed items", buf.count())
	}
	completed := []byte(`{"type":"response.completed","response":{"output":[]}}`)
	if got := buf.patch(completed); string(got) != string(completed) {
		t.Fatalf("dropped buffer patch should pass through, got %s", got)
	}
}

func TestCodexOutputItemPatchBufferRepeatedIndexCountsOnce(t *testing.T) {
	buf := newCodexOutputItemPatchBuffer()
	item := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"` + strings.Repeat("x", codexOutputItemsRetainLimit/2) + `"}}`)
	for i := 0; i < 3; i++ {
		buf.collect(item)
	}
	if buf.dropped {
		t.Fatalf("a replaced index must not be counted twice, retained=%d", buf.retained)
	}
	if want := len(gjson.GetBytes(item, "item").Raw); buf.retained != want {
		t.Fatalf("retained = %d, want %d", buf.retained, want)
	}
}

func TestCodexOutputItemPatchBufferEvictsImagesBeforeDroppingAll(t *testing.T) {
	buf := newCodexOutputItemPatchBuffer()
	buf.collect([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs1"}}`))
	image := `{"type":"response.output_item.done","output_index":%d,"item":{"type":"image_generation_call","id":"ig%d","result":"` + strings.Repeat("A", codexOutputItemsRetainLimit/2) + `"}}`
	for i := 1; i <= 3; i++ {
		buf.collect([]byte(fmt.Sprintf(image, i, i)))
	}
	buf.collect([]byte(`{"type":"response.output_item.done","output_index":4,"item":{"type":"message","id":"m1"}}`))
	if buf.dropped {
		t.Fatal("buffer should evict images instead of dropping every item")
	}

	patched := buf.patch([]byte(`{"type":"response.completed","response":{"output":[]}}`))
	output := gjson.GetBytes(patched, "response.output").Array()
	if len(output) != 2 || output[0].Get("id").String() != "rs1" || output[1].Get("id").String() != "m1" {
		t.Fatalf("patched output = %s, want reasoning and message without images", gjson.GetBytes(patched, "response.output").Raw)
	}
	if buf.count() != 5 {
		t.Fatalf("count() = %d, want 5 observed items", buf.count())
	}
}

func TestCodexOutputItemPatchBufferUsageCountsEvictedImages(t *testing.T) {
	buf := newCodexOutputItemPatchBuffer()
	image := `{"type":"response.output_item.done","output_index":%d,"item":{"type":"image_generation_call","id":"ig%d","status":"completed","size":"1024x1024","result":"` + strings.Repeat("A", codexOutputItemsRetainLimit/2) + `"}}`
	for i := 0; i < 3; i++ {
		buf.collect([]byte(fmt.Sprintf(image, i, i)))
	}
	byIndex, fallback := buf.usageItems()
	if len(byIndex) != 0 || len(fallback) != 3 {
		t.Fatalf("usage items byIndex=%d fallback=%d, want 0 and 3 image stubs", len(byIndex), len(fallback))
	}
	for _, item := range fallback {
		if gjson.GetBytes(item, "result").Exists() {
			t.Fatal("usage stub should not keep the base64 result")
		}
	}

	completed := []byte(`{"type":"response.completed","response":{"output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
	detail, ok := helps.ParseCodexUsageWithOutputItems(completed, byIndex, fallback)
	if !ok || detail.ToolUsageDetails == nil || detail.ToolUsageDetails.ImageGenerationCalls == nil || *detail.ToolUsageDetails.ImageGenerationCalls != 3 {
		t.Fatalf("image generation calls = %+v, want 3", detail.ToolUsageDetails)
	}
}
