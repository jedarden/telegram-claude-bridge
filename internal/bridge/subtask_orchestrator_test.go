// Package bridge provides unit tests for splitParallelPrompts utility function.
// These tests cover edge cases, delimiter handling, whitespace preservation,
// Unicode content, and boundary conditions.
package bridge

import (
	"strings"
	"testing"
)

// TestSplitParallelPrompts_EmptyInputs tests empty and whitespace-only inputs.
// Verifies fallback behavior: empty/whitespace inputs return 1 prompt (trimmed original) instead of 0.
func TestSplitParallelPrompts_EmptyInputs(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLen  int
		wantPrompts []string
	}{
		{
			name:     "empty string returns empty prompt",
			input:    "",
			wantLen:  1,
			wantPrompts: []string{""},
		},
		{
			name:     "single newline returns empty prompt",
			input:    "\n",
			wantLen:  1,
			wantPrompts: []string{""},
		},
		{
			name:     "multiple newlines only returns empty prompt",
			input:    "\n\n\n",
			wantLen:  1,
			wantPrompts: []string{""},
		},
		{
			name:     "spaces only returns empty prompt",
			input:    "     ",
			wantLen:  1,
			wantPrompts: []string{""},
		},
		{
			name:     "tabs and spaces returns empty prompt",
			input:    "\t\t  \t  ",
			wantLen:  1,
			wantPrompts: []string{""},
		},
		{
			name:     "mixed whitespace returns empty prompt",
			input:    "   \n  \t\n   ",
			wantLen:  1,
			wantPrompts: []string{""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
			if tt.wantPrompts != nil && len(prompts) > 0 {
				for i, want := range tt.wantPrompts {
					if prompts[i] != want {
						t.Errorf("prompt[%d] = %q, want %q", i, prompts[i], want)
					}
				}
			}
		})
	}
}

// TestSplitParallelPrompts_DelimiterAtEdges tests delimiters at start/end of input.
func TestSplitParallelPrompts_DelimiterAtEdges(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLen  int
		wantPrompts []string
	}{
		{
			name:     "delimiter at start only",
			input:    "\n---\nFirst prompt",
			wantLen:  1,
			wantPrompts: []string{"First prompt"},
		},
		{
			name:     "delimiter at end only",
			input:    "First prompt\n---\n",
			wantLen:  1,
			wantPrompts: []string{"First prompt"},
		},
		{
			name:     "delimiter at both edges",
			input:    "\n---\nFirst prompt\n---\n",
			wantLen:  1,
			wantPrompts: []string{"First prompt"},
		},
		{
			name:     "multiple delimiters at start",
			input:    "\n---\n\n---\nFirst prompt",
			wantLen:  1,
			wantPrompts: []string{"First prompt"},
		},
		{
			name:     "multiple delimiters at end",
			input:    "First prompt\n---\n\n---\n",
			wantLen:  1,
			wantPrompts: []string{"First prompt"},
		},
		{
			name:     "delimiter at start with whitespace",
			input:    "   \n---\n  First prompt",
			wantLen:  1,
			wantPrompts: []string{"First prompt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
			if tt.wantPrompts != nil && len(prompts) > 0 {
				for i, want := range tt.wantPrompts {
					if prompts[i] != want {
						t.Errorf("prompt[%d] = %q, want %q", i, prompts[i], want)
					}
				}
			}
		})
	}
}

// TestSplitParallelPrompts_ConsecutiveDelimiters tests multiple delimiters in sequence.
// The function splits on "\n---\n" exactly. Consecutive delimiters only split if they have
// newlines between them: "\n---\n---\n" splits once, "\n---\n---\n" does not split twice.
func TestSplitParallelPrompts_ConsecutiveDelimiters(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLen  int
		wantPrompts []string
	}{
		{
			name:     "two consecutive delimiters",
			input:    "First\n---\n---\nSecond",
			wantLen:  2,
			wantPrompts: []string{"First", "---\nSecond"},
		},
		{
			name:     "three consecutive delimiters",
			input:    "First\n---\n---\n---\nSecond",
			wantLen:  3,
			wantPrompts: []string{"First", "---", "Second"},
		},
		{
			name:     "four consecutive delimiters",
			input:    "A\n---\n---\n---\n---\nB",
			wantLen:  3,
			wantPrompts: []string{"A", "---", "---\nB"},
		},
		{
			name:     "consecutive delimiters with whitespace",
			input:    "First\n---\n  \n---\nSecond",
			wantLen:  2,
			wantPrompts: []string{"First", "Second"},
		},
		{
			name:     "consecutive delimiters with more whitespace",
			input:    "First\n---\n   \t   \n---\nSecond",
			wantLen:  2,
			wantPrompts: []string{"First", "Second"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
			if tt.wantPrompts != nil && len(prompts) > 0 {
				for i, want := range tt.wantPrompts {
					if i >= len(prompts) {
						t.Errorf("expected prompt[%d] but got only %d prompts", i, len(prompts))
						break
					}
					if prompts[i] != want {
						t.Errorf("prompt[%d] = %q, want %q", i, prompts[i], want)
					}
				}
			}
		})
	}
}

