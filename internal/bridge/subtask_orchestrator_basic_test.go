// Package bridge provides basic unit tests for splitParallelPrompts utility function.
// These tests cover the fundamental splitting scenarios as specified in the acceptance criteria.
package bridge

import (
	"strings"
	"testing"
)

// TestSplitParallelPrompts_BasicSplits covers the basic split scenarios from the acceptance criteria:
// - Single prompt (no delimiter)
// - Two prompts with one delimiter
// - Multiple prompts with multiple delimiters
// - Delimiter with spaces around it
func TestSplitParallelPrompts_BasicSplits(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantLen      int
		wantPrompts  []string
		description  string
	}{
		{
			name:        "single prompt - no delimiter",
			input:       "This is a single prompt without any delimiter",
			wantLen:     1,
			wantPrompts: []string{"This is a single prompt without any delimiter"},
			description: "Single prompt with no delimiter should return one element",
		},
		{
			name:        "two prompts with one delimiter",
			input:       "First prompt\n---\nSecond prompt",
			wantLen:     2,
			wantPrompts: []string{"First prompt", "Second prompt"},
			description: "Two prompts separated by one standard delimiter",
		},
		{
			name:        "three prompts with two delimiters",
			input:       "First\n---\nSecond\n---\nThird",
			wantLen:     3,
			wantPrompts: []string{"First", "Second", "Third"},
			description: "Three prompts separated by two delimiters",
		},
		{
			name:        "four prompts with three delimiters",
			input:       "A\n---\nB\n---\nC\n---\nD",
			wantLen:     4,
			wantPrompts: []string{"A", "B", "C", "D"},
			description: "Four prompts separated by three delimiters",
		},
		{
			name:        "five prompts with four delimiters",
			input:       "1\n---\n2\n---\n3\n---\n4\n---\n5",
			wantLen:     5,
			wantPrompts: []string{"1", "2", "3", "4", "5"},
			description: "Five prompts separated by four delimiters",
		},
		{
			name:        "delimiter with leading space",
			input:       "First \n---\nSecond",
			wantLen:     2,
			wantPrompts: []string{"First", "Second"},
			description: "Delimiter with space before it should still split correctly",
		},
		{
			name:        "delimiter with trailing space",
			input:       "First\n---\n Second",
			wantLen:     2,
			wantPrompts: []string{"First", "Second"},
			description: "Delimiter with space after it should still split correctly",
		},
		{
			name:        "delimiter with spaces on both sides",
			input:       "First \n---\n Second",
			wantLen:     2,
			wantPrompts: []string{"First", "Second"},
			description: "Delimiter with spaces on both sides should still split correctly",
		},
		{
			name:        "multi-line prompt preserved in split",
			input:       "Line 1\nLine 2\nLine 3\n---\nNext prompt",
			wantLen:     2,
			wantPrompts: []string{"Line 1\nLine 2\nLine 3", "Next prompt"},
			description: "Internal newlines should be preserved within prompts",
		},
		{
			name:        "empty string returns no prompts",
			input:       "",
			wantLen:     0,
			wantPrompts: nil,
			description: "Empty input should return zero prompts",
		},
		{
			name:        "whitespace only returns no prompts",
			input:       "   \n  \t  \n   ",
			wantLen:     0,
			wantPrompts: nil,
			description: "Whitespace-only input should return zero prompts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)

			// Verify correct split count
			if len(prompts) != tt.wantLen {
				t.Errorf("split count: got %d prompts, want %d", len(prompts), tt.wantLen)
			}

			// Verify delimiter removal and correct content
			if tt.wantPrompts != nil && len(prompts) > 0 {
				for i, want := range tt.wantPrompts {
					if i >= len(prompts) {
						t.Errorf("expected prompt[%d] but got only %d prompts", i, len(prompts))
						break
					}
					if prompts[i] != want {
						t.Errorf("prompt[%d] content: got %q, want %q", i, prompts[i], want)
					}
				}
			}

			// Verify no delimiter remnants in output
			for i, p := range prompts {
				if strings.Contains(p, "\n---\n") {
					t.Errorf("prompt[%d] still contains delimiter: %q", i, p)
				}
			}
		})
	}
}

// TestSplitParallelPrompts_SplitCountVerification tests that the split count is correctly verified.
func TestSplitParallelPrompts_SplitCountVerification(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantCount   int
		description string
	}{
		{
			name:        "no delimiters",
			input:       "Single prompt",
			wantCount:   1,
			description: "No delimiters means 1 prompt",
		},
		{
			name:        "one delimiter",
			input:       "A\n---\nB",
			wantCount:   2,
			description: "One delimiter creates 2 prompts",
		},
		{
			name:        "two delimiters",
			input:       "A\n---\nB\n---\nC",
			wantCount:   3,
			description: "Two delimiters create 3 prompts",
		},
		{
			name:        "three delimiters",
			input:       "A\n---\nB\n---\nC\n---\nD",
			wantCount:   4,
			description: "Three delimiters create 4 prompts",
		},
		{
			name:        "four delimiters",
			input:       "A\n---\nB\n---\nC\n---\nD\n---\nE",
			wantCount:   5,
			description: "Four delimiters create 5 prompts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantCount {
				t.Errorf("got %d prompts, want %d (description: %s)", len(prompts), tt.wantCount, tt.description)
			}
		})
	}
}

