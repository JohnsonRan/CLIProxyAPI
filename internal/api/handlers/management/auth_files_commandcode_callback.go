package management

import (
	"encoding/json"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	commandcodeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/commandcode"
)

const (
	// commandCodeCallbackCompletePath is the landing page Studio's form POST is
	// redirected to, mirroring the official CLI.
	commandCodeCallbackCompletePath = "/callback/complete"
	commandCodeCallbackMaxBodyBytes = 10_000
)

var commandCodeCallbackOrigins = []string{
	"https://commandcode.ai",
	"https://staging.commandcode.ai",
	"http://localhost:3000",
}

// commandCodeCallbackHandler serves the Command Code loopback callback.
//
// Studio delivers the API key in a POST to http://127.0.0.1:<port>/callback,
// either as a browser form (application/x-www-form-urlencoded, answered with a
// 303 to /callback/complete) or as a CORS fetch with a JSON body. The address
// bar therefore shows no query, so the credential cannot be relayed with a
// redirect; it is written to the pending session's callback file instead.
// GET /callback with query parameters is accepted for older Studio versions.
func commandCodeCallbackHandler(authDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Access-Control-Allow-Origin", commandCodeAllowedOrigin(r.Header.Get("Origin")))
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			if strings.EqualFold(r.Header.Get("Access-Control-Request-Private-Network"), "true") {
				w.Header().Set("Access-Control-Allow-Private-Network", "true")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		switch r.URL.Path {
		case commandCodeCallbackCompletePath:
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", "GET")
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			writeCommandCodeCallbackPage(w, http.StatusOK, "Authentication successful!", "You can close this window and return to the management panel.")
			return
		case commandcodeauth.CallbackPath:
		default:
			http.NotFound(w, r)
			return
		}

		var values url.Values
		isJSON := false
		switch r.Method {
		case http.MethodGet:
			values = r.URL.Query()
		case http.MethodPost:
			mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			body, errRead := io.ReadAll(io.LimitReader(r.Body, commandCodeCallbackMaxBodyBytes+1))
			if errRead != nil {
				writeCommandCodeCallbackPage(w, http.StatusBadRequest, "Invalid request", "Failed to read the callback body.")
				return
			}
			if len(body) > commandCodeCallbackMaxBodyBytes {
				writeCommandCodeCallbackPage(w, http.StatusRequestEntityTooLarge, "Invalid request", "Callback body is too large.")
				return
			}
			switch mediaType {
			case "application/x-www-form-urlencoded":
				parsed, errParse := url.ParseQuery(string(body))
				if errParse != nil {
					writeCommandCodeCallbackPage(w, http.StatusBadRequest, "Invalid request", "Malformed callback form.")
					return
				}
				values = parsed
			case "application/json":
				isJSON = true
				var payload map[string]any
				if errJSON := json.Unmarshal(body, &payload); errJSON != nil {
					writeCommandCodeCallbackJSON(w, http.StatusBadRequest, "Invalid JSON")
					return
				}
				values = url.Values{}
				for key, value := range payload {
					if s, ok := value.(string); ok {
						values.Set(key, s)
					}
				}
			default:
				writeCommandCodeCallbackPage(w, http.StatusUnsupportedMediaType, "Invalid request", "Unsupported content type.")
				return
			}
		default:
			w.Header().Set("Allow", "GET, POST, OPTIONS")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		state := strings.TrimSpace(values.Get("state"))
		apiKey := strings.TrimSpace(values.Get("apiKey"))
		errMsg := strings.TrimSpace(firstNonEmpty(values.Get("error"), values.Get("error_description")))
		if state == "" || (apiKey == "" && errMsg == "") {
			if isJSON {
				writeCommandCodeCallbackJSON(w, http.StatusBadRequest, "Missing required fields")
				return
			}
			writeCommandCodeCallbackPage(w, http.StatusBadRequest, "Invalid request", "Required parameters are missing. Restart the login from the management panel.")
			return
		}
		if _, errWrite := WriteOAuthCallbackFileForPendingSession(authDir, commandcodeauth.ProviderKey, state, apiKey, errMsg); errWrite != nil {
			if isJSON {
				writeCommandCodeCallbackJSON(w, http.StatusForbidden, "Invalid or expired state token")
				return
			}
			writeCommandCodeCallbackPage(w, http.StatusForbidden, "Invalid state token", "This login attempt is unknown or has expired. Restart the login from the management panel.")
			return
		}

		switch {
		case isJSON:
			writeCommandCodeCallbackJSON(w, http.StatusOK, "")
		case r.Method == http.MethodPost:
			w.Header().Set("Location", commandCodeCallbackCompletePath+"?state="+url.QueryEscape(state))
			w.WriteHeader(http.StatusSeeOther)
		default:
			writeCommandCodeCallbackPage(w, http.StatusOK, "Authentication successful!", "You can close this window and return to the management panel.")
		}
	})
}

func commandCodeAllowedOrigin(origin string) string {
	for _, allowed := range commandCodeCallbackOrigins {
		if origin == allowed {
			return origin
		}
	}
	return commandCodeCallbackOrigins[0]
}

func writeCommandCodeCallbackJSON(w http.ResponseWriter, status int, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{"success": errMsg == ""}
	if errMsg != "" {
		body["error"] = errMsg
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeCommandCodeCallbackPage(w http.ResponseWriter, status int, heading, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Command Code</title></head>`+
		`<body style="font-family: system-ui; padding: 40px; text-align: center;"><h1>`+html.EscapeString(heading)+
		`</h1><p>`+html.EscapeString(message)+`</p></body></html>`)
}
