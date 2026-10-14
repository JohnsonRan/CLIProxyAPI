package managementasset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newPanelReleaseServer(t *testing.T, html []byte, digest string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download/management.html" {
			_, _ = w.Write(html)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"assets": []map[string]any{{
				"name":                 managementAssetName,
				"browser_download_url": "http://" + r.Host + "/download/management.html",
				"digest":               digest,
			}},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestForceUpdateManagementHTMLInstallsThenReportsLatest(t *testing.T) {
	html := []byte("<html><body>panel v2</body></html>")
	server := newPanelReleaseServer(t, html, "sha256:"+sha256Hex(html))
	dir := t.TempDir()

	updated, status, err := forceUpdateManagementHTML(context.Background(), dir, server.Client(), server.URL+"/releases/latest")
	if err != nil || !updated || status != "updated" {
		t.Fatalf("first update = (%v, %q, %v), want (true, updated, nil)", updated, status, err)
	}
	got, errRead := os.ReadFile(filepath.Join(dir, managementAssetName))
	if errRead != nil || string(got) != string(html) {
		t.Fatalf("installed asset = %q, %v", got, errRead)
	}

	updated, status, err = forceUpdateManagementHTML(context.Background(), dir, server.Client(), server.URL+"/releases/latest")
	if err != nil || updated || status != "already_latest" {
		t.Fatalf("second update = (%v, %q, %v), want (false, already_latest, nil)", updated, status, err)
	}
}

func TestForceUpdateManagementHTMLRejectsDigestMismatch(t *testing.T) {
	html := []byte("<html><body>tampered</body></html>")
	server := newPanelReleaseServer(t, html, "sha256:"+sha256Hex([]byte("expected")))
	dir := t.TempDir()
	original := []byte("original")
	if errWrite := os.WriteFile(filepath.Join(dir, managementAssetName), original, 0o644); errWrite != nil {
		t.Fatal(errWrite)
	}

	updated, status, err := forceUpdateManagementHTML(context.Background(), dir, server.Client(), server.URL+"/releases/latest")
	if err == nil || updated || status != "digest_mismatch" {
		t.Fatalf("update = (%v, %q, %v), want (false, digest_mismatch, error)", updated, status, err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, managementAssetName))
	if string(got) != string(original) {
		t.Fatalf("asset was overwritten after a digest mismatch: %q", got)
	}
}
