package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func modelsPayload(ids ...string) string {
	body := `{"object":"list","data":[`
	for i, id := range ids {
		if i > 0 {
			body += ","
		}
		body += fmt.Sprintf(`{"id":%q,"object":"model"}`, id)
	}
	return body + `]}`
}

func modelIDs(models []*ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out
}

func TestFetchOpenAICompatibilityModelIDs(t *testing.T) {
	var gotAuth, gotPath, gotHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotHeader = r.Header.Get("X-Custom")
		w.Header().Set("Content-Type", "application/json")
		// Duplicate IDs must collapse to one entry.
		_, _ = w.Write([]byte(modelsPayload("model-a", "model-b", "model-a", "  ")))
	}))
	defer server.Close()

	compat := &config.OpenAICompatibility{
		Name:          "test",
		BaseURL:       server.URL + "/v1/",
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: ""}, {APIKey: "sk-test"}},
		Headers:       map[string]string{"X-Custom": "custom-value", "X-Dynamic": "$FromClient"},
	}

	ids, err := fetchOpenAICompatibilityModelIDs(context.Background(), compat, "")
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if len(ids) != 2 || ids[0] != "model-a" || ids[1] != "model-b" {
		t.Fatalf("ids = %v, want [model-a model-b]", ids)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("path = %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("authorization = %q, want the first non-empty api key", gotAuth)
	}
	if gotHeader != "custom-value" {
		t.Fatalf("X-Custom = %q, want custom-value", gotHeader)
	}
}

func TestFetchOpenAICompatibilityModelIDsErrors(t *testing.T) {
	t.Run("non-2xx", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()
		if _, err := fetchOpenAICompatibilityModelIDs(context.Background(), &config.OpenAICompatibility{BaseURL: server.URL}, ""); err == nil {
			t.Fatal("expected an error for HTTP 401")
		}
	})

	t.Run("empty list", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(modelsPayload()))
		}))
		defer server.Close()
		if _, err := fetchOpenAICompatibilityModelIDs(context.Background(), &config.OpenAICompatibility{BaseURL: server.URL}, ""); err == nil {
			t.Fatal("expected an error when the provider returns no models")
		}
	})

	t.Run("missing base-url", func(t *testing.T) {
		if _, err := fetchOpenAICompatibilityModelIDs(context.Background(), &config.OpenAICompatibility{}, ""); err == nil {
			t.Fatal("expected an error for an empty base-url")
		}
	})
}

func TestMergeDiscoveredCompatModelsKeepsConfigured(t *testing.T) {
	configured := []config.OpenAICompatibilityModel{{Name: "shared", Alias: "friendly-alias"}}
	merged := mergeDiscoveredCompatModels(configured, []string{"SHARED", "fresh", "fresh", ""})

	if len(merged) != 2 {
		t.Fatalf("merged = %#v, want 2 entries", merged)
	}
	if merged[0].Alias != "friendly-alias" {
		t.Fatalf("configured alias was overwritten: %#v", merged[0])
	}
	if merged[1].Name != "fresh" || merged[1].Alias != "fresh" {
		t.Fatalf("discovered entry = %#v, want fresh/fresh", merged[1])
	}
}

func TestCompatModelsWithDiscoveryDisabledDoesNotFetch(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(modelsPayload("discovered")))
	}))
	defer server.Close()

	service := &Service{}
	compat := &config.OpenAICompatibility{
		Name:    "test",
		BaseURL: server.URL,
		Models:  []config.OpenAICompatibilityModel{{Name: "configured", Alias: "configured"}},
	}

	got := modelIDs(service.compatModelsWithDiscovery(context.Background(), nil, compat))
	if len(got) != 1 || got[0] != "configured" {
		t.Fatalf("models = %v, want only the configured model", got)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("discovery ran %d times while disabled, want 0", hits)
	}
}

func TestCompatModelsWithDiscoveryAppliesExclusions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(modelsPayload("glm-4.5", "glm-4.6", "glm-5", "glm-5-turbo", "keep-me")))
	}))
	defer server.Close()

	service := &Service{}
	compat := &config.OpenAICompatibility{
		Name:               "test",
		BaseURL:            server.URL,
		AutoDiscoverModels: true,
		ExcludedModels:     []string{"glm-4.*", "glm-5-turbo"},
	}

	got := modelIDs(service.compatModelsWithDiscovery(context.Background(), nil, compat))
	want := []string{"glm-5", "keep-me"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("models = %v, want %v", got, want)
	}
}

// waitCompatDiscoveryRefresh installs a hook that reports finished background
// refreshes.
func waitCompatDiscoveryRefresh(t *testing.T) <-chan struct{} {
	t.Helper()
	done := make(chan struct{}, 4)
	compatDiscoveryAfterRefresh = func() { done <- struct{}{} }
	t.Cleanup(func() { compatDiscoveryAfterRefresh = nil })
	return done
}

func receiveRefresh(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("background discovery refresh did not finish")
	}
}

