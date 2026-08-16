// Package bridge provides unit tests for splitParallelPrompts utility function.
// These tests cover edge cases, delimiter handling, whitespace preservation,
// Unicode content, and boundary conditions.
package bridge

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSplitParallelPrompts_EmptyInputs tests empty and whitespace-only inputs.
// Verifies fallback behavior: empty/whitespace inputs return 1 prompt (trimmed original) instead of 0.
func TestSplitParallelPrompts_EmptyInputs(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		wantPrompts []string
	}{
		{
			name:        "empty string returns empty prompt",
			input:       "",
			wantLen:     1,
			wantPrompts: []string{""},
		},
		{
			name:        "single newline returns empty prompt",
			input:       "\n",
			wantLen:     1,
			wantPrompts: []string{""},
		},
		{
			name:        "multiple newlines only returns empty prompt",
			input:       "\n\n\n",
			wantLen:     1,
			wantPrompts: []string{""},
		},
		{
			name:        "spaces only returns empty prompt",
			input:       "     ",
			wantLen:     1,
			wantPrompts: []string{""},
		},
		{
			name:        "tabs and spaces returns empty prompt",
			input:       "\t\t  \t  ",
			wantLen:     1,
			wantPrompts: []string{""},
		},
		{
			name:        "mixed whitespace returns empty prompt",
			input:       "   \n  \t\n   ",
			wantLen:     1,
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
		name        string
		input       string
		wantLen     int
		wantPrompts []string
	}{
		{
			name:        "delimiter at start only",
			input:       "\n---\nFirst prompt",
			wantLen:     1,
			wantPrompts: []string{"First prompt"},
		},
		{
			name:        "delimiter at end only",
			input:       "First prompt\n---\n",
			wantLen:     1,
			wantPrompts: []string{"First prompt"},
		},
		{
			name:        "delimiter at both edges",
			input:       "\n---\nFirst prompt\n---\n",
			wantLen:     1,
			wantPrompts: []string{"First prompt"},
		},
		{
			name:        "multiple delimiters at start",
			input:       "\n---\n\n---\nFirst prompt",
			wantLen:     1,
			wantPrompts: []string{"First prompt"},
		},
		{
			name:        "multiple delimiters at end",
			input:       "First prompt\n---\n\n---\n",
			wantLen:     1,
			wantPrompts: []string{"First prompt"},
		},
		{
			name:        "delimiter at start with whitespace",
			input:       "   \n---\n  First prompt",
			wantLen:     1,
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
		name        string
		input       string
		wantLen     int
		wantPrompts []string
	}{
		// Consecutive delimiters in the middle (with text before and after)
		{
			name:        "two consecutive delimiters",
			input:       "First\n---\n---\nSecond",
			wantLen:     2,
			wantPrompts: []string{"First", "---\nSecond"},
		},
		{
			name:        "three consecutive delimiters",
			input:       "First\n---\n---\n---\nSecond",
			wantLen:     3,
			wantPrompts: []string{"First", "---", "Second"},
		},
		{
			name:        "four consecutive delimiters",
			input:       "A\n---\n---\n---\n---\nB",
			wantLen:     3,
			wantPrompts: []string{"A", "---", "---\nB"},
		},
		// Consecutive delimiters at the start
		{
			name:        "two consecutive delimiters at start",
			input:       "\n---\n---\ntext",
			wantLen:     1,
			wantPrompts: []string{"text"},
		},
		{
			name:        "three consecutive delimiters at start",
			input:       "\n---\n---\n---\ntext",
			wantLen:     1,
			wantPrompts: []string{"text"},
		},
		{
			name:        "four consecutive delimiters at start",
			input:       "\n---\n---\n---\n---\ntext",
			wantLen:     1,
			wantPrompts: []string{"text"},
		},
		// Consecutive delimiters at the end
		{
			name:        "two consecutive delimiters at end",
			input:       "text\n---\n---\n",
			wantLen:     1,
			wantPrompts: []string{"text"},
		},
		{
			name:        "three consecutive delimiters at end",
			input:       "text\n---\n---\n---\n",
			wantLen:     1,
			wantPrompts: []string{"text"},
		},
		{
			name:        "four consecutive delimiters at end",
			input:       "text\n---\n---\n---\n---\n",
			wantLen:     1,
			wantPrompts: []string{"text"},
		},
		// Consecutive delimiters with whitespace between them
		{
			name:        "consecutive delimiters with whitespace",
			input:       "First\n---\n  \n---\nSecond",
			wantLen:     2,
			wantPrompts: []string{"First", "Second"},
		},
		{
			name:        "consecutive delimiters with more whitespace",
			input:       "First\n---\n   \t   \n---\nSecond",
			wantLen:     2,
			wantPrompts: []string{"First", "Second"},
		},
		{
			name:        "consecutive delimiters with whitespace at start",
			input:       "\n---\n  \n---\ntext",
			wantLen:     1,
			wantPrompts: []string{"text"},
		},
		// Consecutive delimiters throughout
		{
			name:        "consecutive delimiters throughout input",
			input:       "\n---\n---\n---\n",
			wantLen:     0,
			wantPrompts: nil,
		},
		// Mixed consecutive patterns
		{
			name:        "two delimiters, text, two more delimiters",
			input:       "\n---\n---\ntext\n---\n---\n",
			wantLen:     1,
			wantPrompts: []string{"text"},
		},
		{
			name:        "consecutive delimiters between valid splits",
			input:       "First\n---\n---\nSecond\n---\n---\nThird",
			wantLen:     3,
			wantPrompts: []string{"First", "Second", "Third"},
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
		name        string
		input       string
		wantLen     int
		wantPrompts []string
	}{
		{
			name:        "spaces before delimiter",
			input:       "First  \n ---\nSecond",
			wantLen:     1,
			wantPrompts: []string{"First  \n ---\nSecond"},
		},
		{
			name:        "spaces after delimiter",
			input:       "First\n---  \nSecond",
			wantLen:     1,
			wantPrompts: []string{"First\n---  \nSecond"},
		},
		{
			name:        "tabs around delimiter",
			input:       "First\t\n---\t\nSecond",
			wantLen:     1,
			wantPrompts: []string{"First\t\n---\t\nSecond"},
		},
		{
			name:        "no newline just dashes",
			input:       "First---Second",
			wantLen:     1,
			wantPrompts: []string{"First---Second"},
		},
		{
			name:        "different whitespace each time",
			input:       "First\n ---\nSecond\n  ---\nThird\n\t---\nFourth",
			wantLen:     1,
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
		name    string
		input   string
		wantLen int
		verify  func([]string) bool
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
		name         string
		input        string
		wantLen      int
		minPromptLen int
	}{
		{
			name:         "single long prompt (10000 chars)",
			input:        strings.Repeat("This is a very long prompt. ", 357), // ~10008 chars
			wantLen:      1,
			minPromptLen: 10000,
		},
		{
			name:         "two long prompts (5000 chars each)",
			input:        strings.Repeat("AAAA", 1250) + "\n---\n" + strings.Repeat("BBBB", 1250),
			wantLen:      2,
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
			wantLen:      5,
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
		name    string
		input   string
		wantLen int
		verify  func([]string) bool
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
		name    string
		input   string
		wantLen int
		verify  func([]string) bool
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
		name    string
		input   string
		wantLen int
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
		name    string
		input   string
		wantLen int
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
		name    string
		input   string
		wantLen int
		verify  func([]string) bool
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
		name    string
		input   string
		wantLen int
		verify  func([]string) bool
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
		name      string
		minLength int
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

// TestSplitParallelPrompts_WhitespaceAroundDelimiters tests whitespace preservation around delimiters.
// These tests verify that whitespace BEFORE/AFTER delimiters prevents splitting (only exact "\n---\n"
// is recognized), and that whitespace within segments is preserved (except outer trim).
func TestSplitParallelPrompts_WhitespaceAroundDelimiters(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		wantPrompts []string
		verify      func([]string) bool
	}{
		// Whitespace BEFORE delimiter (prevents split)
		{
			name:        "whitespace before delimiter - spaces",
			input:       "text\n  ---\nmore",
			wantLen:     1,
			wantPrompts: []string{"text\n  ---\nmore"},
		},
		{
			name:        "whitespace before delimiter - tabs",
			input:       "text\n\t---\nmore",
			wantLen:     1,
			wantPrompts: []string{"text\n\t---\nmore"},
		},
		{
			name:        "whitespace before delimiter - mixed",
			input:       "text\n \t ---\nmore",
			wantLen:     1,
			wantPrompts: []string{"text\n \t ---\nmore"},
		},
		{
			name:        "whitespace before delimiter - multiple lines with spaces",
			input:       "first line\n  \n---\nmore text",
			wantLen:     1,
			wantPrompts: []string{"first line\n  \n---\nmore text"},
		},
		// Whitespace AFTER delimiter (prevents split)
		{
			name:        "whitespace after delimiter - spaces",
			input:       "text\n---  \nmore",
			wantLen:     1,
			wantPrompts: []string{"text\n---  \nmore"},
		},
		{
			name:        "whitespace after delimiter - tabs",
			input:       "text\n---\t\nmore",
			wantLen:     1,
			wantPrompts: []string{"text\n---\t\nmore"},
		},
		{
			name:        "whitespace after delimiter - mixed",
			input:       "text\n--- \t \nmore",
			wantLen:     1,
			wantPrompts: []string{"text\n--- \t \nmore"},
		},
		{
			name:        "whitespace after delimiter - multiple lines with spaces",
			input:       "text\n---\n  \nmore text",
			wantLen:     1,
			wantPrompts: []string{"text\n---\n  \nmore text"},
		},
		// Whitespace AROUND delimiters (prevents split)
		{
			name:        "whitespace around delimiter - both sides",
			input:       "text  \n --- \n  more",
			wantLen:     1,
			wantPrompts: []string{"text  \n --- \n  more"},
		},
		{
			name:        "whitespace around delimiter - tabs",
			input:       "text\t\n\t---\t\n\tmore",
			wantLen:     1,
			wantPrompts: []string{"text\t\n\t---\t\n\tmore"},
		},
		{
			name:        "whitespace around delimiter - mixed space and tabs",
			input:       "text \t\n \t---\t \n\t more",
			wantLen:     1,
			wantPrompts: []string{"text \t\n \t---\t \n\t more"},
		},
		// Leading/trailing whitespace in segments (trimmed by splitParallelPrompts)
		{
			name:        "leading whitespace in segment is trimmed",
			input:       "  \n---\nnext",
			wantLen:     1,
			wantPrompts: []string{"next"},
		},
		{
			name:        "trailing whitespace in segment is trimmed",
			input:       "first\n---\n  ",
			wantLen:     1,
			wantPrompts: []string{"first"},
		},
		{
			name:        "whitespace around both segments is trimmed",
			input:       "  first  \n---\n  second  ",
			wantLen:     2,
			wantPrompts: []string{"first", "second"},
		},
		{
			name:        "tabs around segments are trimmed",
			input:       "\t\tfirst\t\t\n---\n\t\tsecond\t\t",
			wantLen:     2,
			wantPrompts: []string{"first", "second"},
		},
		// Internal whitespace is preserved
		{
			name:    "internal spaces in segment are preserved",
			input:   "words with    spaces\n---\nnext",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return prompts[0] == "words with    spaces" && prompts[1] == "next"
			},
		},
		{
			name:    "internal tabs in segment are preserved",
			input:   "col1\tcol2\tcol3\n---\nnext",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "\t") && prompts[0] == "col1\tcol2\tcol3"
			},
		},
		{
			name:    "internal newlines are preserved",
			input:   "line1\nline2\nline3\n---\nnext",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "\n") && prompts[0] == "line1\nline2\nline3"
			},
		},
		{
			name:    "internal indentation is preserved",
			input:   "  indented\n    more\n---\nnext",
			wantLen: 2,
			verify: func(prompts []string) bool {
				return prompts[0] == "  indented\n    more"
			},
		},
		// Mixed valid and invalid delimiters (whitespace prevents some splits)
		{
			name:        "valid delimiter then invalid (with whitespace)",
			input:       "first\n---\nsecond\n ---\nthird",
			wantLen:     2,
			wantPrompts: []string{"first", "second\n ---\nthird"},
		},
		{
			name:        "invalid delimiter (with whitespace) then valid",
			input:       "first\n ---\nsecond\n---\nthird",
			wantLen:     2,
			wantPrompts: []string{"first\n ---\nsecond", "third"},
		},
		{
			name:        "valid delimiters with whitespace between",
			input:       "first\n---\n  \n---\nsecond",
			wantLen:     2,
			wantPrompts: []string{"first", "second"},
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
						t.Errorf("expected prompt[%d] = %q but got only %d prompts", i, want, len(prompts))
						break
					}
					if prompts[i] != want {
						t.Errorf("prompt[%d] = %q, want %q", i, prompts[i], want)
					}
				}
			}
			if tt.verify != nil && !tt.verify(prompts) {
				t.Errorf("verification failed for prompts: %v", prompts)
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

// TestSplitParallelPrompts_UnicodeCharacterPreservation verifies that non-ASCII
// characters survive splitting byte-for-byte. Covers Latin Extended (accents,
// ligatures), Cyrillic and Greek (including polytonic and final sigma).
// Prompts are compared for exact equality, then checked for UTF-8 validity and
// absence of the replacement character, which is what corruption would look like.
func TestSplitParallelPrompts_UnicodeCharacterPreservation(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		wantPrompts []string
	}{
		// ── Latin Extended ────────────────────────────────────────────────────
		{
			name:        "latin extended-A accented letters",
			input:       "Přeložit dokument\n---\nSzöveg átírása\n---\nĞünlük rapor",
			wantLen:     3,
			wantPrompts: []string{"Přeložit dokument", "Szöveg átírása", "Ğünlük rapor"},
		},
		{
			name:        "latin extended ligatures and eszett",
			input:       "Cœur et æther\n---\nStraße in Weißenburg\n---\nĲsselmeer",
			wantLen:     3,
			wantPrompts: []string{"Cœur et æther", "Straße in Weißenburg", "Ĳsselmeer"},
		},
		{
			name:        "latin-1 supplement punctuation and symbols",
			input:       "¿Qué tal? ¡Hola!\n---\nPreço: 50€ ou £40\n---\nSección №5 · ½ kg",
			wantLen:     3,
			wantPrompts: []string{"¿Qué tal? ¡Hola!", "Preço: 50€ ou £40", "Sección №5 · ½ kg"},
		},
		// ── Cyrillic ──────────────────────────────────────────────────────────
		{
			name:        "cyrillic uppercase and lowercase",
			input:       "АБВГДЕЁЖЗИЙ\n---\nабвгдеёжзий\n---\nЪЫЬЭЮЯ ъыьэюя",
			wantLen:     3,
			wantPrompts: []string{"АБВГДЕЁЖЗИЙ", "абвгдеёжзий", "ЪЫЬЭЮЯ ъыьэюя"},
		},
		{
			name:        "cyrillic non-russian letters",
			input:       "Українська: їєґі\n---\nСрпски: ђћџњљ\n---\nБългарски: щъ",
			wantLen:     3,
			wantPrompts: []string{"Українська: їєґі", "Српски: ђћџњљ", "Български: щъ"},
		},
		// ── Greek ─────────────────────────────────────────────────────────────
		{
			name:        "greek alphabet uppercase and lowercase",
			input:       "ΑΒΓΔΕΖΗΘΙΚΛΜ\n---\nαβγδεζηθικλμ\n---\nΝΞΟΠΡΣΤΥΦΧΨΩ",
			wantLen:     3,
			wantPrompts: []string{"ΑΒΓΔΕΖΗΘΙΚΛΜ", "αβγδεζηθικλμ", "ΝΞΟΠΡΣΤΥΦΧΨΩ"},
		},
		{
			name:        "greek final sigma and accented vowels",
			input:       "Ὀδυσσεύς\n---\nΆλφα βήτα γάμμα\n---\nΤο κείμενος τέλος",
			wantLen:     3,
			wantPrompts: []string{"Ὀδυσσεύς", "Άλφα βήτα γάμμα", "Το κείμενος τέλος"},
		},
		{
			name:        "greek polytonic with breathing marks",
			input:       "Ἄνθρωπος ᾠδή\n---\nἙλλάς ῥόδον\n---\nαἰών ᾅδης",
			wantLen:     3,
			wantPrompts: []string{"Ἄνθρωπος ᾠδή", "Ἑλλάς ῥόδον", "αἰών ᾅδης"},
		},
		// ── Mixed scripts and normalization ───────────────────────────────────
		{
			name:        "all three scripts in one prompt each",
			input:       "Café Ångström\n---\nМосква Київ\n---\nΑθήνα Θεσσαλονίκη",
			wantLen:     3,
			wantPrompts: []string{"Café Ångström", "Москва Київ", "Αθήνα Θεσσαλονίκη"},
		},
		{
			name:        "scripts mixed within a single prompt",
			input:       "Ünïcödë + Кириллица + Ελληνικά\n---\nsecond",
			wantLen:     2,
			wantPrompts: []string{"Ünïcödë + Кириллица + Ελληνικά", "second"},
		},
		{
			name:        "decomposed combining marks are not normalized",
			input:       "café naïve\n---\nΆλφα",
			wantLen:     2,
			wantPrompts: []string{"café naïve", "Άλφα"},
		},
		{
			name:        "precomposed and decomposed forms stay distinct",
			input:       "é\n---\né",
			wantLen:     2,
			wantPrompts: []string{"é", "é"},
		},
		// ── Single prompt (no delimiter) preserves Unicode ─────────────────────
		{
			name:        "single unicode prompt without delimiter",
			input:       "Ελληνικά κείμενο με Ünïcödë και Кириллицей",
			wantLen:     1,
			wantPrompts: []string{"Ελληνικά κείμενο με Ünïcödë και Кириллицей"},
		},
		{
			name:        "unicode preserved across many segments",
			input:       "α\n---\nβ\n---\nγ\n---\nδ\n---\nε",
			wantLen:     5,
			wantPrompts: []string{"α", "β", "γ", "δ", "ε"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Fatalf("got %d prompts, want %d: %q", len(prompts), tt.wantLen, prompts)
			}
			for i, want := range tt.wantPrompts {
				if prompts[i] != want {
					t.Errorf("prompt[%d] = %q, want %q", i, prompts[i], want)
				}
				if got, wantCount := utf8.RuneCountInString(prompts[i]), utf8.RuneCountInString(want); got != wantCount {
					t.Errorf("prompt[%d] has %d runes, want %d", i, got, wantCount)
				}
			}
			for i, p := range prompts {
				if !utf8.ValidString(p) {
					t.Errorf("prompt[%d] = %q is not valid UTF-8", i, p)
				}
				if strings.ContainsRune(p, utf8.RuneError) {
					t.Errorf("prompt[%d] = %q contains the replacement character U+FFFD", i, p)
				}
			}
		})
	}

	// Splitting must not normalize: the precomposed and decomposed spellings of
	// the same grapheme have to come back as the distinct byte sequences they
	// went in as.
	t.Run("no unicode normalization is applied", func(t *testing.T) {
		prompts := splitParallelPrompts("é\n---\né")
		if len(prompts) != 2 {
			t.Fatalf("got %d prompts, want 2: %q", len(prompts), prompts)
		}
		if prompts[0] != "é" || utf8.RuneCountInString(prompts[0]) != 1 {
			t.Errorf("precomposed prompt = %q (%d runes), want %q (1 rune)", prompts[0], utf8.RuneCountInString(prompts[0]), "é")
		}
		if prompts[1] != "é" || utf8.RuneCountInString(prompts[1]) != 2 {
			t.Errorf("decomposed prompt = %q (%d runes), want %q (2 runes)", prompts[1], utf8.RuneCountInString(prompts[1]), "é")
		}
		if prompts[0] == prompts[1] {
			t.Errorf("precomposed and decomposed prompts were normalized to the same value %q", prompts[0])
		}
	})
}

