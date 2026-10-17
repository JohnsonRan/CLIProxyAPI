package util

import "github.com/tidwall/sjson"

// SetJSONBytesIfChanged behaves like sjson.SetBytes but returns data itself
// when path already holds exactly the bytes sjson would write. sjson copies
// the whole document even when the value does not change, which on large
// request bodies is a full copy per redundant write. path must address a
// single value (no wildcards or queries).
func SetJSONBytesIfChanged(data []byte, path string, value any) ([]byte, error) {
	if existing := GetGJSONBytesNoCopy(data, path); existing.Exists() && existing.Index > 0 {
		const prefix, suffix = `{"v":`, `}`
		if encoded, errEncode := sjson.Set(`{"v":null}`, "v", value); errEncode == nil &&
			len(encoded) > len(prefix)+len(suffix) &&
			encoded[len(prefix):len(encoded)-len(suffix)] == existing.Raw {
			return data, nil
		}
	}
	return sjson.SetBytes(data, path, value)
}