// TestSplitParallelPrompts_MixedDelimiterPatterns tests different whitespace patterns around delimiters.
// The function only splits on exact "\n---\n", not on variants with extra whitespace.
func TestSplitParallelPrompts_MixedDelimiterPatterns(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLen  int
		wantPrompts []string
	}{
		{
			name:     "spaces before delimiter",
			input:    "First  \n ---\nSecond",
			wantLen:  1,
			wantPrompts: []string{"First  \n ---\nSecond"},
		},
		{
			name:     "spaces after delimiter",
			input:    "First\n---  \nSecond",
			wantLen:  1,
			wantPrompts: []string{"First\n---  \nSecond"},
		},
		{
			name:     "tabs around delimiter",
			input:    "First\t\n---\t\nSecond",
			wantLen:  1,
			wantPrompts: []string{"First\t\n---\t\nSecond"},
		},
		{
			name:     "no newline just dashes",
			input:    "First---Second",
			wantLen:  1,
			wantPrompts: []string{"First---Second"},
		},
		{
			name:     "different whitespace each time",
			input:    "First\n ---\nSecond\n  ---\nThird\n\t---\nFourth",
			wantLen:  1,
			wantPrompts: []string{"First\n ---\nSecond\n  ---\nThird\n\t---\nFourth"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
			if tt.wantPrompts != nil && len(prompts) > 0 {
				for i, want := range tt.wantPrompts {
					if prompts[i] != want {
						t.Errorf("prompt[%d] = %q, want %q", i, prompts[i], want)
					}
				}
			}
		})
	}
}

// TestSplitParallelPrompts_UnicodeEdgeCases tests Unicode and emoji preservation edge cases.
func TestSplitParallelPrompts_UnicodeEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLen  int
		verify   func([]string) bool
	}{
		{
			name:    "emoji throughout",
			input:   "Check this 🔥\n---\nThat's great 👍\n---\nAmazing work 🎉",
			wantLen: 3,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "🔥") &&
					strings.Contains(prompts[1], "👍") &&
					strings.Contains(prompts[2], "🎉")
			},
		},
		{
			name:    "japanese text",
			input:   "最初のプロンプト\n---\n二番目のプロンプト\n---\n三番目のプロンプト",
			wantLen: 3,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "最初の") &&
					strings.Contains(prompts[1], "二番目の") &&
					strings.Contains(prompts[2], "三番目の")
			},
		},
		{
			name:    "chinese text",
			input:   "第一个提示\n---\n第二个提示\n---\n第三个提示",
			wantLen: 3,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "第一个") &&
					strings.Contains(prompts[1], "第二个") &&
					strings.Contains(prompts[2], "第三个")
			},
		},
		{
			name:    "arabic text (right-to-left)",
			input:   "الطلب الأول\n---\nالطلب الثاني\n---\nالطلب الثالث",
			wantLen: 3,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "الطلب") &&
					strings.Contains(prompts[1], "الطلب") &&
					strings.Contains(prompts[2], "الطلب")
			},
		},
		{
			name:    "mixed scripts and emoji",
			input:   "Hello 世界 🌍\n---\nBonjour monde 🇫🇷\n---\nHallo Welt 🇩🇪",
			wantLen: 3,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "世界") &&
					strings.Contains(prompts[1], "monde") &&
					strings.Contains(prompts[2], "Welt")
			},
		},
		{
			name:    "russian cyrillic",
			input:   "Первый запрос\n---\nВторой запрос\n---\nТретий запрос",
			wantLen: 3,
			verify: func(prompts []string) bool {
				return prompts[0] == "Первый запрос" &&
					prompts[1] == "Второй запрос" &&
					prompts[2] == "Третий запрос"
			},
		},
		{
			name:    "special unicode characters",
			input:   "Test ©®™\n---\nMath: ∑∫√∞\n---\nArrows: ←↑→↓↔",
			wantLen: 3,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "©") &&
					strings.Contains(prompts[1], "∑") &&
					strings.Contains(prompts[2], "←")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
			if tt.verify != nil && !tt.verify(prompts) {
				t.Errorf("verification failed for prompts: %v", prompts)
			}
		})
	}
}