// TestSplitParallelPrompts_UnicodeDelimiterBoundaries verifies behavior when
// non-ASCII characters sit directly against the "\n---\n" delimiter, and that
// Unicode dash lookalikes are never mistaken for the ASCII delimiter.
func TestSplitParallelPrompts_UnicodeDelimiterBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		wantPrompts []string
	}{
		{
			name:        "multibyte rune immediately before and after delimiter",
			input:       "Ω\n---\nα",
			wantLen:     2,
			wantPrompts: []string{"Ω", "α"},
		},
		{
			name:        "cyrillic touching both sides of delimiter",
			input:       "Первый\n---\nВторой\n---\nТретий",
			wantLen:     3,
			wantPrompts: []string{"Первый", "Второй", "Третий"},
		},
		{
			name:        "latin extended touching both sides of delimiter",
			input:       "Ærø\n---\nŽižkov\n---\nÑuñoa",
			wantLen:     3,
			wantPrompts: []string{"Ærø", "Žižkov", "Ñuñoa"},
		},
		{
			name:        "combining mark is last rune before delimiter",
			input:       "ά\n---\nβ",
			wantLen:     2,
			wantPrompts: []string{"ά", "β"},
		},
		{
			name:        "combining mark is first sequence after delimiter",
			input:       "first\n---\nöffnen",
			wantLen:     2,
			wantPrompts: []string{"first", "öffnen"},
		},
		{
			name:        "unicode non-breaking space around delimiter is trimmed",
			input:       "Ελληνικά \n---\n Кириллица",
			wantLen:     2,
			wantPrompts: []string{"Ελληνικά", "Кириллица"},
		},
		{
			name:        "ideographic space around delimiter is trimmed",
			input:       "Ünïcödë　\n---\n　Ελληνικά",
			wantLen:     2,
			wantPrompts: []string{"Ünïcödë", "Ελληνικά"},
		},
		{
			name:        "em dashes are not a delimiter",
			input:       "Πρώτο\n———\nΔεύτερο",
			wantLen:     1,
			wantPrompts: []string{"Πρώτο\n———\nΔεύτερο"},
		},
		{
			name:        "en dashes are not a delimiter",
			input:       "Ünïcödë\n–––\nтекст",
			wantLen:     1,
			wantPrompts: []string{"Ünïcödë\n–––\nтекст"},
		},
		{
			name:        "horizontal bar lookalike is not a delimiter",
			input:       "Άλφα\n―――\nΒήτα",
			wantLen:     1,
			wantPrompts: []string{"Άλφα\n―――\nΒήτα"},
		},
		{
			name:        "unicode content adjacent to leading delimiter",
			input:       "---\nΓάμμα\n---\nΔέλτα",
			wantLen:     2,
			wantPrompts: []string{"---\nΓάμμα", "Δέλτα"},
		},
		{
			name:        "unicode content adjacent to trailing delimiter",
			input:       "Γάμμα\n---\nΔέλτα\n---",
			wantLen:     2,
			wantPrompts: []string{"Γάμμα", "Δέλτα\n---"},
		},
		{
			name:        "unicode segment between consecutive delimiters",
			input:       "Ένα\n---\n\n---\nΔύο",
			wantLen:     2,
			wantPrompts: []string{"Ένα", "Δύο"},
		},
		{
			name:        "delimiter embedded in unicode line does not split",
			input:       "Κείμενο---κείμενο\n---\nтекст---текст",
			wantLen:     2,
			wantPrompts: []string{"Κείμενο---κείμενο", "текст---текст"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Fatalf("got %d prompts, want %d: %q", len(prompts), tt.wantLen, prompts)
			}
			for i, want := range tt.wantPrompts {
				if prompts[i] != want {
					t.Errorf("prompt[%d] = %q, want %q", i, prompts[i], want)
				}
			}
			for i, p := range prompts {
				if !utf8.ValidString(p) {
					t.Errorf("prompt[%d] = %q is not valid UTF-8", i, p)
				}
			}
		})
	}
}

