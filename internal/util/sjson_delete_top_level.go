package util

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// DeleteTopLevelJSONKeys removes the first member named by each key from the
// top-level object, like sjson.DeleteBytes called once per key, but copies
// data at most once instead of once per deleted key. For a compact object
// (members joined by "," with no whitespace, as translators and sjson write)
// sequential deletes leave exactly the kept members joined by ",", which is
// what this builds; any other input goes through sjson key by key. Keys must
// be plain member names (no path syntax).
func DeleteTopLevelJSONKeys(data []byte, keys ...string) []byte {
	if len(keys) == 0 || len(data) == 0 {
		return data
	}
	root := ParseGJSONBytesNoCopy(data)
	if !root.IsObject() {
		return deleteJSONKeysOneByOne(data, keys)
	}

	type member struct{ start, end int }
	var members []member
	deleted := make([]bool, 0, 16)
	pending := make(map[string]bool, len(keys))
	for _, key := range keys {
		pending[key] = true
	}
	compactLen, offset, anyDeleted := 1, 1, false
	root.ForEach(func(key, value gjson.Result) bool {
		if len(members) > 0 {
			compactLen++
			offset++
		}
		size := len(key.Raw) + 1 + len(value.Raw)
		members = append(members, member{start: offset, end: offset + size})
		drop := pending[key.Str]
		if drop {
			// Only the first member with that name, as sjson deletes it.
			delete(pending, key.Str)
			anyDeleted = true
		}
		deleted = append(deleted, drop)
		compactLen += size
		offset += size
		return true
	})
	if !anyDeleted {
		return data
	}
	if compactLen+1 != len(data) || len(root.Raw) != len(data) {
		return deleteJSONKeysOneByOne(data, keys)
	}
	for _, m := range members {
		// Verify each member sits where the compact layout puts it.
		if m.start <= 0 || m.end >= len(data) || (data[m.start-1] != ',' && data[m.start-1] != '{') {
			return deleteJSONKeysOneByOne(data, keys)
		}
	}

	out := make([]byte, 0, len(data))
	out = append(out, '{')
	first := true
	for i, m := range members {
		if deleted[i] {
			continue
		}
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, data[m.start:m.end]...)
	}
	return append(out, '}')
}

// deleteJSONKeysOneByOne is the sequential sjson delete the callers used to
// run, kept as the fallback and the test oracle.
func deleteJSONKeysOneByOne(data []byte, keys []string) []byte {
	for _, key := range keys {
		data, _ = sjson.DeleteBytes(data, key)
	}
	return data
}