// TestSplitParallelPrompts_LongInputs tests handling of very long prompts.
func TestSplitParallelPrompts_LongInputs(t *testing.T) {
	tests := []struct {
	 name       string
 input       string
 wantLen     int
 minPromptLen int
	}{
		{
			name:        "single long prompt (10000 chars)",
			input:       strings.Repeat("This is a very long prompt. ", 357), // ~10008 chars
			wantLen:     1,
			minPromptLen: 10000,
		},
		{
			name: "two long prompts (5000 chars each)",
			input: strings.Repeat("AAAA", 1250) + "\n---\n" + strings.Repeat("BBBB", 1250),
			wantLen: 2,
			minPromptLen: 5000,
		},
		{
			name: "five medium prompts",
			input: strings.Join([]string{
				strings.Repeat("A", 300),
				strings.Repeat("B", 300),
				strings.Repeat("C", 300),
				strings.Repeat("D", 300),
				strings.Repeat("E", 300),
			}, "\n---\n"),
			wantLen: 5,
			minPromptLen: 300,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
			for i, p := range prompts {
				if len(p) < tt.minPromptLen {
					t.Errorf("prompt[%d] length = %d, want >= %d", i, len(p), tt.minPromptLen)
				}
			}
		})
	}
}

// TestSplitParallelPrompts_WhitespacePreservation tests internal whitespace is preserved.
func TestSplitParallelPrompts_WhitespacePreservation(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLen  int
		verify   func([]string) bool
	}{
		{
			name:    "multi-line prompt preserved",
			input:   "Line 1\nLine 2\nLine 3\n---\nNext prompt",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return prompts[0] == "Line 1\nLine 2\nLine 3"
			},
		},
		{
			name:    "internal spaces preserved",
			input:   "Words with    multiple   spaces\n---\nAnother  prompt",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "multiple   spaces")
			},
		},
		{
			name:    "tabs preserved internally",
			input:   "Column1\tColumn2\tColumn3\n---\nNext",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "\t")
			},
		},
		{
			name:    "indentation preserved",
			input:   "  Indented line\n    More indented\n---\nNext",
			wantLen: 2,
			verify: func(prompts []string) bool {
				// Only outer whitespace trimmed, internal preserved
				return prompts[0] == "Indented line\n    More indented"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
			if tt.verify != nil && !tt.verify(prompts) {
				t.Errorf("verification failed, got: %q", prompts)
			}
		})
	}
}

// TestSplitParallelPrompts_SpecialCharsEdgeCases tests special characters in prompts edge cases.
func TestSplitParallelPrompts_SpecialCharsEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLen  int
		verify   func([]string) bool
	}{
		{
			name:    "code blocks with backticks",
			input:   "Run `npm install` then `npm test`\n---\nNext step",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "`npm install`")
			},
		},
		{
			name:    "shell variables",
			input:   "Check $PATH and ${HOME}\n---\nRun with $VAR",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "$PATH") && strings.Contains(prompts[1], "$VAR")
			},
		},
		{
			name:    "markdown formatting",
			input:   "Write **bold** and *italic* text\n---\nUse _underline_ too",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "**bold**") && strings.Contains(prompts[1], "_underline_")
			},
		},
		{
			name:    "JSON content",
			input:   "Parse {\"key\": \"value\", \"nested\": {\"x\": 1}}\n---\nNext",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "{\"key\": \"value\"}")
			},
		},
		{
			name:    "quotes and escapes",
			input:   "Say \"hello\" and 'world'\n---\nUse \\\"escaped\\\" quotes",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "\"hello\"") && strings.Contains(prompts[1], "\\\"escaped\\\"")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
			if tt.verify != nil && !tt.verify(prompts) {
				t.Errorf("verification failed, got: %q", prompts)
			}
		})
	}
}

// TestSplitParallelPrompts_MaxPrompts tests the maximum practical limit.
func TestSplitParallelPrompts_MaxPrompts(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLen  int
	}{
		{
			name:    "six prompts",
			input:   "1\n---\n2\n---\n3\n---\n4\n---\n5\n---\n6",
			wantLen: 6,
		},
		{
			name:    "ten prompts",
			input:   strings.Join([]string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}, "\n---\n"),
			wantLen: 10,
		},
		{
			name:    "twenty prompts",
			input:   strings.Repeat("Prompt\n---\n", 19) + "Final",
			wantLen: 20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
		})
	}
}

