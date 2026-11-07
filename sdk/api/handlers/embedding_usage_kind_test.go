package handlers

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestEmbeddingExecutionTagsUsageRequestKind(t *testing.T) {
	const model = "embedding-kind-model"
	var kinds []string
	executor := &interceptorCaptureExecutor{
		execute: func(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
			kinds = append(kinds, coreusage.RequestKindFromContext(ctx))
			return coreexecutor.Response{Payload: []byte(`{"data":[]}`)}, nil
		},
	}
	handler := newInterceptorHandler(t, model, executor, &sdkconfig.SDKConfig{})
	body := []byte(`{"model":"` + model + `","input":"hi"}`)

	if _, _, errMsg := handler.ExecuteEmbeddingWithAuthManager(context.Background(), "openai-embedding", model, body, ""); errMsg != nil {
		t.Fatalf("embedding execution failed: %v", errMsg.Error)
	}
	if _, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", model, body, ""); errMsg != nil {
		t.Fatalf("chat execution failed: %v", errMsg.Error)
	}
	if len(kinds) != 2 || kinds[0] != coreusage.RequestKindEmbeddings || kinds[1] != "" {
		t.Fatalf("request kinds = %q, want [%q, \"\"]", kinds, coreusage.RequestKindEmbeddings)
	}
}
