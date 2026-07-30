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