// checkEmojiPrompts asserts that the split output matches wantPrompts exactly,
// byte for byte and rune for rune, and that nothing was mangled into invalid
// UTF-8 or the U+FFFD replacement character along the way.
func checkEmojiPrompts(t *testing.T, prompts, wantPrompts []string, wantLen int) {
	t.Helper()
	if len(prompts) != wantLen {
		t.Fatalf("got %d prompts, want %d: %q", len(prompts), wantLen, prompts)
	}
	for i, want := range wantPrompts {
		if prompts[i] != want {
			t.Errorf("prompt[%d] = %q, want %q", i, prompts[i], want)
		}
		if got := len(prompts[i]); got != len(want) {
			t.Errorf("prompt[%d] has %d bytes, want %d", i, got, len(want))
		}
		if got, wantCount := utf8.RuneCountInString(prompts[i]), utf8.RuneCountInString(want); got != wantCount {
			t.Errorf("prompt[%d] has %d runes, want %d", i, got, wantCount)
		}
	}
	for i, p := range prompts {
		if !utf8.ValidString(p) {
			t.Errorf("prompt[%d] = %q is not valid UTF-8", i, p)
		}
		if strings.ContainsRune(p, utf8.RuneError) {
			t.Errorf("prompt[%d] = %q contains the replacement character U+FFFD", i, p)
		}
	}
}

// TestSplitParallelPrompts_CommonEmojiPreservation verifies that everyday emoji
// — faces, hands, symbols, objects — survive splitting unchanged.
func TestSplitParallelPrompts_CommonEmojiPreservation(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		wantPrompts []string
	}{
		// ── Faces ─────────────────────────────────────────────────────────────
		{
			name:        "smiley faces one per prompt",
			input:       "Grinning 😀\n---\nJoy 😂\n---\nWink 😉",
			wantLen:     3,
			wantPrompts: []string{"Grinning 😀", "Joy 😂", "Wink 😉"},
		},
		{
			name:        "multiple faces within a single prompt",
			input:       "😀😃😄😁😆😅\n---\n🙂🙃😉😊😇",
			wantLen:     2,
			wantPrompts: []string{"😀😃😄😁😆😅", "🙂🙃😉😊😇"},
		},
		{
			name:        "cat faces and animal emoji",
			input:       "😺😸😹\n---\n🐶🐱🐭\n---\n🦊🐻🐼",
			wantLen:     3,
			wantPrompts: []string{"😺😸😹", "🐶🐱🐭", "🦊🐻🐼"},
		},
		// ── Symbols ───────────────────────────────────────────────────────────
		{
			name:        "symbol emoji across prompts",
			input:       "Check ✅\n---\nCross ❌\n---\nWarning ⚠️",
			wantLen:     3,
			wantPrompts: []string{"Check ✅", "Cross ❌", "Warning ⚠️"},
		},
		{
			name:        "arrows and math-adjacent symbol emoji",
			input:       "➡️⬅️⬆️⬇️\n---\n➕➖✖️➗\n---\n♻️🔱⚜️",
			wantLen:     3,
			wantPrompts: []string{"➡️⬅️⬆️⬇️", "➕➖✖️➗", "♻️🔱⚜️"},
		},
		{
			name:        "text-presentation symbols without variation selector",
			input:       "Sun ☀ and cloud ☁\n---\nPeace ☮ and yin yang ☯",
			wantLen:     2,
			wantPrompts: []string{"Sun ☀ and cloud ☁", "Peace ☮ and yin yang ☯"},
		},
		// ── Objects and activities ────────────────────────────────────────────
		{
			name:        "object emoji across prompts",
			input:       "Ship it 🚀\n---\nFix the bug 🐛\n---\nCelebrate 🎉",
			wantLen:     3,
			wantPrompts: []string{"Ship it 🚀", "Fix the bug 🐛", "Celebrate 🎉"},
		},
		{
			name:        "emoji-only prompts",
			input:       "🚀\n---\n🔥\n---\n🎯",
			wantLen:     3,
			wantPrompts: []string{"🚀", "🔥", "🎯"},
		},
		// ── Mixed with text and other scripts ─────────────────────────────────
		{
			name:        "emoji interleaved with ascii text",
			input:       "Run 🏃 the tests 🧪 now\n---\nShip 📦 the build 🛠️",
			wantLen:     2,
			wantPrompts: []string{"Run 🏃 the tests 🧪 now", "Ship 📦 the build 🛠️"},
		},
		{
			name:        "emoji mixed with non-latin scripts",
			input:       "日本語 🗾 テスト\n---\nΕλληνικά 🇬🇷 κείμενο\n---\nКириллица 📚 текст",
			wantLen:     3,
			wantPrompts: []string{"日本語 🗾 テスト", "Ελληνικά 🇬🇷 κείμενο", "Кириллица 📚 текст"},
		},
		{
			name:        "emoji inside multiline prompt bodies",
			input:       "Line one 🥇\nLine two 🥈\n---\nLine three 🥉\nLine four 🏅",
			wantLen:     2,
			wantPrompts: []string{"Line one 🥇\nLine two 🥈", "Line three 🥉\nLine four 🏅"},
		},
		// ── Single prompt / no delimiter ──────────────────────────────────────
		{
			name:        "single emoji prompt without delimiter",
			input:       "Just one prompt with emoji 🌈🦄✨",
			wantLen:     1,
			wantPrompts: []string{"Just one prompt with emoji 🌈🦄✨"},
		},
		{
			name:        "emoji preserved across many segments",
			input:       "1️⃣ one\n---\n2️⃣ two\n---\n3️⃣ three\n---\n4️⃣ four\n---\n5️⃣ five",
			wantLen:     5,
			wantPrompts: []string{"1️⃣ one", "2️⃣ two", "3️⃣ three", "4️⃣ four", "5️⃣ five"},
		},
		{
			name:        "whitespace around emoji prompts is trimmed",
			input:       "   🚀 launch   \n---\n\t🔥 burn\t",
			wantLen:     2,
			wantPrompts: []string{"🚀 launch", "🔥 burn"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkEmojiPrompts(t, splitParallelPrompts(tt.input), tt.wantPrompts, tt.wantLen)
		})
	}
}

