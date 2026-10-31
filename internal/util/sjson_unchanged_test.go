package util

import (
	"bytes"
	"testing"

	"github.com/tidwall/sjson"
)

func TestSetJSONBytesIfChangedMatchesSJSON(t *testing.T) {
	cases := []struct {
		body  string
		path  string
		value any
	}{
		{`{"thinking":{"type":"adaptive","display":"omitted"}}`, "thinking.display", "omitted"},
		{`{"thinking":{"type":"adaptive","display":"omitted"}}`, "thinking.display", "summarized"},
		{`{"thinking":{"type":"adaptive"}}`, "thinking.display", "omitted"},
		{`{"thinking":{"type":"adaptive","display":"omitt\u0065d"}}`, "thinking.display", "omitted"},
		{`{"thinking":{"type": "adaptive"}}`, "thinking.type", "adaptive"},
		{`{"thinking":{"budget_tokens":1024}}`, "thinking.budget_tokens", 1024},
		{`{"thinking":{"budget_tokens":1024.0}}`, "thinking.budget_tokens", 1024},
		{`{"a":true}`, "a", true},
		{`{"a":"<b>"}`, "a", "<b>"},
		{`{"a":null}`, "a", "x"},
		{`{}`, "a.b", "x"},
		{`not json`, "a", "x"},
	}
	for _, tc := range cases {
		want, wantErr := sjson.SetBytes([]byte(tc.body), tc.path, tc.value)
		got, gotErr := SetJSONBytesIfChanged([]byte(tc.body), tc.path, tc.value)
		if !bytes.Equal(got, want) || (gotErr == nil) != (wantErr == nil) {
			t.Errorf("%s %s=%v: got %q (%v), want %q (%v)", tc.body, tc.path, tc.value, got, gotErr, want, wantErr)
		}
	}
}

func TestSetJSONBytesIfChangedSkipsTheCopy(t *testing.T) {
	body := []byte(`{"messages":[` + string(bytes.Repeat([]byte(`{"role":"user","content":"x"},`), 1000)) + `{}],"thinking":{"type":"adaptive","display":"omitted"}}`)
	got, _ := SetJSONBytesIfChanged(body, "thinking.display", "omitted")
	if &got[0] != &body[0] {
		t.Fatal("an unchanged value still copied the body")
	}
}
