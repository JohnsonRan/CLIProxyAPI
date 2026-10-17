package executor

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

func TestInsertCompactClaudeMessagesMatchesRebuild(t *testing.T) {
	payloads := []string{
		`{"model":"m","messages":[{"role":"user","content":"a"}],"stream":true}`,
		`{"messages":[{"role":"user","content":"a"},{"role":"user","content":[{"type":"text","text":"b"}]},{"role":"assistant","content":"c"}]}`,
		`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"c"},{"role":"user","content":"d"}],"x":1}`,
	}
	inserted := [][]string{
		{`{"role":"system","content":[{"type":"text","text":"s1"}]}`},
		{`{"role":"system","content":"s1"}`, `{"role":"system","content":"s2"}`},
	}
	for _, payload := range payloads {
		messages := util.GetGJSONBytesNoCopy([]byte(payload), "messages")
		blocks := messages.Array()
		for at := 0; at <= len(blocks); at++ {
			for _, ins := range inserted {
				want := rebuildClaudeMessagesWithInserted([]byte(payload), blocks, at, ins)
				got, ok := insertCompactClaudeMessages([]byte(payload), messages, blocks, at, ins)
				if !ok {
					t.Fatalf("compact array rejected: %s", payload)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("at %d:\n got  %s\n want %s", at, got, want)
				}
			}
		}
	}

	spaced := []byte(`{"messages":[ {"role":"user","content":"a"} , {"role":"assistant","content":"c"} ]}`)
	messages := util.GetGJSONBytesNoCopy(spaced, "messages")
	if _, ok := insertCompactClaudeMessages(spaced, messages, messages.Array(), 1, inserted[0]); ok {
		t.Fatal("non-compact array must fall back to the rebuild")
	}
}

func TestInsertClaudeMidConversationSystemBlocksCopiesOnce(t *testing.T) {
	body := largeClaudeCodeBody(100)
	blocks := []forwardedClaudeSystemPromptBlock{{Text: "Remember the rules.", CacheControl: &claudeCodeCacheControl}}
	updated := insertClaudeMidConversationSystemBlocks(body, blocks, false)
	if bytes.Equal(updated, body) {
		t.Fatal("no system block was inserted")
	}
	messages := util.GetGJSONBytesNoCopy(body, "messages")
	firstUser := firstClaudeUserMessageIndex(body)
	want := rebuildClaudeMessagesWithInserted(body, messages.Array(), claudeLeadingUserRunEnd(messages.Array(), firstUser),
		[]string{`{"role":"system","content":[` + buildForwardedSystemBlock(blocks[0].Text, blocks[0], false) + `]}`})
	if !bytes.Equal(updated, want) {
		t.Fatal("insert differs from the rebuild")
	}

	const runs = 5
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		insertClaudeMidConversationSystemBlocks(body, blocks, false)
	}
	runtime.ReadMemStats(&after)
	if allocated := int64(after.TotalAlloc-before.TotalAlloc) / runs; allocated > int64(len(body))*3/2 {
		t.Fatalf("insert allocates %d bytes per call on a %d byte body", allocated, len(body))
	}
}
