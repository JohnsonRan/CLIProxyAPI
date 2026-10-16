// Package commandcode implements the Command Code (commandcode.ai) browser login.
//
// The flow mirrors `cmd login` from the official CLI: the browser opens the
// Studio CLI authorization page, and after the user authorizes, Studio
// redirects to a loopback callback with a long-lived API key in the query
// (apiKey, state, userId, userName, keyName). There is no refresh token; the
// key stays valid until it is revoked in Studio.
package commandcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/tidwall/gjson"
)

const (
	// ProviderKey is the credential type and provider identifier.
	ProviderKey = "commandcode"
	// StudioBaseURL hosts the browser authorization page.
	StudioBaseURL = "https://commandcode.ai"
	// APIBaseURL hosts the account endpoints (whoami, billing).
	APIBaseURL = "https://api.commandcode.ai"
	// ProviderBaseURL is the OpenAI-compatible Provider API base URL.
	ProviderBaseURL = APIBaseURL + "/provider/v1"
	// CallbackPort is the loopback port registered as the Studio callback.
	CallbackPort = 54546
	// CallbackPath is the path Studio redirects to on the loopback port.
	CallbackPath = "/callback"

	whoamiPath     = "/alpha/whoami"
	requestTimeout = 30 * time.Second
)

// BuildAuthURL returns the Studio CLI authorization URL for the given loopback port and state.
func BuildAuthURL(port int, state string) string {
	callback := fmt.Sprintf("http://127.0.0.1:%d%s", port, CallbackPath)
	return fmt.Sprintf("%s/studio/auth/cli?callback=%s&state=%s&mode=redirect",
		StudioBaseURL, url.QueryEscape(callback), url.QueryEscape(state))
}

// Account is the identity returned by GET /alpha/whoami.
type Account struct {
	UserID   string
	UserName string
	Email    string
	OrgID    string
}

// Label returns a human-readable account label.
func (a *Account) Label() string {
	if a == nil {
		return ""
	}
	for _, v := range []string{a.Email, a.UserName, a.UserID} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// ErrInvalidAPIKey is returned when Command Code rejects the API key.
var ErrInvalidAPIKey = errors.New("command code rejected the API key")

// NewHTTPClient builds a proxy-aware client for login requests.
func NewHTTPClient(cfg *config.Config) *http.Client {
	client := &http.Client{Timeout: requestTimeout}
	if cfg == nil {
		return client
	}
	sdkCfg := cfg.SDKConfig
	return util.SetProxy(&sdkCfg, client)
}

// FetchAccount validates apiKey against GET /alpha/whoami and returns the account identity.
func FetchAccount(ctx context.Context, client *http.Client, apiKey string) (*Account, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, ErrInvalidAPIKey
	}
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, APIBaseURL+whoamiPath, nil)
	if errReq != nil {
		return nil, fmt.Errorf("command code whoami: build request: %w", errReq)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("command code whoami: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, fmt.Errorf("command code whoami: read body: %w", errRead)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrInvalidAPIKey
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("command code whoami: unexpected status %d", resp.StatusCode)
	}
	return parseAccount(body), nil
}

func parseAccount(body []byte) *Account {
	root := gjson.ParseBytes(body)
	user := root.Get("user")
	userName := strings.TrimSpace(user.Get("userName").String())
	if userName == "" {
		userName = strings.TrimSpace(user.Get("name").String())
	}
	return &Account{
		UserID:   strings.TrimSpace(user.Get("id").String()),
		UserName: userName,
		Email:    strings.TrimSpace(user.Get("email").String()),
		OrgID:    strings.TrimSpace(root.Get("org.id").String()),
	}
}

// CredentialFileName returns the auth file name for an account.
func CredentialFileName(account *Account) string {
	if account != nil {
		for _, v := range []string{account.Email, account.UserName, account.UserID} {
			if seg := sanitizeFileSegment(v); seg != "" {
				return fmt.Sprintf("%s-%s.json", ProviderKey, seg)
			}
		}
	}
	return fmt.Sprintf("%s-%d.json", ProviderKey, time.Now().UnixMilli())
}

func sanitizeFileSegment(value string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '@' || r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
