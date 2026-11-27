package registry

import (
	"reflect"
	"testing"
)

func TestModelCapabilitiesOpenAIReasoning(t *testing.T) {
	cases := []struct {
		name  string
		model *ModelInfo
		want  map[string]any
	}{
		{name: "nil model", model: nil, want: nil},
		{
			name:  "user-defined without thinking config is unknown",
			model: &ModelInfo{ID: "m", UserDefined: true},
			want:  nil,
		},
		{
			name: "fallback default thinking is unknown",
			model: &ModelInfo{
				ID:              "m",
				DefaultThinking: true,
				Thinking:        &ThinkingSupport{Levels: []string{"low", "medium", "high"}},
			},
			want: nil,
		},
		{
			name:  "catalog model without thinking does not reason",
			model: &ModelInfo{ID: "m"},
			want:  map[string]any{"supported": false},
		},
		{
			name: "explicitly configured empty thinking does not reason",
			model: &ModelInfo{
				ID:               "m",
				UserDefined:      true,
				ExplicitThinking: true,
				Thinking:         &ThinkingSupport{},
			},
			want: map[string]any{"supported": false},
		},
		{
			name: "levels keep wire values",
			model: &ModelInfo{
				ID:       "m",
				Thinking: &ThinkingSupport{Levels: []string{"none", " Low ", "medium", "high", "xhigh", "ultra", "low"}},
			},
			want: map[string]any{
				"supported": true,
				"levels":    []string{"none", "low", "medium", "high", "xhigh", "ultra"},
			},
		},
		{
			name: "only none level does not reason",
			model: &ModelInfo{
				ID:       "m",
				Thinking: &ThinkingSupport{Levels: []string{"none"}},
			},
			want: map[string]any{"supported": false, "levels": []string{"none"}},
		},
		{
			name: "budget only",
			model: &ModelInfo{
				ID:       "m",
				Thinking: &ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true},
			},
			want: map[string]any{
				"supported": true,
				"budget": map[string]any{
					"min":             1024,
					"max":             128000,
					"zero_allowed":    true,
					"dynamic_allowed": false,
				},
			},
		},
		{
			name: "hybrid",
			model: &ModelInfo{
				ID:       "m",
				Thinking: &ThinkingSupport{Min: 128, Max: 32768, DynamicAllowed: true, Levels: []string{"low", "high"}},
			},
			want: map[string]any{
				"supported": true,
				"levels":    []string{"low", "high"},
				"budget": map[string]any{
					"min":             128,
					"max":             32768,
					"zero_allowed":    false,
					"dynamic_allowed": true,
				},
			},
		},
		{
			name: "user-defined with explicit levels",
			model: &ModelInfo{
				ID:               "m",
				UserDefined:      true,
				ExplicitThinking: true,
				Thinking:         &ThinkingSupport{Levels: []string{"low", "high"}},
			},
			want: map[string]any{"supported": true, "levels": []string{"low", "high"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := openAIModelReasoning(tc.model)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("reasoning = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestModelCapabilitiesOpenAIModelMapIncludesReasoning(t *testing.T) {
	r := newTestModelRegistry()
	declared := r.convertModelToMap(&ModelInfo{
		ID:       "reasoner",
		OwnedBy:  "test",
		Thinking: &ThinkingSupport{Levels: []string{"low", "high"}},
	}, "openai")
	reasoning, ok := declared["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning missing: %#v", declared)
	}
	if got := reasoning["supported"]; got != true {
		t.Fatalf("supported = %v, want true", got)
	}

	unknown := r.convertModelToMap(&ModelInfo{ID: "unknown", OwnedBy: "test", UserDefined: true}, "openai")
	if _, exists := unknown["reasoning"]; exists {
		t.Fatalf("unknown model must not advertise reasoning: %#v", unknown)
	}

	claude := r.convertModelToMap(&ModelInfo{
		ID:       "reasoner",
		Thinking: &ThinkingSupport{Levels: []string{"low", "high"}},
	}, "claude")
	if _, exists := claude["reasoning"]; exists {
		t.Fatalf("claude listing must not change: %#v", claude)
	}
}