// TestSplitParallelPrompts_NonDelimiterDashes tests dash patterns that are NOT delimiters.
func TestSplitParallelPrompts_NonDelimiterDashes(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		wantContent string
	}{
		{
			name:        "single dash on line",
			input:       "Text-\nwith-\ndashes",
			wantLen:     1,
			wantContent: "Text-\nwith-\ndashes",
		},
		{
			name:        "double dash on line",
			input:       "Text--\nwith--\ndashes",
			wantLen:     1,
			wantContent: "Text--\nwith--\ndashes",
		},
		{
			name:        "four dashes",
			input:       "Text\n----\nMore",
			wantLen:     1,
			wantContent: "Text\n----\nMore",
		},
		{
			name:        "five dashes",
			input:       "Text\n-----\nMore",
			wantLen:     1,
			wantContent: "Text\n-----\nMore",
		},
		{
			name:        "dashes without surrounding newlines",
			input:       "Text---More",
			wantLen:     1,
			wantContent: "Text---More",
		},
		{
			name:        "dashes with only trailing newline",
			input:       "Text---\nNext",
			wantLen:     1,
			wantContent: "Text---\nNext",
		},
		{
			name:        "dashes with only leading newline",
			input:       "Text\n---Next",
			wantLen:     1,
			wantContent: "Text\n---Next",
		},
		{
			name:        "spaces around dashes prevent split",
			input:       "First \n --- \n Second",
			wantLen:     1,
			wantContent: "First \n --- \n Second",
		},
		{
			name:        "tab before dashes",
			input:       "First\n\t---\nSecond",
			wantLen:     1,
			wantContent: "First\n\t---\nSecond",
		},
		{
			name:        "tab after dashes",
			input:       "First\n---\t\nSecond",
			wantLen:     1,
			wantContent: "First\n---\t\nSecond",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
			if len(prompts) > 0 && prompts[0] != tt.wantContent {
				t.Errorf("got content %q, want %q", prompts[0], tt.wantContent)
			}
		})
	}
}

// TestSplitParallelPrompts_EmptySegments tests that empty segments are filtered out.
func TestSplitParallelPrompts_EmptySegments(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLen  int
	}{
		{
			name:    "empty segment between delimiters",
			input:   "First\n---\n\n---\nSecond",
			wantLen: 2,
		},
		{
			name:    "whitespace-only segment",
			input:   "First\n---\n   \t  \n---\nSecond",
			wantLen: 2,
		},
		{
			name:    "multiple empty segments",
			input:   "A\n---\n\n---\n\n---\nB",
			wantLen: 2,
		},
		{
			name:    "empty segments at start",
			input:   "\n---\nFirst\n---\nSecond",
			wantLen: 2,
		},
		{
			name:    "empty segments at end",
			input:   "First\n---\nSecond\n---\n",
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
		})
	}
}

// TestSplitParallelPrompts_DelimiterInContent tests delimiters that appear within
// multiline content (where they should NOT split).
func TestSplitParallelPrompts_DelimiterInContent(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLen  int
		verify   func([]string) bool
	}{
		{
			name:    "dashes within a code block",
			input:   "Here's a list:\n- item 1\n- item 2\n---\nNext prompt",
			wantLen: 1,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "- item 2") &&
					!strings.Contains(prompts[0], "Next prompt")
			},
		},
		{
			name:    "dashes in markdown list",
			input:   "Check:\n- First task\n- Second task\n- Third task\n---\nNext",
			wantLen: 1,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "- Third task")
			},
		},
		{
			name:    "three dashes mid-line",
			input:   "Use the command: ls -la ---help to see options",
			wantLen: 1,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "---help")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
			if tt.verify != nil && !tt.verify(prompts) {
				t.Errorf("verification failed for prompts: %v", prompts)
			}
		})
	}
}

