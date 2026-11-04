package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	claudemodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/claude/models"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// keyScope returns the providers the client API key of the request may use,
// from its client.key-scopes entry. scoped is false for a key that no entry
// applies to, which is unrestricted. Key scopes do not apply in Home mode,
// where Home selects the credential for a model this proxy cannot map to a
// provider.
func (h *BaseAPIHandler) keyScope(c *gin.Context) (allowed map[string]struct{}, scoped bool) {
	if h == nil || h.Cfg == nil || c == nil || h.AuthManager.HomeEnabled() {
		return nil, false
	}
	scope, ok := h.Cfg.Client.KeyScope(c.GetString("userApiKey"))
	if !ok {
		return nil, false
	}
	allowed = make(map[string]struct{}, 2*len(scope.Providers))
	for _, provider := range scope.Providers {
		for _, key := range internalconfig.KeyScopeProviderKeys(provider) {
			allowed[key] = struct{}{}
		}
	}
	return allowed, true
}

// KeyAllowsProvider reports whether the request's client API key may use the
// provider. Routes that serve a single provider without resolving a model call
// it; an unrestricted key may use every provider.
func (h *BaseAPIHandler) KeyAllowsProvider(c *gin.Context, provider string) bool {
	allowed, scoped := h.keyScope(c)
	if !scoped {
		return true
	}
	_, ok := allowed[internalconfig.NormalizeKeyScopeProvider(provider)]
	return ok
}

// scopedProvidersForExecution resolves the providers for a request, like
// providersForExecution, and narrows them to the ones the request's client API
// key may use. Every execution path calls it, so all of them apply the same rule.
func (h *BaseAPIHandler) scopedProvidersForExecution(ctx context.Context, modelName, originalRequestedModel string, allowImageModel bool, routeDecision modelRouteDecision, execOptions modelExecutionOptions) ([]string, string, *interfaces.ErrorMessage) {
	providers, normalizedModel, errMsg := h.providersForExecution(modelName, originalRequestedModel, allowImageModel, routeDecision, execOptions)
	if errMsg != nil {
		return providers, normalizedModel, errMsg
	}
	providers, errMsg = h.scopeProviders(ctx, originalRequestedModel, providers)
	if errMsg != nil {
		return nil, "", errMsg
	}
	return providers, normalizedModel, nil
}

// scopePluginExecutor applies the request's key scope to a request that a
// model router hands to an executor plugin. The plugin picks its own
// credentials, so a scoped key may use it only when the scope names the plugin
// by its ID, as it names a provider.
func (h *BaseAPIHandler) scopePluginExecutor(ctx context.Context, modelName, pluginID string) *interfaces.ErrorMessage {
	_, errMsg := h.scopeProviders(ctx, modelName, []string{pluginID})
	return errMsg
}

// scopeProviders keeps the providers the request's client API key may use. A
// model that only other providers serve is reported as not found, the answer
// an unknown model gets.
func (h *BaseAPIHandler) scopeProviders(ctx context.Context, modelName string, providers []string) ([]string, *interfaces.ErrorMessage) {
	if len(providers) == 0 || ctx == nil {
		return providers, nil
	}
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	allowed, scoped := h.keyScope(ginCtx)
	if !scoped {
		return providers, nil
	}
	kept := make([]string, 0, len(providers))
	for _, provider := range providers {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(provider))]; ok {
			kept = append(kept, provider)
		}
	}
	if len(kept) > 0 {
		return kept, nil
	}
	doc := ojson.NewDocString(`{"error":{"message":"","type":"invalid_request_error","code":"model_not_found","param":"model"}}`)
	if errSet := doc.Set("error.message", "model "+modelName+" is not available to this API key"); errSet != nil {
		return nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      errors.New(`{"error":{"message":"model is not available to this API key","type":"invalid_request_error","code":"model_not_found","param":"model"}}`),
		}
	}
	return nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errors.New(doc.String())}
}

// keyModelFilter returns a predicate that reports whether the request's client
// API key may use a model: whether a provider in its scope serves the model.
// scoped is false, and the predicate nil, for an unrestricted key.
func (h *BaseAPIHandler) keyModelFilter(c *gin.Context) (allows func(model string) bool, scoped bool) {
	allowed, scoped := h.keyScope(c)
	if !scoped {
		return nil, false
	}
	modelRegistry := registry.GetGlobalRegistry()
	return func(model string) bool {
		for _, provider := range modelRegistry.GetModelProviders(model) {
			if _, ok := allowed[strings.ToLower(provider)]; ok {
				return true
			}
		}
		return false
	}, true
}

// KeyAllowsModel reports whether the request's client API key may use the
// model. Handlers that answer with one model's details, outside the model
// lists, call it.
func (h *BaseAPIHandler) KeyAllowsModel(c *gin.Context, model string) bool {
	allows, scoped := h.keyModelFilter(c)
	return !scoped || allows(model)
}

// scopeModelList removes from a model-list response the models the request's
// client API key may not use. It understands every list shape the proxy
// serves: an array under "data" (OpenAI, Anthropic, Grok) or under "models"
// (Codex, Gemini), with the model named by id, slug, model or name.
func (h *BaseAPIHandler) scopeModelList(c *gin.Context, body []byte) []byte {
	allows, scoped := h.keyModelFilter(c)
	if !scoped || !ojson.ValidBytes(body) {
		return body
	}
	doc := ojson.NewDoc(body)
	modified := false
	for _, listKey := range []string{"data", "models"} {
		list := doc.Get(listKey)
		if !list.IsArray() {
			continue
		}
		kept := make([]string, 0, len(list.Array()))
		firstID, lastID := "", ""
		list.ForEach(func(_, entry ojson.Result) bool {
			if id := modelListEntryID(entry); id != "" && allows(id) {
				kept = append(kept, entry.Raw)
				if firstID == "" {
					firstID = entry.Get("id").String()
				}
				lastID = entry.Get("id").String()
			}
			return true
		})
		if errSet := doc.SetRaw(listKey, "["+strings.Join(kept, ",")+"]"); errSet != nil {
			return body
		}
		modified = true
		// The Anthropic list names its first and last entries.
		if doc.Get("first_id").Exists() {
			_ = doc.Set("first_id", firstID)
		}
		if doc.Get("last_id").Exists() {
			_ = doc.Set("last_id", lastID)
		}
	}
	if !modified {
		return body
	}
	return doc.Bytes()
}

// modelListEntryID returns the registry ID of the model a list entry names.
// Gemini lists prefix it with "models/", and the Anthropic list disguises
// models from other providers, which requests resolve back the same way.
func modelListEntryID(entry ojson.Result) string {
	for _, field := range []string{"id", "slug", "model", "name"} {
		if value := strings.TrimSpace(entry.Get(field).String()); value != "" {
			return claudemodels.ResolveClaudeModelIDPrefix(strings.TrimPrefix(value, "models/"))
		}
	}
	return ""
}