// TestSplitParallelPrompts_MultiByteEmojiSequences verifies that composite emoji
// — ZWJ sequences, skin-tone modifiers, regional-indicator flags, keycaps,
// variation selectors and tag sequences — are never broken apart by splitting.
func TestSplitParallelPrompts_MultiByteEmojiSequences(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		wantPrompts []string
		wantRunes   []int // rune count per prompt, guards against dropped joiners
	}{
		// ── ZWJ sequences ─────────────────────────────────────────────────────
		{
			name:        "family zwj sequence stays intact",
			input:       "Family 👨‍👩‍👧‍👦\n---\nCouple 👩‍❤️‍👨",
			wantLen:     2,
			wantPrompts: []string{"Family 👨‍👩‍👧‍👦", "Couple 👩‍❤️‍👨"},
			wantRunes:   []int{14, 13},
		},
		{
			name:        "profession zwj sequences",
			input:       "👩‍💻 codes\n---\n👨‍🚀 flies\n---\n🧑‍🍳 cooks",
			wantLen:     3,
			wantPrompts: []string{"👩‍💻 codes", "👨‍🚀 flies", "🧑‍🍳 cooks"},
			wantRunes:   []int{9, 9, 9},
		},
		{
			name:        "handshake zwj sequence between delimiters",
			input:       "first\n---\n🧑‍🤝‍🧑\n---\nlast",
			wantLen:     3,
			wantPrompts: []string{"first", "🧑‍🤝‍🧑", "last"},
			wantRunes:   []int{5, 5, 4},
		},
		// ── Skin tone modifiers ───────────────────────────────────────────────
		{
			name:        "skin tone modifiers on thumbs up",
			input:       "👍🏻\n---\n👍🏽\n---\n👍🏿",
			wantLen:     3,
			wantPrompts: []string{"👍🏻", "👍🏽", "👍🏿"},
			wantRunes:   []int{2, 2, 2},
		},
		{
			name:        "skin tone modifier inside a zwj sequence",
			input:       "👩🏽‍💻 reviews\n---\n👨🏿‍🔬 experiments",
			wantLen:     2,
			wantPrompts: []string{"👩🏽‍💻 reviews", "👨🏿‍🔬 experiments"},
			wantRunes:   []int{12, 16},
		},
		// ── Regional indicator flags ──────────────────────────────────────────
		{
			name:        "regional indicator flags one per prompt",
			input:       "🇫🇷\n---\n🇩🇪\n---\n🇯🇵",
			wantLen:     3,
			wantPrompts: []string{"🇫🇷", "🇩🇪", "🇯🇵"},
			wantRunes:   []int{2, 2, 2},
		},
		{
			name:        "adjacent flags are not regrouped across the delimiter",
			input:       "🇺🇸🇬🇧\n---\n🇨🇦🇦🇺",
			wantLen:     2,
			wantPrompts: []string{"🇺🇸🇬🇧", "🇨🇦🇦🇺"},
			wantRunes:   []int{4, 4},
		},
		{
			name:        "tag sequence flag stays intact",
			input:       "Scotland 🏴󠁧󠁢󠁳󠁣󠁴󠁿\n---\nEngland 🏴󠁧󠁢󠁥󠁮󠁧󠁿",
			wantLen:     2,
			wantPrompts: []string{"Scotland 🏴󠁧󠁢󠁳󠁣󠁴󠁿", "England 🏴󠁧󠁢󠁥󠁮󠁧󠁿"},
			wantRunes:   []int{16, 15},
		},
		// ── Keycaps and variation selectors ───────────────────────────────────
		{
			name:        "keycap sequences with combining enclosing keycap",
			input:       "0️⃣\n---\n#️⃣\n---\n*️⃣",
			wantLen:     3,
			wantPrompts: []string{"0️⃣", "#️⃣", "*️⃣"},
			wantRunes:   []int{3, 3, 3},
		},
		{
			name:        "variation selector 16 is preserved",
			input:       "❤️ heart\n---\n☂️ umbrella",
			wantLen:     2,
			wantPrompts: []string{"❤️ heart", "☂️ umbrella"},
			wantRunes:   []int{8, 11},
		},
		{
			name:        "emoji and text presentation of the same base char stay distinct",
			input:       "❤️\n---\n❤",
			wantLen:     2,
			wantPrompts: []string{"❤️", "❤"},
			wantRunes:   []int{2, 1},
		},
		// ── Mixed composite sequences ─────────────────────────────────────────
		{
			name:        "several composite sequences in one prompt",
			input:       "👨‍👩‍👧 👍🏾 🇧🇷 7️⃣\n---\nplain",
			wantLen:     2,
			wantPrompts: []string{"👨‍👩‍👧 👍🏾 🇧🇷 7️⃣", "plain"},
			wantRunes:   []int{15, 5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			checkEmojiPrompts(t, prompts, tt.wantPrompts, tt.wantLen)
			for i, want := range tt.wantRunes {
				if i >= len(prompts) {
					break
				}
				if got := utf8.RuneCountInString(prompts[i]); got != want {
					t.Errorf("prompt[%d] = %q has %d runes, want %d", i, prompts[i], got, want)
				}
			}
		})
	}

	// Joiners carry the meaning of a composite sequence: if splitting dropped a
	// zero-width joiner or a variation selector the prompts would still compare
	// as "looks like emoji" but render as separate glyphs.
	t.Run("zero-width joiners and variation selectors survive", func(t *testing.T) {
		prompts := splitParallelPrompts("👨‍👩‍👧‍👦\n---\n❤️\n---\n5️⃣")
		if len(prompts) != 3 {
			t.Fatalf("got %d prompts, want 3: %q", len(prompts), prompts)
		}
		if got := strings.Count(prompts[0], "‍"); got != 3 {
			t.Errorf("family sequence has %d zero-width joiners, want 3", got)
		}
		if !strings.ContainsRune(prompts[1], '️') {
			t.Errorf("prompt[1] = %q lost variation selector U+FE0F", prompts[1])
		}
		if !strings.ContainsRune(prompts[2], '⃣') {
			t.Errorf("prompt[2] = %q lost combining enclosing keycap U+20E3", prompts[2])
		}
	})
}

// TestSplitParallelPrompts_EmojiAtDelimiterBoundaries verifies emoji sitting
// directly against the "\n---\n" delimiter, including emoji-only segments and
// literal "---" runs that are not delimiters.
func TestSplitParallelPrompts_EmojiAtDelimiterBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		wantPrompts []string
	}{
		{
			name:        "emoji touching both sides of delimiter",
			input:       "🚀\n---\n🔥",
			wantLen:     2,
			wantPrompts: []string{"🚀", "🔥"},
		},
		{
			name:        "zwj sequence touching both sides of delimiter",
			input:       "👨‍👩‍👧‍👦\n---\n👩‍💻",
			wantLen:     2,
			wantPrompts: []string{"👨‍👩‍👧‍👦", "👩‍💻"},
		},
		{
			name:        "flag touching both sides of delimiter",
			input:       "🇯🇵\n---\n🇰🇷",
			wantLen:     2,
			wantPrompts: []string{"🇯🇵", "🇰🇷"},
		},
		{
			name:        "skin tone modifier is last rune before delimiter",
			input:       "ship it 👍🏽\n---\nnext",
			wantLen:     2,
			wantPrompts: []string{"ship it 👍🏽", "next"},
		},
		{
			name:        "keycap sequence is first thing after delimiter",
			input:       "first\n---\n1️⃣ step one",
			wantLen:     2,
			wantPrompts: []string{"first", "1️⃣ step one"},
		},
		{
			name:        "emoji adjacent to leading delimiter",
			input:       "---\n🚀 launch\n---\n🔥 burn",
			wantLen:     2,
			wantPrompts: []string{"🚀 launch", "🔥 burn"},
		},
		{
			name:        "emoji adjacent to trailing delimiter",
			input:       "🚀 launch\n---\n🔥 burn\n---",
			wantLen:     2,
			wantPrompts: []string{"🚀 launch", "🔥 burn"},
		},
		{
			name:        "emoji-only segment between consecutive delimiters",
			input:       "first\n---\n🎉\n---\nlast",
			wantLen:     3,
			wantPrompts: []string{"first", "🎉", "last"},
		},
		{
			name:        "empty segment between emoji segments is dropped",
			input:       "🚀\n---\n\n---\n🔥",
			wantLen:     2,
			wantPrompts: []string{"🚀", "🔥"},
		},
		{
			name:        "whitespace-only segment between emoji segments is dropped",
			input:       "🚀\n---\n   \n---\n🔥",
			wantLen:     2,
			wantPrompts: []string{"🚀", "🔥"},
		},
		{
			name:        "dashes on the same line as emoji do not split",
			input:       "🚀---🔥\n---\n🎯---🎉",
			wantLen:     2,
			wantPrompts: []string{"🚀---🔥", "🎯---🎉"},
		},
		{
			name:        "delimiter-like line flanked by emoji does not split",
			input:       "🚀\n--- 🔥\nstill one prompt",
			wantLen:     1,
			wantPrompts: []string{"🚀\n--- 🔥\nstill one prompt"},
		},
		{
			name:        "emoji dash lookalikes are not a delimiter",
			input:       "🚀\n➖➖➖\n🔥",
			wantLen:     1,
			wantPrompts: []string{"🚀\n➖➖➖\n🔥"},
		},
		{
			name:        "trailing whitespace after emoji before delimiter is trimmed",
			input:       "🚀 launch  \n---\n  🔥 burn",
			wantLen:     2,
			wantPrompts: []string{"🚀 launch", "🔥 burn"},
		},
		{
			name:        "blank lines around emoji segments are trimmed",
			input:       "\n\n🚀\n\n---\n\n🔥\n\n",
			wantLen:     2,
			wantPrompts: []string{"🚀", "🔥"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkEmojiPrompts(t, splitParallelPrompts(tt.input), tt.wantPrompts, tt.wantLen)
		})
	}
}

// ── length limits and truncation ───────────────────────────────────────────────
//
// splitParallelPrompts applies no length limit of its own: it splits on
// "\n---\n", trims each segment, and returns whatever remains. The package's
// only length ceiling is maxMessageLen (4096), which belongs to the Telegram
// sender and is applied downstream when *results* are posted back — never to
// prompts on the way in. The tests below pin that contract down so a long
// prompt reaches Claude whole instead of being silently clipped at a delimiter
// or, worse, in the middle of a multi-byte rune.

// checkNoTruncation asserts prompts match wantPrompts exactly — byte count,
// rune count, and content — so silent clipping surfaces as a length mismatch
// rather than an unreadable inequality on multi-kilobyte strings.
func checkNoTruncation(t *testing.T, prompts, wantPrompts []string) {
	t.Helper()
	if len(prompts) != len(wantPrompts) {
		t.Fatalf("got %d prompts, want %d", len(prompts), len(wantPrompts))
	}
	for i, want := range wantPrompts {
		got := prompts[i]
		if len(got) != len(want) {
			t.Errorf("prompt[%d] has %d bytes, want %d (short by %d)", i, len(got), len(want), len(want)-len(got))
		}
		if g, w := utf8.RuneCountInString(got), utf8.RuneCountInString(want); g != w {
			t.Errorf("prompt[%d] has %d runes, want %d", i, g, w)
		}
		if got != want {
			// Report the divergence offset instead of dumping kilobytes.
			t.Errorf("prompt[%d] diverges from want at byte offset %d", i, firstDiffOffset(got, want))
		}
		if !utf8.ValidString(got) {
			t.Errorf("prompt[%d] is not valid UTF-8 — cut mid-rune?", i)
		}
		if strings.ContainsRune(got, utf8.RuneError) && !strings.ContainsRune(want, utf8.RuneError) {
			t.Errorf("prompt[%d] gained the replacement character U+FFFD", i)
		}
	}
}

// firstDiffOffset returns the byte offset of the first difference between a and
// b, or the length of the shorter string if one is a prefix of the other.
func firstDiffOffset(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// TestSplitParallelPrompts_SinglePromptExceedsLengthLimit verifies that a single
// prompt straddling maxMessageLen is returned whole — the sender's 4096-byte
// ceiling is not applied to prompts.
func TestSplitParallelPrompts_SinglePromptExceedsLengthLimit(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "one byte under the limit",
			input: strings.Repeat("a", maxMessageLen-1),
			want:  []string{strings.Repeat("a", maxMessageLen-1)},
		},
		{
			name:  "exactly at the limit",
			input: strings.Repeat("a", maxMessageLen),
			want:  []string{strings.Repeat("a", maxMessageLen)},
		},
		{
			name:  "one byte over the limit",
			input: strings.Repeat("a", maxMessageLen+1),
			want:  []string{strings.Repeat("a", maxMessageLen+1)},
		},
		{
			name:  "ten times the limit",
			input: strings.Repeat("a", maxMessageLen*10),
			want:  []string{strings.Repeat("a", maxMessageLen*10)},
		},
		{
			name:  "over the limit with distinct tail so a prefix cut is detectable",
			input: strings.Repeat("a", maxMessageLen) + "TAIL-SENTINEL",
			want:  []string{strings.Repeat("a", maxMessageLen) + "TAIL-SENTINEL"},
		},
		{
			name:  "over the limit wrapped in whitespace trims only the edges",
			input: "\n\n  " + strings.Repeat("b", maxMessageLen+500) + "  \n\n",
			want:  []string{strings.Repeat("b", maxMessageLen+500)},
		},
		{
			name:  "over the limit with embedded newlines stays one prompt",
			input: strings.Repeat("line of text\n", 500),
			want:  []string{strings.TrimSpace(strings.Repeat("line of text\n", 500))},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkNoTruncation(t, splitParallelPrompts(tt.input), tt.want)
		})
	}
}

