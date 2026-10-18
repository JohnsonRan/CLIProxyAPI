package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// clientUsageRelayProvider names an API-key upstream whose own
	// GET /v1/usage is relayed, typically another CLIProxyAPI.
	clientUsageRelayProvider = "relay"
	// clientUsageDepthHeader counts the proxies a relayed quota request has
	// passed through, so relay loops end.
	clientUsageDepthHeader = "X-CLIProxy-Usage-Depth"
	clientUsageMaxDepth    = 3
)

// clientUsageIsRelay reports whether auth is an API-key credential pointing
// at a custom base URL, the shape of one proxy configured on top of another.
func clientUsageIsRelay(auth *coreauth.Auth) bool {
	return auth != nil && auth.AuthKind() == coreauth.AuthKindAPIKey &&
		strings.TrimSpace(authAttribute(auth, "base_url")) != "" &&
		strings.TrimSpace(authAttribute(auth, coreauth.AttributeAPIKey)) != ""
}

// clientUsageRelayURL maps a provider base URL to the upstream's quota
// endpoint: OpenAI-style bases end in /v1, Claude-style bases do not.
func clientUsageRelayURL(baseURL string) (string, error) {
	parsed, errParse := url.Parse(strings.TrimSpace(baseURL))
	if errParse != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("invalid base URL")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(strings.ToLower(path), "/v1") {
		path = path[:len(path)-len("/v1")]
	}
	parsed.Path = path + "/v1/usage"
	parsed.RawQuery, parsed.Fragment = "", ""
	return parsed.String(), nil
}

func clientUsageRequestDepth(header string) int {
	depth, errParse := strconv.Atoi(strings.TrimSpace(header))
	if errParse != nil || depth < 0 {
		return 0
	}
	if depth > clientUsageMaxDepth {
		return clientUsageMaxDepth
	}
	return depth
}

// probeRelayClientUsage reads the upstream proxy's quota summary with this
// credential's API key. An upstream without the endpoint, or one that reports
// nothing, is an error: the caller keeps not knowing, as before.
func (h *Handler) probeRelayClientUsage(ctx context.Context, auth *coreauth.Auth, depth int) clientUsageProbeResult {
	result := clientUsageProbeResult{observedAt: time.Now().UTC()}
	if depth >= clientUsageMaxDepth {
		result.err = "relay depth limit reached"
		return result
	}
	target, errURL := clientUsageRelayURL(authAttribute(auth, "base_url"))
	if errURL != nil {
		result.err = errURL.Error()
		return result
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if errRequest != nil {
		result.err = "build quota request failed"
		return result
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(authAttribute(auth, coreauth.AttributeAPIKey)))
	req.Header.Set(clientUsageDepthHeader, strconv.Itoa(depth+1))

	client := &http.Client{Transport: h.apiCallTransport(auth, "")}
	resp, errDo := client.Do(req)
	if errDo != nil {
		log.WithFields(log.Fields{"auth_index": auth.Index}).WithError(errDo).Debug("client usage: relay quota request failed")
		result.err = "upstream quota request failed"
		return result
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("client usage: relay response body close error: %v", errClose)
		}
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		result.err = fmt.Sprintf("upstream quota endpoint returned %d", resp.StatusCode)
		return result
	}
	payload, errRead := io.ReadAll(io.LimitReader(resp.Body, clientUsageMaxResponseBytes+1))
	if errRead != nil || len(payload) > clientUsageMaxResponseBytes {
		result.err = "upstream quota response unreadable"
		return result
	}
	var upstream struct {
		Version     int                      `json:"version"`
		Credentials *[]clientUsageCredential `json:"credentials"`
	}
	if errDecode := json.Unmarshal(payload, &upstream); errDecode != nil || upstream.Credentials == nil || upstream.Version < clientUsageVersion {
		result.err = "upstream does not report quota"
		return result
	}
	result.upstream = *upstream.Credentials
	if result.upstream == nil {
		result.upstream = []clientUsageCredential{}
	}
	return result
}
