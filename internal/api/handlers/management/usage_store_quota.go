package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// usageStoreAntigravityQuotaURLs are the panel's Antigravity quota endpoints, tried in order.
var usageStoreAntigravityQuotaURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

// AntigravityQuotaReport fetches a credential's retrieveUserQuotaSummary body for the usage
// store's quota valuation (usagestore.SetQuotaProbe). Like the management APICall, each request
// is bounded by defaultAPICallTimeout so a stalled upstream cannot block the background prober.
func (h *Handler) AntigravityQuotaReport(ctx context.Context, auth *coreauth.Auth) ([]byte, error) {
	if auth == nil {
		return nil, fmt.Errorf("credential is nil")
	}
	token, errToken := h.resolveTokenForAuth(ctx, auth, "")
	if errToken != nil {
		return nil, fmt.Errorf("resolve token: %w", errToken)
	}
	if token == "" {
		return nil, fmt.Errorf("access token unavailable")
	}
	projectID := stringValue(auth.Metadata, "project_id")
	if projectID == "" {
		projectID = stringValue(auth.Metadata, "projectId")
	}
	if projectID == "" && auth.Attributes != nil {
		projectID = strings.TrimSpace(auth.Attributes["project_id"])
	}
	if projectID == "" {
		return nil, fmt.Errorf("credential project ID is unavailable")
	}
	body, errMarshal := json.Marshal(map[string]string{"project": projectID})
	if errMarshal != nil {
		return nil, fmt.Errorf("build quota request: %w", errMarshal)
	}
	client := &http.Client{Timeout: defaultAPICallTimeout, Transport: h.apiCallTransport(auth, "")}
	var lastErr error
	for _, targetURL := range usageStoreAntigravityQuotaURLs {
		req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
		if errReq != nil {
			return nil, errReq
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", misc.AntigravityUserAgent())
		resp, errDo := client.Do(req)
		if errDo != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = errDo
			continue
		}
		data, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		if errRead != nil {
			lastErr = errRead
			continue
		}
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			return data, nil
		}
		lastErr = fmt.Errorf("%s: status %d", targetURL, resp.StatusCode)
	}
	return nil, lastErr
}
