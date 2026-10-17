package util

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestDeleteTopLevelJSONKeysMatchesSJSON(t *testing.T) {
	docs := []string{
		`{"temperature":1,"top_p":0.9,"top_k":5,"messages":[{"a":1}]}`,
		`{"messages":[],"temperature":0.5}`,
		`{"temperature":1}`,
		`{"top_p":1,"temperature":1}`,
		`{"a":{"temperature":1},"temperature":2,"b":"top_p"}`,
		`{"temperature":1,"temperature":2,"top_p":3}`,
		`{"te\u006dperature":1,"x":2}`,
		`{ "temperature" : 1 , "top_p" : 2 }`,
		"{\"temperature\":1,\"x\":2}\n",
		`{}`,
		`[1,2]`,
		`not json`,
		`{"x":1}`,
	}
	keySets := [][]string{{"temperature"}, {"top_p"}, {"temperature", "top_p"}, {"temperature", "top_p", "top_k"}, {"top_k", "temperature"}, {"missing"}}
	for _, doc := range docs {
		for _, keys := range keySets {
			want := deleteJSONKeysOneByOne([]byte(doc), keys)
			got := DeleteTopLevelJSONKeys([]byte(doc), keys...)
			if !bytes.Equal(got, want) {
				t.Errorf("%s minus %v:\n got  %q\n want %q", doc, keys, got, want)
			}
		}
	}
}

func TestDeleteTopLevelJSONKeysCopiesOnce(t *testing.T) {
	body := []byte(`{"model":"m","temperature":1,"top_p":0.9,"top_k":5,"messages":[` + strings.Repeat(`{"role":"user","content":"x"},`, 20000) + `{}]}`)
	unchanged := []byte(`{"model":"m","messages":[]}`)
	if got := DeleteTopLevelJSONKeys(unchanged, "temperature"); &got[0] != &unchanged[0] {
		t.Fatal("deleting absent keys copied the body")
	}
	const runs = 5
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		DeleteTopLevelJSONKeys(body, "temperature", "top_p", "top_k")
	}
	runtime.ReadMemStats(&after)
	// Three sequential sjson deletes copied the body three times.
	if allocated := int64(after.TotalAlloc-before.TotalAlloc) / runs; allocated > int64(len(body))*5/4 {
		t.Fatalf("delete allocates %d bytes per call on a %d byte body", allocated, len(body))
	}
}