// TestSplitParallelPrompts_UnicodeAtBoundaries tests Unicode/emoji characters
// adjacent to delimiters to ensure they're preserved correctly.
func TestSplitParallelPrompts_UnicodeAtBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLen  int
		verify   func([]string) bool
	}{
		{
			name:    "emoji immediately before delimiter",
			input:   "Test 🔥\n---\nNext task",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "🔥") && prompts[0] == "Test 🔥"
			},
		},
		{
			name:    "emoji immediately after delimiter",
			input:   "First\n---\n🚀 Next",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[1], "🚀") && prompts[1] == "🚀 Next"
			},
		},
		{
			name:    "CJK adjacent to delimiter",
			input:   "テスト\n---\n测试",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "テ") && strings.Contains(prompts[1], "测")
			},
		},
		{
			name:    "RTL script adjacent to delimiter",
			input:   "مرحبا\n---\nשלום",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "مرحبا") && strings.Contains(prompts[1], "שלום")
			},
		},
		{
			name:    "mixed emoji and scripts",
			input:   "Start 🌸\n---\n中間\n---\nEnd 🎌",
			wantLen: 3,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "🌸") &&
					strings.Contains(prompts[1], "中間") &&
					strings.Contains(prompts[2], "🎌")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
			if tt.verify != nil && !tt.verify(prompts) {
				t.Errorf("verification failed for prompts: %v", prompts)
			}
		})
	}
}

// TestSplitParallelPrompts_VeryLongSinglePrompt tests extremely long single prompts.
func TestSplitParallelPrompts_VeryLongSinglePrompt(t *testing.T) {
	tests := []struct {
		name         string
		minLength    int
	}{
		{
			name:      "1000 character prompt",
			minLength: 1000,
		},
		{
			name:      "10000 character prompt",
			minLength: 10000,
		},
		{
			name:      "100000 character prompt",
			minLength: 100000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Generate a long prompt without delimiters
			longText := strings.Repeat("This is a test prompt with content. ", tt.minLength/40)
			prompts := splitParallelPrompts(longText)
			if len(prompts) != 1 {
				t.Errorf("got %d prompts, want 1", len(prompts))
			}
			if len(prompts[0]) < tt.minLength {
				t.Errorf("got prompt length %d, want >= %d", len(prompts[0]), tt.minLength)
			}
		})
	}
}

// TestSplitParallelPrompts_DelimiterPositionEdgeCases tests exact delimiter position patterns.
// These tests cover the specific edge cases of delimiter at beginning/end without
// surrounding newlines, and input that is only the delimiter.
func TestSplitParallelPrompts_DelimiterPositionEdgeCases(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		wantPrompts []string
	}{
		{
			name:        "delimiter at beginning of input",
			input:       "---\ntext",
			wantLen:     1,
			wantPrompts: []string{"---\ntext"},
		},
		{
			name:        "delimiter at end of input",
			input:       "text\n---",
			wantLen:     1,
			wantPrompts: []string{"text\n---"},
		},
		{
			name:        "only the delimiter",
			input:       "---",
			wantLen:     1,
			wantPrompts: []string{"---"},
		},
		{
			name:        "delimiter at beginning with multiple lines",
			input:       "---\nfirst line\nsecond line",
			wantLen:     1,
			wantPrompts: []string{"---\nfirst line\nsecond line"},
		},
		{
			name:        "delimiter at end with multiple lines",
			input:       "first line\nsecond line\n---",
			wantLen:     1,
			wantPrompts: []string{"first line\nsecond line\n---"},
		},
		{
			name:        "delimiter at both edges without surrounding newlines",
			input:       "---\ntext\n---",
			wantLen:     1,
			wantPrompts: []string{"---\ntext\n---"},
		},
		{
			name:        "multiple delimiters at beginning without leading newline",
			input:       "---\n---\ntext",
			wantLen:     2,
			wantPrompts: []string{"---", "text"},
		},
		{
			name:        "multiple delimiters at end without trailing newline",
			input:       "text\n---\n---",
			wantLen:     2,
			wantPrompts: []string{"text", "---"},
		},
		{
			name:        "valid delimiter after delimiter at beginning",
			input:       "---\ntext\n---\nnext",
			wantLen:     2,
			wantPrompts: []string{"---\ntext", "next"},
		},
		{
			name:        "valid delimiter before delimiter at end",
			input:       "first\n---\nsecond\n---",
			wantLen:     2,
			wantPrompts: []string{"first", "second\n---"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
			if tt.wantPrompts != nil && len(prompts) > 0 {
				// Verify we got the expected prompts
				for i, want := range tt.wantPrompts {
					if i >= len(prompts) {
						t.Errorf("expected prompt[%d] = %q but got only %d prompts", i, want, len(prompts))
						break
					}
					if prompts[i] != want {
						t.Errorf("prompt[%d] = %q, want %q", i, prompts[i], want)
					}
				}
			}
		})
	}
}
