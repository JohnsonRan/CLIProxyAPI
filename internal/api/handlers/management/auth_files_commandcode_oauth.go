package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	commandcodeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/commandcode"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const commandCodeCallbackTimeout = 5 * time.Minute

// commandCodeFetchAccount is swapped in tests.
var commandCodeFetchAccount = commandcodeauth.FetchAccount

// RequestCommandCodeToken starts the Command Code browser login.
//
// Studio redirects to http://127.0.0.1:<CallbackPort>/callback with the API key
// in the query. For web UI requests a forwarder on that port relays the
// callback to /commandcode/callback; otherwise the user pastes the redirect URL
// into POST /oauth/callback. The key is validated with /alpha/whoami before it
// is saved.
func (h *Handler) RequestCommandCodeToken(c *gin.Context) {
	ctx := PopulateAuthContext(context.Background(), c)
	provider := commandcodeauth.ProviderKey

	state, errState := misc.GenerateRandomState()
	if errState != nil {
		log.Errorf("Failed to generate state parameter: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}
	authURL := commandcodeauth.BuildAuthURL(commandcodeauth.CallbackPort, state)

	RegisterOAuthSession(state, provider)

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/commandcode/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute command code callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(commandcodeauth.CallbackPort, provider, targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start command code callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(commandcodeauth.CallbackPort, forwarder)
		}
		apiKey, errWait := waitForCommandCodeCallback(h.cfg.AuthDir, state)
		if errWait != nil {
			if IsOAuthSessionPending(state, provider) {
				log.WithError(errWait).Error("command code login failed")
				SetOAuthSessionError(state, errWait.Error())
			}
			return
		}

		account, errAccount := commandCodeFetchAccount(ctx, commandcodeauth.NewHTTPClient(h.cfg), apiKey)
		if errAccount != nil {
			log.WithError(errAccount).Error("command code API key validation failed")
			if errors.Is(errAccount, commandcodeauth.ErrInvalidAPIKey) {
				SetOAuthSessionError(state, "Command Code rejected the API key")
			} else {
				SetOAuthSessionError(state, oauthSessionErrorWithCause("Failed to validate API key", errAccount))
			}
			return
		}

		if account == nil {
			account = &commandcodeauth.Account{}
		}
		record := buildCommandCodeAuthRecord(apiKey, account, time.Now())
		if errGuard := guardOAuthSessionPendingForSave(state, provider); errGuard != nil {
			return
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save Command Code credential: %v", errSave)
			SetOAuthSessionError(state, "Failed to save authentication tokens")
			return
		}
		CompleteOAuthSession(state)
		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
	}()

	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

// waitForCommandCodeCallback waits for the callback file and returns the API key.
func waitForCommandCodeCallback(authDir, state string) (string, error) {
	provider := commandcodeauth.ProviderKey
	waitFile := filepath.Join(authDir, fmt.Sprintf(".oauth-%s-%s.oauth", provider, state))
	deadline := time.Now().Add(commandCodeCallbackTimeout)
	for {
		if !IsOAuthSessionPending(state, provider) {
			return "", errOAuthSessionNotPending
		}
		if time.Now().After(deadline) {
			return "", errors.New("Timeout waiting for OAuth callback")
		}
		data, errRead := os.ReadFile(waitFile)
		if errRead == nil {
			_ = os.Remove(waitFile)
			var m map[string]string
			_ = json.Unmarshal(data, &m)
			if errStr := strings.TrimSpace(m["error"]); errStr != "" {
				if errStr == "access_denied" {
					return "", errors.New("Authorization was denied")
				}
				return "", fmt.Errorf("Authentication failed: %s", errStr)
			}
			if m["state"] != state {
				return "", errors.New("State code error")
			}
			apiKey := strings.TrimSpace(m["code"])
			if apiKey == "" {
				return "", errors.New("Callback did not include an API key")
			}
			return apiKey, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func buildCommandCodeAuthRecord(apiKey string, account *commandcodeauth.Account, now time.Time) *coreauth.Auth {
	fileName := commandcodeauth.CredentialFileName(account)
	label := account.Label()
	if label == "" {
		label = "Command Code"
	}
	metadata := map[string]any{
		"type":             commandcodeauth.ProviderKey,
		"api_key":          apiKey,
		"base_url":         commandcodeauth.ProviderBaseURL,
		"prefix":           commandcodeauth.DefaultModelPrefix,
		"auth_kind":        coreauth.AuthKindOAuth,
		"authenticated_at": now.UTC().Format(time.RFC3339),
	}
	for key, value := range map[string]string{
		"email":     account.Email,
		"user_id":   account.UserID,
		"user_name": account.UserName,
		"org_id":    account.OrgID,
	} {
		if value != "" {
			metadata[key] = value
		}
	}
	return &coreauth.Auth{
		ID:       fileName,
		Provider: commandcodeauth.ProviderKey,
		FileName: fileName,
		Label:    label,
		Prefix:   commandcodeauth.DefaultModelPrefix,
		Metadata: metadata,
		Attributes: map[string]string{
			coreauth.AttributeAuthKind: coreauth.AuthKindOAuth,
			"base_url":                 commandcodeauth.ProviderBaseURL,
		},
	}
}
