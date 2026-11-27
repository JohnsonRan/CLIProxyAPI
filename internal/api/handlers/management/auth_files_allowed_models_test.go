package management

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPatchAuthFileFieldsAllowedModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name   string
		fields string
		status int
		want   string
	}{
		{"set", `"allowed_models":[" Gpt-* ","gpt-*","o3"]`, http.StatusOK, "gpt-*,o3"},
		{"empty clears", `"allowed_models":[]`, http.StatusOK, ""},
		{"null clears", `"allowed_models":null`, http.StatusOK, ""},
		{"string rejected", `"allowed_models":"gpt-*"`, http.StatusBadRequest, "keep-*"},
		{"non-string rejected", `"allowed_models":["gpt-*",1]`, http.StatusBadRequest, "keep-*"},
		{"nested rejected", `"allowed_models.0":"gpt-*"`, http.StatusBadRequest, "keep-*"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, manager, record := newExcludedModelsPatchTestHandler(t, "oauth")
			record.Attributes["allowed_models"] = "keep-*"
			if _, err := manager.Update(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			h.postAuthPersistHook = func(context.Context, *coreauth.Auth) error { return nil }
			rec := patchExcludedModelsFields(h, tt.fields)
			if rec.Code != tt.status {
				t.Fatalf("status = %d: %s, want %d", rec.Code, rec.Body.String(), tt.status)
			}
			updated, _ := manager.GetByID(record.ID)
			got, exists := updated.Attributes["allowed_models"]
			if got != tt.want || (tt.want == "" && exists) {
				t.Fatalf("allowed_models = %q (present %v), want %q", got, exists, tt.want)
			}
			// Exclusions are independent of the allow-list.
			if updated.Attributes["excluded_models"] != "stale-*" {
				t.Fatalf("excluded_models changed: %q", updated.Attributes["excluded_models"])
			}
		})
	}
}
