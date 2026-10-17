package commandcode

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/tidwall/gjson"
)

const (
	chatCompletionsEndpoint = "/chat/completions"
	systemOneEndpoint       = "/systemone"
	// decisionsOutputModality is the OpenRouter-style output modality of
	// TypeSafe System One decision models.
	decisionsOutputModality = "decisions"
)

// systemOneModels are TypeSafe System One decision models served on
// <baseURL>/systemone. They are appended to a non-empty catalog unless the
// catalog already lists them.
var systemOneModels = []struct{ id, name string }{
	{id: "typesafe/jev", name: "TypeSafe Jev"},
}

// FetchModels reads the public Provider API catalog (GET <baseURL>/models).
//
// Only models served on /chat/completions or /systemone are returned: the
// credential is executed through the OpenAI-compatible executor, and Command
// Code serves its Claude models on /messages only. Models are marked
// user-defined so that the request's reasoning settings pass through to the
// upstream, which validates them.
func FetchModels(ctx context.Context, client *http.Client, baseURL, apiKey string) ([]*registry.ModelInfo, error) {
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = ProviderBaseURL
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if errReq != nil {
		return nil, fmt.Errorf("command code models: build request: %w", errReq)
	}
	req.Header.Set("Accept", "application/json")
	if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("command code models: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if errRead != nil {
		return nil, fmt.Errorf("command code models: read body: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("command code models: unexpected status %d", resp.StatusCode)
	}
	return ParseModels(body, time.Now().Unix()), nil
}

// ParseModels converts a Provider API /models body into registry models and
// appends the System One models when a non-empty catalog does not list them.
// Models served only on /systemone declare the decisions output modality.
func ParseModels(body []byte, now int64) []*registry.ModelInfo {
	data := gjson.GetBytes(body, "data")
	if !data.IsArray() {
		return nil
	}
	models := make([]*registry.ModelInfo, 0, len(data.Array()))
	seen := make(map[string]struct{})
	listed := make(map[string]struct{})
	for _, item := range data.Array() {
		id := strings.TrimSpace(item.Get("id").String())
		if id == "" {
			continue
		}
		listed[id] = struct{}{}
		if _, dup := seen[id]; dup {
			continue
		}
		var outputModalities []string
		if endpoints := item.Get("supported_endpoints"); endpoints.IsArray() && !containsEndpoint(endpoints, chatCompletionsEndpoint) {
			if !containsEndpoint(endpoints, systemOneEndpoint) {
				continue
			}
			outputModalities = []string{decisionsOutputModality}
		}
		seen[id] = struct{}{}
		created := item.Get("created").Int()
		if created <= 0 {
			created = now
		}
		models = append(models, &registry.ModelInfo{
			ID:            id,
			Object:        "model",
			Created:       created,
			OwnedBy:       ProviderKey,
			Type:          ProviderKey,
			DisplayName:   strings.TrimSpace(item.Get("name").String()),
			ContextLength: int(item.Get("context_length").Int()),
			UserDefined:   true,

			SupportedOutputModalities: outputModalities,
		})
	}
	if len(models) == 0 {
		return models
	}
	for _, model := range systemOneModels {
		if _, exists := listed[model.id]; exists {
			continue
		}
		models = append(models, &registry.ModelInfo{
			ID:          model.id,
			Object:      "model",
			Created:     now,
			OwnedBy:     ProviderKey,
			Type:        ProviderKey,
			DisplayName: model.name,
			UserDefined: true,

			SupportedOutputModalities: []string{decisionsOutputModality},
		})
	}
	return models
}

func containsEndpoint(endpoints gjson.Result, want string) bool {
	for _, endpoint := range endpoints.Array() {
		if strings.EqualFold(strings.TrimSpace(endpoint.String()), want) {
			return true
		}
	}
	return false
}