// TestSplitParallelPrompts_MultipleLongPromptsNotTruncated verifies that every
// segment of a multi-prompt input survives at full length, including inputs
// whose combined size dwarfs maxMessageLen.
func TestSplitParallelPrompts_MultipleLongPromptsNotTruncated(t *testing.T) {
	var (
		longA = strings.Repeat("A", maxMessageLen+1)
		longB = strings.Repeat("B", maxMessageLen+1)
		longC = strings.Repeat("C", maxMessageLen*3)
	)

	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "two prompts each over the limit",
			input: longA + "\n---\n" + longB,
			want:  []string{longA, longB},
		},
		{
			name:  "five prompts each over the limit",
			input: strings.Join([]string{longA, longB, longC, longA, longB}, "\n---\n"),
			want:  []string{longA, longB, longC, longA, longB},
		},
		{
			name: "each segment under the limit but the total is far over",
			input: strings.Join([]string{
				strings.Repeat("x", 2000),
				strings.Repeat("y", 2000),
				strings.Repeat("z", 2000),
			}, "\n---\n"),
			want: []string{
				strings.Repeat("x", 2000),
				strings.Repeat("y", 2000),
				strings.Repeat("z", 2000),
			},
		},
		{
			name:  "long prompt followed by a short one",
			input: longA + "\n---\nshort",
			want:  []string{longA, "short"},
		},
		{
			name:  "short prompt followed by a long one",
			input: "short\n---\n" + longB,
			want:  []string{"short", longB},
		},
		{
			name:  "long prompts around a dropped empty segment",
			input: longA + "\n---\n\n---\n" + longB,
			want:  []string{longA, longB},
		},
		{
			name:  "long prompts with padded segments trimmed at the edges only",
			input: "  " + longA + "  \n---\n\t" + longB + "\t",
			want:  []string{longA, longB},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkNoTruncation(t, splitParallelPrompts(tt.input), tt.want)
		})
	}
}

// TestSplitParallelPrompts_UnicodeExceedsLengthLimit verifies that oversized
// multi-byte content is not clipped. A byte-oriented cut at maxMessageLen would
// land mid-rune for most of these inputs, so the rune-count and validity checks
// in checkNoTruncation are the real assertions here.
func TestSplitParallelPrompts_UnicodeExceedsLengthLimit(t *testing.T) {
	var (
		// 3 bytes per rune: byte offset 4096 falls one byte into a rune.
		cjk = strings.Repeat("日", 2000)
		// 4 bytes per rune: byte offset 4096 falls on a rune boundary, so a
		// naive cut would silently produce a shorter-but-valid string.
		emoji = strings.Repeat("🚀", 1500)
		// Decomposed sequences: a cut could strand a combining mark.
		combining = strings.Repeat("é", 3000) // e + U+0301
		// Mixed scripts, well past the limit.
		mixed = strings.Repeat("aé日🚀Ω", 1200)
	)

	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "CJK prompt over the limit in bytes",
			input: cjk,
			want:  []string{cjk},
		},
		{
			name:  "emoji prompt over the limit in bytes",
			input: emoji,
			want:  []string{emoji},
		},
		{
			name:  "combining sequences over the limit",
			input: combining,
			want:  []string{combining},
		},
		{
			name:  "mixed scripts over the limit",
			input: mixed,
			want:  []string{mixed},
		},
		{
			name:  "prompt over the limit in runes as well as bytes",
			input: strings.Repeat("日", maxMessageLen+1),
			want:  []string{strings.Repeat("日", maxMessageLen+1)},
		},
		{
			name:  "two oversized Unicode prompts",
			input: cjk + "\n---\n" + emoji,
			want:  []string{cjk, emoji},
		},
		{
			name:  "oversized Unicode prompts with a ZWJ family sequence at the seam",
			input: cjk + "👨‍👩‍👧‍👦\n---\n👨‍👩‍👧‍👦" + emoji,
			want:  []string{cjk + "👨‍👩‍👧‍👦", "👨‍👩‍👧‍👦" + emoji},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			checkNoTruncation(t, prompts, tt.want)
			for i, p := range prompts {
				if r, size := utf8.DecodeLastRuneInString(p); r == utf8.RuneError && size <= 1 {
					t.Errorf("prompt[%d] ends in a severed rune", i)
				}
				if r, size := utf8.DecodeRuneInString(p); r == utf8.RuneError && size <= 1 {
					t.Errorf("prompt[%d] starts in a severed rune", i)
				}
			}
		})
	}
}

// TestSplitParallelPrompts_LimitBoundaryAtDelimiterVsMidPrompt places
// maxMessageLen at a delimiter boundary and then in the middle of a segment.
// Both must return whole prompts: the boundary is not a cut point either way.
func TestSplitParallelPrompts_LimitBoundaryAtDelimiterVsMidPrompt(t *testing.T) {
	const delim = "\n---\n"
	tail := strings.Repeat("t", 500)

	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "delimiter starts exactly at the limit",
			input: strings.Repeat("h", maxMessageLen) + delim + tail,
			want:  []string{strings.Repeat("h", maxMessageLen), tail},
		},
		{
			name:  "delimiter ends exactly at the limit",
			input: strings.Repeat("h", maxMessageLen-len(delim)) + delim + tail,
			want:  []string{strings.Repeat("h", maxMessageLen-len(delim)), tail},
		},
		{
			name:  "limit falls inside the delimiter itself",
			input: strings.Repeat("h", maxMessageLen-2) + delim + tail,
			want:  []string{strings.Repeat("h", maxMessageLen-2), tail},
		},
		{
			name:  "limit falls mid-way through the first prompt",
			input: strings.Repeat("h", maxMessageLen*2) + delim + tail,
			want:  []string{strings.Repeat("h", maxMessageLen*2), tail},
		},
		{
			name:  "limit falls mid-way through the second prompt",
			input: strings.Repeat("h", 4000) + delim + strings.Repeat("s", 4000),
			want:  []string{strings.Repeat("h", 4000), strings.Repeat("s", 4000)},
		},
		{
			name:  "limit falls on the last byte of the first prompt",
			input: strings.Repeat("h", maxMessageLen) + delim + strings.Repeat("s", maxMessageLen),
			want:  []string{strings.Repeat("h", maxMessageLen), strings.Repeat("s", maxMessageLen)},
		},
		{
			name:  "limit falls mid-rune inside an oversized Unicode prompt",
			input: strings.Repeat("日", 2000) + delim + tail,
			want:  []string{strings.Repeat("日", 2000), tail},
		},
		{
			name:  "literal --- at the limit is content, not a delimiter",
			input: strings.Repeat("h", maxMessageLen) + "---" + tail,
			want:  []string{strings.Repeat("h", maxMessageLen) + "---" + tail},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkNoTruncation(t, splitParallelPrompts(tt.input), tt.want)
		})
	}
}

// TestSplitParallelPrompts_ManyDelimitersInLongInput verifies that delimiter
// count and placement do not interact with input size: every segment of a
// far-over-limit input comes back intact, and delimiter-lookalikes buried deep
// in long prompts still do not split.
func TestSplitParallelPrompts_ManyDelimitersInLongInput(t *testing.T) {
	// Ten 1000-byte segments: ~10KB total, each segment individually small.
	tenSegments := make([]string, 10)
	for i := range tenSegments {
		tenSegments[i] = strings.Repeat(string(rune('A'+i)), 1000)
	}

	// Three segments, each itself over the limit.
	threeHuge := []string{
		strings.Repeat("p", maxMessageLen+1),
		strings.Repeat("q", maxMessageLen+7),
		strings.Repeat("r", maxMessageLen+13),
	}

	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "ten delimited segments in a 10KB input",
			input: strings.Join(tenSegments, "\n---\n"),
			want:  tenSegments,
		},
		{
			name:  "three oversized segments",
			input: strings.Join(threeHuge, "\n---\n"),
			want:  threeHuge,
		},
		{
			name:  "consecutive delimiters between long segments collapse",
			input: threeHuge[0] + "\n---\n\n---\n\n---\n" + threeHuge[1],
			want:  []string{threeHuge[0], threeHuge[1]},
		},
		{
			name:  "leading and trailing delimiters on a long input",
			input: "\n---\n" + threeHuge[0] + "\n---\n" + threeHuge[1] + "\n---\n",
			want:  []string{threeHuge[0], threeHuge[1]},
		},
		{
			name:  "indented delimiters deep inside a long input split",
			input: strings.Repeat("z", maxMessageLen) + "\n  ---  \n" + strings.Repeat("z", maxMessageLen),
			want:  []string{strings.Repeat("z", maxMessageLen), strings.Repeat("z", maxMessageLen)},
		},
		{
			name:  "long horizontal rules inside a long prompt do not split",
			input: strings.Repeat("z", maxMessageLen) + "\n-----\n" + strings.Repeat("z", 1000),
			want:  []string{strings.Repeat("z", maxMessageLen) + "\n-----\n" + strings.Repeat("z", 1000)},
		},
		{
			name:  "many delimiters yield many prompts regardless of total size",
			input: strings.Repeat(strings.Repeat("m", 200)+"\n---\n", 49) + strings.Repeat("m", 200),
			want: func() []string {
				out := make([]string, 50)
				for i := range out {
					out[i] = strings.Repeat("m", 200)
				}
				return out
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkNoTruncation(t, splitParallelPrompts(tt.input), tt.want)
		})
	}
}

