package bridge

import (
	"strings"
	"testing"
)

// ── modelTier ─────────────────────────────────────────────────────────────────

func TestModelTier(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		expected int
	}{
		{
			name:     "haiku returns tier 0",
			model:    "claude-haiku-4-5",
			expected: modelTierHaiku,
		},
		{
			name:     "sonnet returns tier 1",
			model:    "claude-sonnet-4-6",
			expected: modelTierSonnet,
		},
		{
			name:     "opus returns tier 2",
			model:    "claude-opus-4-6",
			expected: modelTierOpus,
		},
		{
			name:     "unknown model defaults to sonnet tier",
			model:    "unknown-model",
			expected: modelTierSonnet,
		},
		{
			name:     "empty string defaults to sonnet tier",
			model:    "",
			expected: modelTierSonnet,
		},
		{
			name:     "old haiku version defaults to sonnet tier",
			model:    "claude-haiku-4-4",
			expected: modelTierSonnet,
		},
		{
			name:     "old sonnet version still returns sonnet tier",
			model:    "claude-sonnet-4-5",
			expected: modelTierSonnet,
		},
		{
			name:     "old opus version defaults to sonnet tier",
			model:    "claude-opus-4-5",
			expected: modelTierSonnet,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := modelTier(tc.model)
			if result != tc.expected {
				t.Errorf("modelTier(%q) = %d, want %d", tc.model, result, tc.expected)
			}
		})
	}
}

// ── prependConversationHistory ─────────────────────────────────────────────────

func TestPrependConversationHistory(t *testing.T) {
	tests := []struct {
		name     string
		history  []*ConversationMessage
		prompt   string
		contains []string // substrings that should be in the result
	}{
		{
			name:     "empty history returns prompt unchanged",
			history:  []*ConversationMessage{},
			prompt:   "What is the capital of France?",
			contains: []string{"What is the capital of France?"},
		},
		{
			name: "nil history returns prompt unchanged",
			history:  nil,
			prompt:   "What is the capital of France?",
			contains: []string{"What is the capital of France?"},
		},
		{
			name: "single user message prepended",
			history: []*ConversationMessage{
				{Role: "user", Content: "Hello"},
			},
			prompt:   "How are you?",
			contains: []string{"[Conversation history", "User: Hello", "How are you?"},
		},
		{
			name: "single assistant message prepended",
			history: []*ConversationMessage{
				{Role: "assistant", Content: "Hi there!"},
			},
			prompt:   "Continue",
			contains: []string{"[Conversation history", "Assistant: Hi there!", "Continue"},
		},
		{
			name: "multiple messages prepended in order",
			history: []*ConversationMessage{
				{Role: "user", Content: "What is 2+2?"},
				{Role: "assistant", Content: "4"},
				{Role: "user", Content: "What is 3+3?"},
				{Role: "assistant", Content: "6"},
			},
			prompt: "And 4+4?",
			contains: []string{
				"User: What is 2+2?",
				"Assistant: 4",
				"User: What is 3+3?",
				"Assistant: 6",
				"And 4+4?",
			},
		},
		{
			name: "long message truncated at 800 chars with ellipsis",
			history: []*ConversationMessage{
				{Role: "user", Content: strings.Repeat("a", 1000)},
			},
			prompt:   "Continue",
			contains: []string{"…", "Continue"},
		},
		{
			name: "message exactly at limit not truncated",
			history: []*ConversationMessage{
				{Role: "user", Content: strings.Repeat("b", 800)},
			},
			prompt:   "Continue",
			contains: []string{strings.Repeat("b", 800), "Continue"},
		},
		{
			name: "separator between history and prompt",
			history: []*ConversationMessage{
				{Role: "user", Content: "Previous message"},
			},
			prompt:   "New prompt",
			contains: []string{"---\n\n", "New prompt"},
		},
		{
			name: "header included",
			history: []*ConversationMessage{
				{Role: "user", Content: "Test"},
			},
			prompt:   "Prompt",
			contains: []string{"[Conversation history — prior exchanges in this topic]"},
		},
		{
			name: "alternating user and assistant messages",
			history: []*ConversationMessage{
				{Role: "user", Content: "First user message"},
				{Role: "assistant", Content: "First assistant response"},
				{Role: "user", Content: "Second user message"},
				{Role: "assistant", Content: "Second assistant response"},
				{Role: "user", Content: "Third user message"},
			},
			prompt: "Final prompt",
			contains: []string{
				"User: First user message",
				"Assistant: First assistant response",
				"User: Second user message",
				"Assistant: Second assistant response",
				"User: Third user message",
				"Final prompt",
			},
		},
		{
			name: "empty content in history messages",
			history: []*ConversationMessage{
				{Role: "user", Content: ""},
				{Role: "assistant", Content: "Response"},
			},
			prompt:   "Next",
			contains: []string{"User: ", "Assistant: Response", "Next"},
		},
		{
			name: "special characters in content",
			history: []*ConversationMessage{
				{Role: "user", Content: "Hello <world> & goodbye"},
			},
			prompt:   "Continue",
			contains: []string{"Hello <world> & goodbye", "Continue"},
		},
		{
			name: "multiline content preserved",
			history: []*ConversationMessage{
				{Role: "user", Content: "Line 1\nLine 2\nLine 3"},
			},
			prompt:   "Response",
			contains: []string{"Line 1\nLine 2\nLine 3", "Response"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := prependConversationHistory(tc.history, tc.prompt)
			for _, expected := range tc.contains {
				if !strings.Contains(result, expected) {
					t.Errorf("prependConversationHistory() result does not contain expected substring\nExpected: %q\nGot:\n%s", expected, result)
				}
			}
		})
	}
}

// ── tierModel (helper for tier changes) ─────────────────────────────────────────

func TestTierModel(t *testing.T) {
	tests := []struct {
		name          string
		tier          int
		expectedModel string
	}{
		{
			name:          "haiku tier",
			tier:          modelTierHaiku,
			expectedModel: "claude-haiku-4-5",
		},
		{
			name:          "sonnet tier",
			tier:          modelTierSonnet,
			expectedModel: "claude-sonnet-4-6",
		},
		{
			name:          "opus tier",
			tier:          modelTierOpus,
			expectedModel: "claude-opus-4-6",
		},
		{
			name:          "invalid negative tier defaults to sonnet",
			tier:          -1,
			expectedModel: defaultSessionModel,
		},
		{
			name:          "invalid high tier defaults to sonnet",
			tier:          100,
			expectedModel: defaultSessionModel,
		},
		{
			name:          "zero tier defaults to sonnet",
			tier:          0,
			expectedModel: "claude-haiku-4-5",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tierModel(tc.tier)
			if got != tc.expectedModel {
				t.Errorf("tierModel(%d) = %q, want %q", tc.tier, got, tc.expectedModel)
			}
		})
	}
}

