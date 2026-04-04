package telegram

import (
	"testing"
)

// TestMarkdownToHTML_PlainText verifies HTML entity escaping in plain text.
func TestMarkdownToHTML_PlainText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no-op", "Hello, world!", "Hello, world!"},
		{"ampersand", "AT&T", "AT&amp;T"},
		{"less-than", "x < y", "x &lt; y"},
		{"greater-than", "x > y", "x &gt; y"},
		{"all-three", "<b>&amp;</b>", "&lt;b&gt;&amp;amp;&lt;/b&gt;"},
		{"multiline", "line1\nline2", "line1\nline2"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToHTML(tt.input)
			if got != tt.want {
				t.Errorf("MarkdownToHTML(%q)\ngot:  %q\nwant: %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestMarkdownToHTML_Bold tests **bold** conversion.
func TestMarkdownToHTML_Bold(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"basic", "**bold**", "<b>bold</b>"},
		{"in-sentence", "This is **important** text.", "This is <b>important</b> text."},
		{"multiple", "**a** and **b**", "<b>a</b> and <b>b</b>"},
		{"with-html-inside", "**AT&T**", "<b>AT&amp;T</b>"},
		{"unmatched-passthrough", "**unclosed", "**unclosed"},
		{"empty-markers-passthrough", "****", "****"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToHTML(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMarkdownToHTML_Italic tests *italic* conversion.
func TestMarkdownToHTML_Italic(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"basic", "*italic*", "<i>italic</i>"},
		{"in-sentence", "Use *emphasis* here.", "Use <i>emphasis</i> here."},
		{"unmatched-passthrough", "*unclosed", "*unclosed"},
		{"multiple", "*a* and *b*", "<i>a</i> and <i>b</i>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToHTML(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMarkdownToHTML_BoldItalic tests nested bold+italic (***text***).
func TestMarkdownToHTML_BoldItalic(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"triple-asterisk", "***bold-italic***", "<b><i>bold-italic</i></b>"},
		{"unmatched-triple", "***unclosed", "***unclosed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToHTML(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMarkdownToHTML_Strikethrough tests ~~strikethrough~~ conversion.
func TestMarkdownToHTML_Strikethrough(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"basic", "~~strike~~", "<s>strike</s>"},
		{"in-sentence", "The ~~old~~ new approach.", "The <s>old</s> new approach."},
		{"unmatched-passthrough", "~~unclosed", "~~unclosed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToHTML(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMarkdownToHTML_InlineCode tests `code` conversion.
func TestMarkdownToHTML_InlineCode(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"basic", "`code`", "<code>code</code>"},
		{"in-sentence", "Run `go build` to compile.", "Run <code>go build</code> to compile."},
		{"html-entities-escaped", "`<div>`", "<code>&lt;div&gt;</code>"},
		{"unmatched-passthrough", "`unclosed", "`unclosed"},
		// Inline code should not process markdown inside it.
		{"no-inner-markdown", "`**not bold**`", "<code>**not bold**</code>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToHTML(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMarkdownToHTML_CodeFence tests fenced code block conversion.
func TestMarkdownToHTML_CodeFence(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "with-language",
			input: "```go\nfmt.Println(\"hello\")\n```",
			want:  `<pre><code class="language-go">fmt.Println("hello")</code></pre>`,
		},
		{
			name:  "without-language",
			input: "```\nsome code\n```",
			want:  "<pre><code>some code</code></pre>",
		},
		{
			name:  "html-entities-in-code",
			input: "```\n<div>&amp;</div>\n```",
			want:  "<pre><code>&lt;div&gt;&amp;amp;&lt;/div&gt;</code></pre>",
		},
		{
			name:  "no-markdown-processing-inside",
			input: "```\n**not bold** and *not italic*\n```",
			want:  "<pre><code>**not bold** and *not italic*</code></pre>",
		},
		{
			name:  "multiline-code",
			input: "```python\ndef hello():\n    print(\"world\")\n```",
			want:  "<pre><code class=\"language-python\">def hello():\n    print(\"world\")</code></pre>",
		},
		{
			name:  "surrounded-by-text",
			input: "Before:\n```go\ncode\n```\nAfter.",
			want:  "Before:\n<pre><code class=\"language-go\">code</code></pre>\nAfter.",
		},
		{
			name:  "unclosed-fence-passthrough",
			input: "```go\nunclosed",
			want:  "```go\nunclosed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToHTML(tt.input)
			if got != tt.want {
				t.Errorf("MarkdownToHTML(%q)\ngot:  %q\nwant: %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestMarkdownToHTML_Link tests [text](url) conversion.
func TestMarkdownToHTML_Link(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "basic-https",
			input: "[Go docs](https://golang.org)",
			want:  `<a href="https://golang.org">Go docs</a>`,
		},
		{
			name:  "basic-http",
			input: "[example](http://example.com)",
			want:  `<a href="http://example.com">example</a>`,
		},
		{
			name:  "url-with-ampersand",
			input: "[search](https://example.com/?a=1&b=2)",
			want:  `<a href="https://example.com/?a=1&amp;b=2">search</a>`,
		},
		{
			name:  "javascript-blocked",
			input: "[evil](javascript:alert(1))",
			want:  "[evil](javascript:alert(1))",
		},
		{
			name:  "no-closing-paren-passthrough",
			input: "[text](https://example.com",
			want:  "[text](https://example.com",
		},
		{
			name:  "bold-text-in-link",
			input: "[**bold link**](https://example.com)",
			want:  `<a href="https://example.com"><b>bold link</b></a>`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToHTML(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMarkdownToHTML_Blockquote tests > blockquote conversion.
func TestMarkdownToHTML_Blockquote(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"basic", "> Note: important.", "<blockquote>Note: important.</blockquote>"},
		{"with-inline-markdown", "> Use **bold** here.", "<blockquote>Use <b>bold</b> here.</blockquote>"},
		{"empty", ">", "<blockquote></blockquote>"},
		{
			name:  "with-surrounding-text",
			input: "Before.\n> Quote\nAfter.",
			want:  "Before.\n<blockquote>Quote</blockquote>\nAfter.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToHTML(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMarkdownToHTML_RealClaudeOutput tests against realistic Claude response patterns.
func TestMarkdownToHTML_RealClaudeOutput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "summary-with-list",
			input: "**Summary**\n\nHere are the key points:\n- Run `go test ./...` to execute tests\n- Returns `*Error` when something goes **wrong**",
			want:  "<b>Summary</b>\n\nHere are the key points:\n- Run <code>go test ./...</code> to execute tests\n- Returns <code>*Error</code> when something goes <b>wrong</b>",
		},
		{
			name:  "function-explanation",
			input: "The `chunkText` function splits text at **paragraph breaks** (`\\n\\n`) first, then single newlines, then hard-cuts at 4096 characters.",
			want:  "The <code>chunkText</code> function splits text at <b>paragraph breaks</b> (<code>\\n\\n</code>) first, then single newlines, then hard-cuts at 4096 characters.",
		},
		{
			name: "code-block-with-explanation",
			input: "To install, run:\n\n```bash\ngo install github.com/foo/bar@latest\n```\n\nThis downloads and installs the binary.",
			want:  "To install, run:\n\n<pre><code class=\"language-bash\">go install github.com/foo/bar@latest</code></pre>\n\nThis downloads and installs the binary.",
		},
		{
			name:  "deprecation-notice",
			input: "~~Use oldFunc()~~ Use **newFunc()** instead.",
			want:  "<s>Use oldFunc()</s> Use <b>newFunc()</b> instead.",
		},
		{
			name:  "link-in-prose",
			input: "See the [official documentation](https://pkg.go.dev/fmt) for details.",
			want:  `See the <a href="https://pkg.go.dev/fmt">official documentation</a> for details.`,
		},
		{
			name: "mixed-formatting",
			input: "**Important**: Run `go vet ./...` to check for issues.\n> Note: always run tests before committing.",
			want:  "<b>Important</b>: Run <code>go vet ./...</code> to check for issues.\n<blockquote>Note: always run tests before committing.</blockquote>",
		},
		{
			name:  "html-in-output",
			input: "Avoid using `<script>` tags. Use `&amp;` for ampersands.",
			want:  "Avoid using <code>&lt;script&gt;</code> tags. Use <code>&amp;amp;</code> for ampersands.",
		},
		{
			name: "error-message-with-code",
			input: "The error `undefined: foo` means the variable **foo** was not declared in this scope.",
			want:  "The error <code>undefined: foo</code> means the variable <b>foo</b> was not declared in this scope.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToHTML(tt.input)
			if got != tt.want {
				t.Errorf("MarkdownToHTML(%q)\ngot:  %q\nwant: %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestMarkdownToHTML_FaultTolerance tests that unmatched/malformed markers pass through.
func TestMarkdownToHTML_FaultTolerance(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"unmatched-bold", "**bold text without close", "**bold text without close"},
		{"unmatched-italic", "*italic text without close", "*italic text without close"},
		{"unmatched-strike", "~~strike without close", "~~strike without close"},
		{"unmatched-code", "`code without close", "`code without close"},
		{"lone-asterisk", "price is $5 * 3 = $15", "price is $5 * 3 = $15"},
		{"lone-backtick", "press ` to open terminal", "press ` to open terminal"},
		{
			name:  "partial-bold-in-sentence",
			input: "The ** operator in Go",
			want:  "The ** operator in Go",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToHTML(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMarkdownToHTML_NewlinePreservation checks that newlines in the input are
// faithfully reproduced in the output.
func TestMarkdownToHTML_NewlinePreservation(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"trailing-newline", "hello\n", "hello\n"},
		{"no-trailing-newline", "hello", "hello"},
		{"blank-line", "a\n\nb", "a\n\nb"},
		{"multiple-blank-lines", "a\n\n\nb", "a\n\n\nb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToHTML(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