// TestSplitParallelPrompts_LengthLimitBoundaries tests exact length limit boundary
// conditions including empty prompts, single characters, very short Unicode/emoji
// prompts, and prompts at exact byte boundaries. These tests verify that the
// function handles edge cases at the extremes of input length without truncation
// or corruption.
func TestSplitParallelPrompts_LengthLimitBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		wantPrompts []string
		verify      func([]string) bool
	}{
		// ── Empty and zero-length inputs ─────────────────────────────────────────
		{
			name:        "empty string length 0",
			input:       "",
			wantLen:     1,
			wantPrompts: []string{""},
		},
		{
			name:        "single newline",
			input:       "\n",
			wantLen:     1,
			wantPrompts: []string{""},
		},
		{
			name:        "multiple newlines only",
			input:       "\n\n\n",
			wantLen:     1,
			wantPrompts: []string{""},
		},
		// ── Single character prompts ───────────────────────────────────────────────
		{
			name:        "single ASCII character",
			input:       "a",
			wantLen:     1,
			wantPrompts: []string{"a"},
		},
		{
			name:        "single digit",
			input:       "5",
			wantLen:     1,
			wantPrompts: []string{"5"},
		},
		{
			name:        "single punctuation",
			input:       "?",
			wantLen:     1,
			wantPrompts: []string{"?"},
		},
		{
			name:        "single space",
			input:       " ",
			wantLen:     1,
			wantPrompts: []string{""},
		},
		{
			name:        "single tab",
			input:       "\t",
			wantLen:     1,
			wantPrompts: []string{""},
		},
		{
			name:        "two single-char prompts with delimiter",
			input:       "a\n---\nb",
			wantLen:     2,
			wantPrompts: []string{"a", "b"},
		},
		{
			name:        "five single-char prompts",
			input:       "a\n---\nb\n---\nc\n---\nd\n---\ne",
			wantLen:     5,
			wantPrompts: []string{"a", "b", "c", "d", "e"},
		},
		// ── Very short prompts (2-5 bytes) ─────────────────────────────────────────
		{
			name:        "two ASCII characters",
			input:       "ab",
			wantLen:     1,
			wantPrompts: []string{"ab"},
		},
		{
			name:        "three ASCII characters",
			input:       "abc",
			wantLen:     1,
			wantPrompts: []string{"abc"},
		},
		{
			name:        "five ASCII characters",
			input:       "abcde",
			wantLen:     1,
			wantPrompts: []string{"abcde"},
		},
		{
			name:        "short word",
			input:       "test",
			wantLen:     1,
			wantPrompts: []string{"test"},
		},
		// ── Very short Unicode prompts ─────────────────────────────────────────────
		{
			name:        "single CJK character (3 bytes)",
			input:       "日",
			wantLen:     1,
			wantPrompts: []string{"日"},
			verify: func(p []string) bool {
				return utf8.RuneCountInString(p[0]) == 1 && len(p[0]) == 3
			},
		},
		{
			name:        "single emoji (4 bytes)",
			input:       "🚀",
			wantLen:     1,
			wantPrompts: []string{"🚀"},
			verify: func(p []string) bool {
				return utf8.RuneCountInString(p[0]) == 1 && len(p[0]) == 4
			},
		},
		{
			name:        "single accented Latin (2 bytes)",
			input:       "é",
			wantLen:     1,
			wantPrompts: []string{"é"},
			verify: func(p []string) bool {
				return utf8.RuneCountInString(p[0]) == 1 && len(p[0]) == 2
			},
		},
		{
			name:        "two CJK characters",
			input:       "日本",
			wantLen:     1,
			wantPrompts: []string{"日本"},
			verify: func(p []string) bool {
				return utf8.RuneCountInString(p[0]) == 2 && len(p[0]) == 6
			},
		},
		{
			name:        "two emoji",
			input:       "🚀🔥",
			wantLen:     1,
			wantPrompts: []string{"🚀🔥"},
			verify: func(p []string) bool {
				return utf8.RuneCountInString(p[0]) == 2 && len(p[0]) == 8
			},
		},
		{
			name:        "three CJK characters",
			input:       "日本語",
			wantLen:     1,
			wantPrompts: []string{"日本語"},
			verify: func(p []string) bool {
				return utf8.RuneCountInString(p[0]) == 3 && len(p[0]) == 9
			},
		},
		// ── Very short prompts with delimiter ────────────────────────────────────
		{
			name:        "single CJK per prompt",
			input:       "日\n---\n本\n---\n語",
			wantLen:     3,
			wantPrompts: []string{"日", "本", "語"},
			verify: func(p []string) bool {
				for _, s := range p {
					if utf8.RuneCountInString(s) != 1 || len(s) != 3 {
						return false
					}
				}
				return true
			},
		},
		{
			name:        "single emoji per prompt",
			input:       "🚀\n---\n🔥\n---\n🎯",
			wantLen:     3,
			wantPrompts: []string{"🚀", "🔥", "🎯"},
			verify: func(p []string) bool {
				for _, s := range p {
					if utf8.RuneCountInString(s) != 1 || len(s) != 4 {
						return false
					}
				}
				return true
			},
		},
		{
			name:        "mixed single-char Unicode prompts",
			input:       "日\n---\n🚀\n---\né",
			wantLen:     3,
			wantPrompts: []string{"日", "🚀", "é"},
		},
		{
			name:        "two-byte char per prompt",
			input:       "é\n---\nñ\n---\nü",
			wantLen:     3,
			wantPrompts: []string{"é", "ñ", "ü"},
		},
		// ── Prompts at exact byte boundaries ────────────────────────────────────────
		{
			name:        "exactly 1 byte prompt",
			input:       "a",
			wantLen:     1,
			wantPrompts: []string{"a"},
		},
		{
			name:        "exactly 2 bytes (two ASCII)",
			input:       "ab",
			wantLen:     1,
			wantPrompts: []string{"ab"},
		},
		{
			name:        "exactly 3 bytes (one CJK)",
			input:       "日",
			wantLen:     1,
			wantPrompts: []string{"日"},
		},
		{
			name:        "exactly 4 bytes (one emoji)",
			input:       "🚀",
			wantLen:     1,
			wantPrompts: []string{"🚀"},
		},
		{
			name:        "exactly 5 bytes (mixed)",
			input:       "a日",
			wantLen:     1,
			wantPrompts: []string{"a日"},
			verify: func(p []string) bool {
				return len(p[0]) == 4 // 'a'=1 + '日'=3
			},
		},
		{
			name:        "exactly 6 bytes (two CJK)",
			input:       "日本",
			wantLen:     1,
			wantPrompts: []string{"日本"},
		},
		{
			name:        "exactly 7 bytes (ASCII + emoji)",
			input:       "ab🚀",
			wantLen:     1,
			wantPrompts: []string{"ab🚀"},
			verify: func(p []string) bool {
				return len(p[0]) == 6 // 'a'=1 + 'b'=1 + '🚀'=4
			},
		},
		{
			name:        "exactly 8 bytes (two emoji)",
			input:       "🚀🔥",
			wantLen:     1,
			wantPrompts: []string{"🚀🔥"},
		},
		{
			name:        "exactly 10 bytes (mixed)",
			input:       "abc日🚀",
			wantLen:     1,
			wantPrompts: []string{"abc日🚀"},
			verify: func(p []string) bool {
				return len(p[0]) == 10 // 'a'=1+'b'=1+'c'=1+'日'=3+'🚀'=4
			},
		},
		// ── Short prompts at delimiter boundaries ───────────────────────────────────
		{
			name:        "1-byte prompts with delimiter",
			input:       "a\n---\nb\n---\nc",
			wantLen:     3,
			wantPrompts: []string{"a", "b", "c"},
		},
		{
			name:        "3-byte CJK prompts with delimiter",
			input:       "日\n---\n本\n---\n語",
			wantLen:     3,
			wantPrompts: []string{"日", "本", "語"},
		},
		{
			name:        "4-byte emoji prompts with delimiter",
			input:       "🚀\n---\n🔥\n---\n🎯",
			wantLen:     3,
			wantPrompts: []string{"🚀", "🔥", "🎯"},
		},
		{
			name:        "mixed-byte prompts with delimiter",
			input:       "a\n---\n日\n---\n🚀\n---\né",
			wantLen:     4,
			wantPrompts: []string{"a", "日", "🚀", "é"},
		},
		// ── Very short prompts with Unicode zero-width joiner ────────────────────────
		{
			name:        "single ZWJ sequence (family emoji)",
			input:       "👨‍👩‍👧‍👦",
			wantLen:     1,
			wantPrompts: []string{"👨‍👩‍👧‍👦"},
			verify: func(p []string) bool {
				// Family emoji is 7 code points: 👨(1) + ‍(1) + 👩(1) + ‍(1) + 👧(1) + ‍(1) + 👦(1)
				// Each emoji is 4 bytes, each ZWJ is 3 bytes = 4+3+4+3+4+3+4 = 25 bytes
				return utf8.RuneCountInString(p[0]) == 7 && len(p[0]) == 25
			},
		},
		{
			name:        "single ZWJ sequence at delimiter boundary",
			input:       "👨‍👩‍👧‍👦\n---\nnext",
			wantLen:     2,
			wantPrompts: []string{"👨‍👩‍👧‍👦", "next"},
		},
		// ── Short prompts with skin tone modifiers ────────────────────────────────────
		{
			name:        "emoji with skin tone modifier",
			input:       "👍🏽",
			wantLen:     1,
			wantPrompts: []string{"👍🏽"},
			verify: func(p []string) bool {
				// 👍🏽 is a multi-byte sequence (base emoji + skin tone modifier)
				return utf8.RuneCountInString(p[0]) == 2 && len(p[0]) > 4
			},
		},
		{
			name:        "skin tone emoji at delimiter boundary",
			input:       "👍🏽\n---\n👍🏿",
			wantLen:     2,
			wantPrompts: []string{"👍🏽", "👍🏿"},
		},
		// ── Regional indicator flags (2 regional indicators = 1 flag) ─────────────────
		{
			name:        "single flag emoji (8 bytes)",
			input:       "🇯🇵",
			wantLen:     1,
			wantPrompts: []string{"🇯🇵"},
			verify: func(p []string) bool {
				// Two regional indicators = 8 bytes (4 bytes each)
				return utf8.RuneCountInString(p[0]) == 2 && len(p[0]) == 8
			},
		},
		{
			name:        "flag emoji at delimiter boundary",
			input:       "🇯🇵\n---\n🇨🇳\n---\n🇫🇷",
			wantLen:     3,
			wantPrompts: []string{"🇯🇵", "🇨🇳", "🇫🇷"},
		},
		// ── Keycap emoji sequences ────────────────────────────────────────────────────
		{
			name:        "single keycap (7 bytes)",
			input:       "1️⃣",
			wantLen:     1,
			wantPrompts: []string{"1️⃣"},
			verify: func(p []string) bool {
				// '1' (1) + variation selector (4) + combining keycap (3) = 8 bytes
				return len(p[0]) >= 7
			},
		},
		{
			name:        "keycap at delimiter boundary",
			input:       "1️⃣\n---\n2️⃣\n---\n3️⃣",
			wantLen:     3,
			wantPrompts: []string{"1️⃣", "2️⃣", "3️⃣"},
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
			if tt.verify != nil && !tt.verify(prompts) {
				t.Errorf("verification failed for prompts: %v", prompts)
			}
			// Verify UTF-8 validity for all prompts
			for i, p := range prompts {
				if !utf8.ValidString(p) {
					t.Errorf("prompt[%d] = %q is not valid UTF-8", i, p)
				}
				if strings.ContainsRune(p, utf8.RuneError) && !strings.Contains(tt.input, string(utf8.RuneError)) {
					t.Errorf("prompt[%d] = %q contains the replacement character U+FFFD", i, p)
				}
			}
		})
	}
}