func TestDiscoverCompatModelIDsUsesCacheWithinTTL(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			_, _ = w.Write([]byte(modelsPayload("model-a")))
			return
		}
		_, _ = w.Write([]byte(modelsPayload("model-a", "model-b")))
	}))
	defer server.Close()

	base := time.Unix(1700000000, 0)
	original := compatDiscoveryNow
	compatDiscoveryNow = func() time.Time { return base }
	defer func() { compatDiscoveryNow = original }()
	done := waitCompatDiscoveryRefresh(t)

	service := &Service{}
	compat := &config.OpenAICompatibility{Name: "test", BaseURL: server.URL, AutoDiscoverModels: true}

	for i := 0; i < 3; i++ {
		if ids := service.discoverCompatModelIDs(context.Background(), nil, compat); len(ids) != 1 {
			t.Fatalf("call %d returned %v, want one model", i, ids)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("upstream hit %d times within the TTL, want 1", got)
	}

	// Past the TTL the cached list is returned at once and refreshed in the
	// background; the next call sees the refreshed list.
	compatDiscoveryNow = func() time.Time { return base.Add(compatDiscoveryTTL + time.Second) }
	if ids := service.discoverCompatModelIDs(context.Background(), nil, compat); len(ids) != 1 {
		t.Fatalf("stale call returned %v, want the cached model without waiting", ids)
	}
	receiveRefresh(t, done)
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("upstream hit %d times after the TTL expired, want 2", got)
	}
	if ids := service.discoverCompatModelIDs(context.Background(), nil, compat); len(ids) != 2 {
		t.Fatalf("after refresh returned %v, want two models", ids)
	}
}

func TestDiscoverCompatModelIDsStaleRefreshDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) > 1 {
			<-release
		}
		_, _ = w.Write([]byte(modelsPayload("model-a")))
	}))
	defer server.Close()
	defer close(release)

	base := time.Unix(1700000000, 0)
	original := compatDiscoveryNow
	compatDiscoveryNow = func() time.Time { return base }
	defer func() { compatDiscoveryNow = original }()
	done := waitCompatDiscoveryRefresh(t)

	service := &Service{}
	compat := &config.OpenAICompatibility{Name: "test", BaseURL: server.URL, AutoDiscoverModels: true}
	service.discoverCompatModelIDs(context.Background(), nil, compat)

	compatDiscoveryNow = func() time.Time { return base.Add(compatDiscoveryTTL + time.Second) }
	// The upstream now hangs; registration must still get the cached list,
	// and repeated calls must not start a second refresh.
	for i := 0; i < 3; i++ {
		if ids := service.discoverCompatModelIDs(context.Background(), nil, compat); len(ids) != 1 {
			t.Fatalf("call %d returned %v, want the cached model", i, ids)
		}
	}
	release <- struct{}{}
	receiveRefresh(t, done)
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("upstream hit %d times, want one background refresh", got)
	}
}

func TestDiscoverCompatModelIDsKeepsStaleResultOnFailure(t *testing.T) {
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(modelsPayload("model-a", "model-b")))
	}))
	defer server.Close()

	base := time.Unix(1700000000, 0)
	original := compatDiscoveryNow
	compatDiscoveryNow = func() time.Time { return base }
	defer func() { compatDiscoveryNow = original }()
	done := waitCompatDiscoveryRefresh(t)

	service := &Service{}
	compat := &config.OpenAICompatibility{Name: "test", BaseURL: server.URL, AutoDiscoverModels: true}

	if ids := service.discoverCompatModelIDs(context.Background(), nil, compat); len(ids) != 2 {
		t.Fatalf("warm-up returned %v, want two models", ids)
	}

	// A transient upstream failure after the TTL must not empty the model list.
	fail.Store(true)
	compatDiscoveryNow = func() time.Time { return base.Add(compatDiscoveryTTL + time.Second) }
	service.discoverCompatModelIDs(context.Background(), nil, compat)
	receiveRefresh(t, done)
	if ids := service.discoverCompatModelIDs(context.Background(), nil, compat); len(ids) != 2 {
		t.Fatalf("after a failed refresh returned %v, want the cached two models", ids)
	}
}

func TestCompatDiscoveryCacheKeyTracksRequestInputs(t *testing.T) {
	base := config.OpenAICompatibility{
		Name:          "Test",
		BaseURL:       "https://api.example.com/v1",
		Headers:       map[string]string{"X-A": "1"},
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "sk-1"}},
	}
	key := compatDiscoveryCacheKey(&base)
	if strings.Contains(key, "sk-1") {
		t.Fatalf("cache key %q leaks the API key", key)
	}
	rotated := base
	rotated.APIKeyEntries = []config.OpenAICompatibilityAPIKey{{APIKey: "sk-2"}}
	headers := base
	headers.Headers = map[string]string{"X-A": "2"}
	for name, changed := range map[string]config.OpenAICompatibility{"rotated key": rotated, "changed headers": headers} {
		if compatDiscoveryCacheKey(&changed) == key {
			t.Fatalf("%s kept the same cache key", name)
		}
	}
	if same := base; compatDiscoveryCacheKey(&same) != key {
		t.Fatal("identical inputs produced different cache keys")
	}
}

func TestDiscoverCompatModelIDsWithoutCacheReturnsNilOnFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	service := &Service{}
	compat := &config.OpenAICompatibility{Name: "test", BaseURL: server.URL, AutoDiscoverModels: true}
	if ids := service.discoverCompatModelIDs(context.Background(), nil, compat); ids != nil {
		t.Fatalf("ids = %v, want nil when no cached result exists", ids)
	}
}
