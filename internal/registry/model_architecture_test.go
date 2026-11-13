package registry

import (
	"reflect"
	"testing"
)

func TestModelCapabilitiesOpenAIArchitecture(t *testing.T) {
	cases := []struct {
		name  string
		model *ModelInfo
		want  map[string]any
	}{
		{name: "nil model", model: nil, want: nil},
		{name: "unknown modalities", model: &ModelInfo{ID: "m"}, want: nil},
		{
			name: "input and output",
			model: &ModelInfo{
				SupportedInputModalities:  []string{"TEXT", " image ", "text"},
				SupportedOutputModalities: []string{"text"},
			},
			want: map[string]any{
				"input_modalities":  []string{"text", "image"},
				"output_modalities": []string{"text"},
				"modality":          "text+image->text",
			},
		},
		{
			name:  "output only",
			model: &ModelInfo{SupportedOutputModalities: []string{"embeddings"}},
			want:  map[string]any{"output_modalities": []string{"embeddings"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := openAIModelArchitecture(tc.model)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("architecture = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestModelCapabilitiesIsNonChatOutput(t *testing.T) {
	cases := []struct {
		output []string
		want   bool
	}{
		{output: nil, want: false},
		{output: []string{"text"}, want: false},
		{output: []string{"image"}, want: false},
		{output: []string{"TEXT", "image"}, want: false},
		{output: []string{"embeddings"}, want: true},
		{output: []string{"rerank"}, want: true},
		{output: []string{"decisions"}, want: true},
		{output: []string{"Embeddings", "rerank"}, want: true},
		{output: []string{"audio"}, want: false},
		{output: []string{"embeddings", "text"}, want: false},
	}
	for _, tc := range cases {
		if got := IsNonChatOutput(tc.output); got != tc.want {
			t.Fatalf("IsNonChatOutput(%v) = %v, want %v", tc.output, got, tc.want)
		}
	}
}

func TestModelCapabilitiesOpenAIModelMapIncludesArchitecture(t *testing.T) {
	r := newTestModelRegistry()
	withArchitecture := r.convertModelToMap(&ModelInfo{
		ID:                        "vision",
		OwnedBy:                   "test",
		ContextLength:             1000,
		SupportedInputModalities:  []string{"text", "image"},
		SupportedOutputModalities: []string{"text"},
	}, "openai")
	architecture, ok := withArchitecture["architecture"].(map[string]any)
	if !ok {
		t.Fatalf("architecture missing: %#v", withArchitecture)
	}
	if got := architecture["modality"]; got != "text+image->text" {
		t.Fatalf("modality = %v, want text+image->text", got)
	}
	if got := withArchitecture["context_length"]; got != 1000 {
		t.Fatalf("context_length = %v, want 1000", got)
	}

	unknown := r.convertModelToMap(&ModelInfo{ID: "unknown", OwnedBy: "test"}, "openai")
	if _, exists := unknown["architecture"]; exists {
		t.Fatalf("unknown model must not advertise architecture: %#v", unknown)
	}
}

func TestModelCapabilitiesOpenAIModelMapKeepsTokenLimitsSeparate(t *testing.T) {
	r := newTestModelRegistry()
	model := r.convertModelToMap(&ModelInfo{
		ID:               "gemini-style",
		OwnedBy:          "test",
		InputTokenLimit:  1048576,
		OutputTokenLimit: 65536,
	}, "openai")
	if got := model["input_token_limit"]; got != 1048576 {
		t.Fatalf("input_token_limit = %v, want 1048576", got)
	}
	if got := model["output_token_limit"]; got != 65536 {
		t.Fatalf("output_token_limit = %v, want 65536", got)
	}
	// Codex catalog reads these keys; token limits must not leak into them.
	for _, key := range []string{"context_length", "max_completion_tokens"} {
		if _, exists := model[key]; exists {
			t.Fatalf("%s must stay undeclared: %#v", key, model)
		}
	}
}