// TestSplitParallelPrompts_ZeroWidthCharacters tests zero-width characters
// including zero-width space (U+200B), zero-width non-joiner (U+200C), and
// zero-width joiner (U+200D). These invisible characters must survive
// splitting intact to preserve text rendering and formatting behavior.
func TestSplitParallelPrompts_ZeroWidthCharacters(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		wantPrompts []string
		verify      func([]string) bool
	}{
		{
			name:        "zero-width space (U+200B) within text",
			input:       "word​break​test\n---\nnext",
			wantLen:     2,
			wantPrompts: []string{"word​break​test", "next"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "​")
			},
		},
		{
			name:        "zero-width space at delimiter boundary",
			input:       "test​\n---\n​start",
			wantLen:     2,
			wantPrompts: []string{"test​", "​start"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "​") && strings.Contains(prompts[1], "​")
			},
		},
		{
			name:        "zero-width non-joiner (U+200C) within text",
			input:       "نَص‌عربي\n---\nnext",
			wantLen:     2,
			wantPrompts: []string{"نَص‌عربي", "next"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "‌")
			},
		},
		{
			name:        "zero-width non-joiner in Hindi text",
			input:       "हिंदी‌पाठ\n---\nनमस्ते",
			wantLen:     2,
			wantPrompts: []string{"हिंदी‌पाठ", "नमस्ते"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "‌")
			},
		},
		{
			name:        "zero-width joiner (U+200D) outside emoji context",
			input:       "test‍joined\n---\nnext",
			wantLen:     2,
			wantPrompts: []string{"test‍joined", "next"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "‍")
			},
		},
		{
			name:        "multiple zero-width characters in sequence",
			input:       "a​‌‍b\n---\nc",
			wantLen:     2,
			wantPrompts: []string{"a​‌‍b", "c"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "​") &&
					strings.Contains(prompts[0], "‌") &&
					strings.Contains(prompts[0], "‍")
			},
		},
		{
			name:        "zero-width characters with emoji",
			input:       "🚀​🔥\n---\n🎯‌🎉",
			wantLen:     2,
			wantPrompts: []string{"🚀​🔥", "🎯‌🎉"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "​") &&
					strings.Contains(prompts[1], "‌")
			},
		},
		{
			name:        "zero-width characters in multilingual text",
			input:       "English​日本语‌العربية\n---\nnext",
			wantLen:     2,
			wantPrompts: []string{"English​日本语‌العربية", "next"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "​") &&
					strings.Contains(prompts[0], "‌")
			},
		},
		{
			name:        "zero-width characters survive long content",
			input:       strings.Repeat("test", 1000) + "​‌‍\n---\nend",
			wantLen:     2,
			wantPrompts: []string{strings.Repeat("test", 1000) + "​‌‍", "end"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "​") &&
					strings.Contains(prompts[0], "‌") &&
					strings.Contains(prompts[0], "‍")
			},
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
			if tt.verify != nil && !tt.verify(prompts) {
				t.Errorf("verification failed for prompts: %v", prompts)
			}
			for i, p := range prompts {
				if !utf8.ValidString(p) {
					t.Errorf("prompt[%d] = %q is not valid UTF-8", i, p)
				}
			}
		})
	}
}

// TestSplitParallelPrompts_KoreanHangul tests Korean text (Hangul) including
// modern Hangul syllables, Jamo (consonant/vowel clusters), and mixed
// Korean-ASCII content.
func TestSplitParallelPrompts_KoreanHangul(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		wantPrompts []string
		verify      func([]string) bool
	}{
		{
			name:        "basic korean hangul syllables",
			input:       "안녕하세요\n---\n반갑습니다\n---\n감사합니다",
			wantLen:     3,
			wantPrompts: []string{"안녕하세요", "반갑습니다", "감사합니다"},
			verify: func(prompts []string) bool {
				// Verify all Korean syllables are preserved
				return utf8.RuneCountInString(prompts[0]) == 5 &&
					utf8.RuneCountInString(prompts[1]) == 5 &&
					utf8.RuneCountInString(prompts[2]) == 5
			},
		},
		{
			name:        "korean text with numbers",
			input:       "테스트 123\n---\n결과 456",
			wantLen:     2,
			wantPrompts: []string{"테스트 123", "결과 456"},
		},
		{
			name:        "korean with ascii words",
			input:       "한글 Korean test\n---\nNext 다음 task",
			wantLen:     2,
			wantPrompts: []string{"한글 Korean test", "Next 다음 task"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "한글") &&
					strings.Contains(prompts[1], "다음")
			},
		},
		{
			name:        "korean at delimiter boundaries",
			input:       "안녕\n---\n하세요\n---\n반갑습니다",
			wantLen:     3,
			wantPrompts: []string{"안녕", "하세요", "반갑습니다"},
		},
		{
			name:        "korean with emoji",
			input:       "안녕하세요! 🇰🇷\n---\n반갑습니다 🎉\n---\n감사합니다 ❤️",
			wantLen:     3,
			wantPrompts: []string{"안녕하세요! 🇰🇷", "반갑습니다 🎉", "감사합니다 ❤️"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "안녕하세요") &&
					strings.Contains(prompts[1], "반갑습니다") &&
					strings.Contains(prompts[2], "감사합니다")
			},
		},
		{
			name:        "multiline korean text",
			input:       "첫 번째 줄\n두 번째 줄\n---\n다음 텍스트",
			wantLen:     2,
			wantPrompts: []string{"첫 번째 줄\n두 번째 줄", "다음 텍스트"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "첫 번째") &&
					strings.Contains(prompts[0], "\n")
			},
		},
		{
			name:        "korean technical terms with code",
			input:       "Git 커밋 하기\n---\n코드 리뷰 `git diff`",
			wantLen:     2,
			wantPrompts: []string{"Git 커밋 하기", "코드 리뷰 `git diff`"},
		},
		{
			name:        "korean programming terminology",
			input:       "함수 Function 실행\n---\n배열 Array 정렬",
			wantLen:     2,
			wantPrompts: []string{"함수 Function 실행", "배열 Array 정렬"},
		},
		{
			name:        "korean with other scripts",
			input:       "한글 日本語 中文\n---\nEnglish 한국어 test",
			wantLen:     2,
			wantPrompts: []string{"한글 日本語 中文", "English 한국어 test"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "한글") &&
					strings.Contains(prompts[0], "日本語") &&
					strings.Contains(prompts[0], "中文") &&
					strings.Contains(prompts[1], "한국어")
			},
		},
		{
			name:        "korean question and exclamation marks",
			input:       "안녕하세요?\n---\n환영합니다!\n---\n감사합니다~",
			wantLen:     3,
			wantPrompts: []string{"안녕하세요?", "환영합니다!", "감사합니다~"},
		},
		{
			name:        "long korean text preserved",
			input:       strings.Repeat("한글 테스트", 100) + "\n---\nend",
			wantLen:     2,
			wantPrompts: []string{strings.Repeat("한글 테스트", 100), "end"},
			verify: func(prompts []string) bool {
				return strings.HasPrefix(prompts[0], "한글 테스트") && len(prompts[0]) > 1000
			},
		},
		{
			name:        "korean consonant jamo ᄀᄂᄃ",
			input:       "ᄀᄂᄃ\n---\nᅡᅢᅣ",
			wantLen:     2,
			wantPrompts: []string{"ᄀᄂᄃ", "ᅡᅢᅣ"},
		},
		{
			name:        "mixed jamo and syllables",
			input:       "안녕하세요\n---\n반ᄀᆞᆸ습니다",
			wantLen:     2,
			wantPrompts: []string{"안녕하세요", "반ᄀᆞᆸ습니다"},
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
			if tt.verify != nil && !tt.verify(prompts) {
				t.Errorf("verification failed for prompts: %v", prompts)
			}
			for i, p := range prompts {
				if !utf8.ValidString(p) {
					t.Errorf("prompt[%d] = %q is not valid UTF-8", i, p)
				}
				if strings.ContainsRune(p, utf8.RuneError) {
					t.Errorf("prompt[%d] = %q contains the replacement character U+FFFD", i, p)
				}
			}
		})
	}
}

// TestSplitParallelPrompts_AdditionalCombiningMarks tests additional combining
// diacritical marks beyond those already covered, including combining macron,
// breve, diaeresis, cedilla, ogonek, and ring above in various scripts.
func TestSplitParallelPrompts_AdditionalCombiningMarks(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		wantPrompts []string
		verify      func([]string) bool
	}{
		{
			name:        "combining macron (āēīōū)",
			input:       "ābēc̄dn\n---\nnext",
			wantLen:     2,
			wantPrompts: []string{"ābēc̄dn", "next"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "̄") // combining macron
			},
		},
		{
			name:        "combining breve (ăĭŏ)",
			input:       "brevĕa tĕst\n---\nnext",
			wantLen:     2,
			wantPrompts: []string{"brevĕa tĕst", "next"},
		},
		{
			name:        "combining diaeresis (äëïöü)",
			input:       "Mädlėn's name\n---\nnaïve recäve",
			wantLen:     2,
			wantPrompts: []string{"Mädlėn's name", "naïve recäve"},
		},
		{
			name:        "combining cedilla (çşţ)",
			input:       "françois garçon\n---\ntest Romanian",
			wantLen:     2,
			wantPrompts: []string{"françois garçon", "test Romanian"},
		},
		{
			name:        "combining ogonek (ąęįų)",
			input:       "Polish język\n---\nnext test",
			wantLen:     2,
			wantPrompts: []string{"Polish język", "next test"},
		},
		{
			name:        "combining ring above (åų)",
			input:       "København Århus\n---\nnext test",
			wantLen:     2,
			wantPrompts: []string{"København Århus", "next test"},
		},
		{
			name:        "combining tilde (ãñõ)",
			input:       "añorance señor\n---\nõpalā",
			wantLen:     2,
			wantPrompts: []string{"añorance señor", "õpalā"},
		},
		{
			name:        "combining dot above (żċ)",
			input:       "Maltast żobb\n---\nnext",
			wantLen:     2,
			wantPrompts: []string{"Maltast żobb", "next"},
		},
		{
			name:        "multiple combining marks on single character",
			input:       "ā́n (macron + acute)\n---\nnext",
			wantLen:     2,
			wantPrompts: []string{"ā́n (macron + acute)", "next"},
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "̄") && strings.Contains(prompts[0], "́")
			},
		},
		{
			name:        "combining marks at delimiter boundaries",
			input:       "năv\n---\nĭgăt\n---\nŭt",
			wantLen:     3,
			wantPrompts: []string{"năv", "ĭgăt", "ŭt"},
		},
		{
			name:        "combining marks with emoji",
			input:       "Test 🎯 with marks ē\n---\nNext 🚀",
			wantLen:     2,
			wantPrompts: []string{"Test 🎯 with marks ē", "Next 🚀"},
		},
		{
			name:        "combining marks in multiple languages",
			input:       "françois käse\n---\nnaïve võörk",
			wantLen:     2,
			wantPrompts: []string{"françois käse", "naïve võörk"},
		},
		{
			name:        "combining caron (ščřž)",
			input:       "Češki jězyk\n---\nPŕagůe",
			wantLen:     2,
			wantPrompts: []string{"Češki jězyk", "Pŕagůe"},
		},
		{
			name:        "combining double acute (őű)",
			input:       "Hungarian őű\n---\nnext",
			wantLen:     2,
			wantPrompts: []string{"Hungarian őű", "next"},
		},
		{
			name:        "combining horn (ơư)",
			input:       "Tiếng Việt ơ\n---\nđưà",
			wantLen:     2,
			wantPrompts: []string{"Tiếng Việt ơ", "đưà"},
		},
		{
			name:        "combining hook above (ảỏ)",
			input:       "Tiếng Việt ả\n---\nđỏ",
			wantLen:     2,
			wantPrompts: []string{"Tiếng Việt ả", "đỏ"},
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
			if tt.verify != nil && !tt.verify(prompts) {
				t.Errorf("verification failed for prompts: %v", prompts)
			}
			for i, p := range prompts {
				if !utf8.ValidString(p) {
					t.Errorf("prompt[%d] = %q is not valid UTF-8", i, p)
				}
			}
		})
	}
}