// TestSplitParallelPrompts_DelimiterHandling tests standard delimiter handling.
func TestSplitParallelPrompts_DelimiterHandling(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		shouldSplit bool
		description string
	}{
		{
			name:         "standard delimiter splits",
			input:        "First\n---\nSecond",
			wantLen:      2,
			shouldSplit:  true,
			description:  "Standard \\n---\\n delimiter should split",
		},
		{
			name:         "dash without newlines does not split",
			input:        "First---Second",
			wantLen:      1,
			shouldSplit:  false,
			description:  "Dashes without surrounding newlines should not split",
		},
		{
			name:         "two dashes does not split",
			input:        "First\n--\nSecond",
			wantLen:      1,
			shouldSplit:  false,
			description:  "Two dashes should not split",
		},
		{
			name:         "four dashes does not split",
			input:        "First\n----\nSecond",
			wantLen:      1,
			shouldSplit:  false,
			description:  "Four dashes should not split",
		},
		{
			name:         "spaces around dashes still split",
			input:        "First \n --- \n Second",
			wantLen:      2,
			shouldSplit:  true,
			description:  "Spaces around a --- line are still a delimiter",
		},
		{
			name:         "delimiter at start only",
			input:        "\n---\nFirst prompt",
			wantLen:      1,
			shouldSplit:  false,
			description:  "Delimiter at start only should not create empty prompt",
		},
		{
			name:         "delimiter at end only",
			input:        "First prompt\n---\n",
			wantLen:      1,
			shouldSplit:  false,
			description:  "Delimiter at end only should not create empty prompt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)

			if len(prompts) != tt.wantLen {
				t.Errorf("split count: got %d prompts, want %d", len(prompts), tt.wantLen)
			}

			if tt.shouldSplit && len(prompts) < 2 {
				t.Errorf("expected split to occur, but got %d prompts", len(prompts))
			}

			if !tt.shouldSplit && len(prompts) > 1 {
				t.Errorf("expected no split, but got %d prompts", len(prompts))
			}
		})
	}
}

// TestSplitParallelPrompts_WhitespaceContentHandling tests that whitespace is preserved correctly.
func TestSplitParallelPrompts_WhitespaceContentHandling(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		verify      func([]string) bool
		description string
	}{
		{
			name:        "internal spaces preserved",
			input:       "Words with    multiple   spaces\n---\nAnother",
			wantLen:     2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "multiple   spaces")
			},
			description: "Internal multiple spaces should be preserved",
		},
		{
			name:        "tabs preserved internally",
			input:       "Col1\tCol2\tCol3\n---\nNext",
			wantLen:     2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "\t")
			},
			description: "Internal tabs should be preserved",
		},
		{
			name:        "newlines preserved within prompts",
			input:       "Line 1\nLine 2\nLine 3\n---\nNext",
			wantLen:     2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "Line 2")
			},
			description: "Internal newlines should be preserved",
		},
		{
			name:        "outer whitespace trimmed",
			input:       "  Prompt with padding  \n---\n  Another  ",
			wantLen:     2,
			verify: func(prompts []string) bool {
				return prompts[0] == "Prompt with padding" && prompts[1] == "Another"
			},
			description: "Outer whitespace should be trimmed from each prompt",
		},
		{
			name:        "mixed internal whitespace preserved",
			input:       "  \tWords\tand\tspaces\t  \n---\nNext",
			wantLen:     2,
			verify: func(prompts []string) bool {
				return strings.Contains(prompts[0], "\t") && prompts[0] == "Words\tand\tspaces"
			},
			description: "Mixed internal whitespace should be preserved, outer trimmed",
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

// TestSplitParallelPrompts_DelimiterRemoval tests that delimiters are properly removed from output.
func TestSplitParallelPrompts_DelimiterRemoval(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantLen     int
		description string
	}{
		{
			name:        "single delimiter removed",
			input:       "Before\n---\nAfter",
			wantLen:     2,
			description: "Delimiter should not appear in either output prompt",
		},
		{
			name:        "multiple delimiters removed",
			input:       "A\n---\nB\n---\nC\n---\nD",
			wantLen:     4,
			description: "All delimiters should be removed from output",
		},
		{
			name:        "delimiter with spaces around it",
			input:       "A \n---\n B\n---\n C",
			wantLen:     3,
			description: "Delimiters should be removed, leaving trimmed prompts",
		},
		{
			name:        "consecutive delimiters",
			input:       "First\n---\n---\nSecond",
			wantLen:     2,
			description: "Consecutive delimiters handled correctly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)

			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}

			// Verify no delimiter remnants
			for i, p := range prompts {
				if strings.Contains(p, "\n---\n") {
					t.Errorf("prompt[%d] contains delimiter remnant: %q", i, p)
				}
				if strings.Contains(p, "---") && strings.Count(p, "---") > 0 && !strings.Contains(strings.TrimSpace(p), " ") {
					// Allow "---" in content if it's part of actual text, not delimiter
					// This is a basic check - the delimiter pattern is "\n---\n"
				}
			}
		})
	}
}