// TestSplitParallelPrompts_MixedUnicodeASCII tests combinations of ASCII and
// Unicode content within prompts and across delimiter boundaries. Verifies that
// mixed scripts, emoji embedded in ASCII text, and alternating patterns are
// handled correctly through splitting.
func TestSplitParallelPrompts_MixedUnicodeASCII(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		wantPrompts []string
		verify      func([]string) bool
	}{
		// ── ASCII with embedded Unicode characters ─────────────────────────────
		{
			name:        "ASCII text with embedded accented characters",
			input:       "Check the café and résumé\n---\nNext task about naïve",
			wantLen:     2,
			wantPrompts: []string{"Check the café and résumé", "Next task about naïve"},
		},
		{
			name:        "ASCII with currency symbols",
			input:       "Price: $100 or €90\n---\nTotal: £50 + ¥1000",
			wantLen:     2,
			wantPrompts: []string{"Price: $100 or €90", "Total: £50 + ¥1000"},
		},
		{
			name:        "ASCII with mathematical symbols",
			input:       "Use ∑ for sum, √ for root\n---\nCalculate ∞ + ∆",
			wantLen:     2,
			wantPrompts: []string{"Use ∑ for sum, √ for root", "Calculate ∞ + ∆"},
		},
		{
			name:        "ASCII with punctuation symbols",
			input:       "Question: What is this?\n---\nAnswer: §©®™ elements",
			wantLen:     2,
			wantPrompts: []string{"Question: What is this?", "Answer: §©®™ elements"},
		},
		// ── ASCII with embedded emoji ────────────────────────────────────────────
		{
			name:        "ASCII sentence with emoji at end",
			input:       "This is great! 🎉\n---\nThat works well 👍",
			wantLen:     2,
			wantPrompts: []string{"This is great! 🎉", "That works well 👍"},
		},
		{
			name:        "ASCII with emoji in the middle",
			input:       "The 🚀 ship has sailed\n---\nA 🐛 bug was found",
			wantLen:     2,
			wantPrompts: []string{"The 🚀 ship has sailed", "A 🐛 bug was found"},
		},
		{
			name:        "ASCII with multiple emoji",
			input:       "Test ✅ the code 🧪 now 🏃‍♂️\n---\nShip 📦 the build 🛠️ fast",
			wantLen:     2,
			wantPrompts: []string{"Test ✅ the code 🧪 now 🏃‍♂️", "Ship 📦 the build 🛠️ fast"},
		},
		{
			name:        "ASCII technical terms with emoji",
			input:       "Run npm install 📦 then npm test 🧪\n---\nDeploy 🚀 to production 🌍",
			wantLen:     2,
			wantPrompts: []string{"Run npm install 📦 then npm test 🧪", "Deploy 🚀 to production 🌍"},
		},
		{
			name:        "ASCII with emoji-only word replacements",
			input:       "I ❤️ programming\n---\nShe 👩‍💻 codes",
			wantLen:     2,
			wantPrompts: []string{"I ❤️ programming", "She 👩‍💻 codes"},
		},
		// ── Multiple scripts in single prompt ────────────────────────────────────
		{
			name:        "ASCII, Japanese, and emoji in one prompt",
			input:       "Check 日本語 🗾 the data\n---\nNext task",
			wantLen:     2,
			wantPrompts: []string{"Check 日本語 🗾 the data", "Next task"},
		},
		{
			name:        "ASCII, Chinese, and Cyrillic mixed",
			input:       "Test 测试 the code\n---\nNext функцию task",
			wantLen:     2,
			wantPrompts: []string{"Test 测试 the code", "Next функцию task"},
		},
		{
			name:        "ASCII, Arabic, and Greek in one line",
			input:       "Hello مرحبا world\n---\nNext Γεια σου task",
			wantLen:     2,
			wantPrompts: []string{"Hello مرحبا world", "Next Γεια σου task"},
		},
		{
			name:        "Four scripts alternating in one prompt",
			input:       "A 日本 test 测试\n---\nNext مرحبا Γεια",
			wantLen:     2,
			wantPrompts: []string{"A 日本 test 测试", "Next مرحبا Γεια"},
		},
		{
			name:        "ASCII with emoji and multiple scripts",
			input:       "Start 🚀 with 日本 🗾\n---\nAdd 测试 and 🇨🇳",
			wantLen:     2,
			wantPrompts: []string{"Start 🚀 with 日本 🗾", "Add 测试 and 🇨🇳"},
		},
		{
			name:        "technical ASCII with CJK and emoji",
			input:       "Run `git commit` and check 日本 🗾\n---\nDeploy to 测试 🇨🇳 server",
			wantLen:     2,
			wantPrompts: []string{"Run `git commit` and check 日本 🗾", "Deploy to 测试 🇨🇳 server"},
		},
		// ── Mixed content across delimiter boundaries ────────────────────────────
		{
			name:        "Unicode ends first prompt, ASCII starts second",
			input:       "テスト ends\n---\nASCII starts",
			wantLen:     2,
			wantPrompts: []string{"テスト ends", "ASCII starts"},
		},
		{
			name:        "ASCII ends first prompt, Unicode starts second",
			input:       "ASCII ends\n---\n日本語 starts",
			wantLen:     2,
			wantPrompts: []string{"ASCII ends", "日本語 starts"},
		},
		{
			name:        "Emoji at delimiter boundary",
			input:       "End 🚀\n---\n🔥 Start",
			wantLen:     2,
			wantPrompts: []string{"End 🚀", "🔥 Start"},
		},
		{
			name:        "Mixed scripts at delimiter boundaries",
			input:       "A 日本 B\n---\nC 测试 D",
			wantLen:     2,
			wantPrompts: []string{"A 日本 B", "C 测试 D"},
		},
		{
			name:        "ASCII-emoji-ASCII pattern across delimiter",
			input:       "Test 🚀 code\n---\nShip 📦 now",
			wantLen:     2,
			wantPrompts: []string{"Test 🚀 code", "Ship 📦 now"},
		},
		{
			name:        "ZWJ sequence at delimiter boundary",
			input:       "Family 👨‍👩‍👧‍👦\n---\nNext 👩‍💻 task",
			wantLen:     2,
			wantPrompts: []string{"Family 👨‍👩‍👧‍👦", "Next 👩‍💻 task"},
		},
		{
			name:        "Multiple alternating patterns across boundaries",
			input:       "ASCII 日本 🚀 test\n---\nNext 测试 🔥 code",
			wantLen:     2,
			wantPrompts: []string{"ASCII 日本 🚀 test", "Next 测试 🔥 code"},
		},
		{
			name:        "Unicode-ASCII-Unicode sandwich pattern",
			input:       "日本 ASCII テスト\n---\nNext word 文字",
			wantLen:     2,
			wantPrompts: []string{"日本 ASCII テスト", "Next word 文字"},
		},
		// ── Complex real-world mixed scenarios ───────────────────────────────────
		{
			name:        "international greeting message",
			input:       "Hello! 🌍 Bonjour! 🇫🇷\n---\nKonnichiwa! 🇯🇵 Nihao! 🇨🇳",
			wantLen:     2,
			wantPrompts: []string{"Hello! 🌍 Bonjour! 🇫🇷", "Konnichiwa! 🇯🇵 Nihao! 🇨🇳"},
		},
		{
			name:        "technical documentation with code examples",
			input:       "Use `npm install` for dependencies 📦\n---\nCheck the 日本 README for setup 🇯🇵",
			wantLen:     2,
			wantPrompts: []string{"Use `npm install` for dependencies 📦", "Check the 日本 README for setup 🇯🇵"},
		},
		{
			name:        "multiline prompt with mixed content",
			input:       "Line 1: ASCII\nLine 2: 日本語 🇯🇵\n---\nNext task starts here",
			wantLen:     2,
			wantPrompts: []string{"Line 1: ASCII\nLine 2: 日本語 🇯🇵", "Next task starts here"},
		},
		{
			name:        "code comments in multiple languages",
			input:       "// This is a test comment 日本語\n---\n// Next line 测试 emoji 🚀",
			wantLen:     2,
			wantPrompts: []string{"// This is a test comment 日本語", "// Next line 测试 emoji 🚀"},
		},
		{
			name:        "international deployment pipeline",
			input:       "Deploy to us-east-1 🇺🇸 region\n---\nCheck 日本 🇯🇵 and 测试 🇨🇳 deployments",
			wantLen:     2,
			wantPrompts: []string{"Deploy to us-east-1 🇺🇸 region", "Check 日本 🇯🇵 and 测试 🇨🇳 deployments"},
		},
		// ── Edge cases with mixed content ────────────────────────────────────────
		{
			name:        "single ASCII character with emoji delimiter boundary",
			input:       "A 🚀\n---\n🔥 B",
			wantLen:     2,
			wantPrompts: []string{"A 🚀", "🔥 B"},
		},
		{
			name:        "mixed content with whitespace trimming",
			input:       "  ASCII 日本 🚀  \n---\n  测试 emoji 🔥  ",
			wantLen:     2,
			wantPrompts: []string{"ASCII 日本 🚀", "测试 emoji 🔥"},
		},
		{
			name:        "mixed RTL and LTR scripts",
			input:       "Hello مرحبا World\n---\nNext שלום test",
			wantLen:     2,
			wantPrompts: []string{"Hello مرحبا World", "Next שלום test"},
			verify: func(prompts []string) bool {
				// Verify RTL content is preserved
				return strings.Contains(prompts[0], "مرحبا") && strings.Contains(prompts[1], "שלום")
			},
		},
		{
			name:        "emoji skin tone with ASCII",
			input:        "Team 👨🏽‍💻 reviews code\n---\nManager 👩🏿‍💻 approves",
			wantLen:     2,
			wantPrompts: []string{"Team 👨🏽‍💻 reviews code", "Manager 👩🏿‍💻 approves"},
		},
		{
			name:        "flag emoji with country names",
			input:       "Visit Japan 🇯🇵 and China 🇨🇳\n---\nThen France 🇫🇷 and Germany 🇩🇪",
			wantLen:     2,
			wantPrompts: []string{"Visit Japan 🇯🇵 and China 🇨🇳", "Then France 🇫🇷 and Germany 🇩🇪"},
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
			if tt.verify != nil && !tt.verify(prompts) {
				t.Errorf("verification failed for prompts: %v", prompts)
			}
			// Verify UTF-8 validity for all mixed content
			for i, p := range prompts {
				if !utf8.ValidString(p) {
					t.Errorf("prompt[%d] = %q is not valid UTF-8", i, p)
				}
				if strings.ContainsRune(p, utf8.RuneError) {
					t.Errorf("prompt[%d] = %q contains the replacement character U+FFFD", i, p)
				}
			}
		})
	}
}
