package telegram

import (
	"strings"
)

// MarkdownToHTML converts Claude's markdown output to Telegram-compatible HTML.
//
// Supported conversions:
//   - **bold** → <b>bold</b>
//   - *italic* → <i>italic</i>
//   - `code` → <code>code</code>
//   - ```lang\ncode\n``` → <pre><code class="language-X">code</code></pre>
//   - [text](url) → <a href="url">text</a>
//   - ~~strikethrough~~ → <s>strikethrough</s>
//   - > blockquote → <blockquote>text</blockquote>
//   - Plain text: <, >, & are escaped to &lt; &gt; &amp;
//
// Unmatched inline markers are emitted as literal text.
// Inside code fences only HTML entity escaping is applied; markdown is not processed.
func MarkdownToHTML(md string) string {
	var out strings.Builder
	i := 0
	n := len(md)

	for i < n {
		isLineStart := i == 0 || md[i-1] == '\n'

		if isLineStart {
			// Code fence opening: ``` optionally followed by language tag.
			if strings.HasPrefix(md[i:], "```") {
				i = processFence(&out, md, i)
				continue
			}

			// Blockquote: "> " or lone ">" at end-of-line.
			if strings.HasPrefix(md[i:], "> ") {
				end := lineEnd(md, i)
				out.WriteString("<blockquote>")
				out.WriteString(convertInline(md[i+2 : end]))
				out.WriteString("</blockquote>")
				i = end
				continue
			}
			if i+1 <= n && md[i] == '>' && (i+1 == n || md[i+1] == '\n') {
				out.WriteString("<blockquote></blockquote>")
				i++
				continue
			}
		}

		// Regular content: process until end of line.
		end := lineEnd(md, i)
		out.WriteString(convertInline(md[i:end]))
		i = end

		// Emit the newline that terminated this line (if any).
		if i < n && md[i] == '\n' {
			out.WriteByte('\n')
			i++
		}
	}

	return out.String()
}

// processFence handles a fenced code block starting at md[start] (which must be "```").
// It emits <pre><code ...>content</code></pre> and returns the position immediately
// after the closing fence line (i.e. pointing at the '\n' that follows "```").
// On an unclosed fence, it emits the raw remainder and returns len(md).
func processFence(out *strings.Builder, md string, start int) int {
	// Parse language tag from the opening line.
	openEnd := lineEnd(md, start)
	lang := strings.TrimSpace(md[start+3 : openEnd])

	// Advance past the opening line + its newline.
	i := openEnd
	if i < len(md) && md[i] == '\n' {
		i++
	}

	// Scan for the closing fence: a line whose trimmed content is exactly "```".
	codeStart := i
	for i < len(md) {
		end := lineEnd(md, i)
		if strings.TrimRight(md[i:end], " \t") == "```" {
			// Found the closing fence.
			codeContent := md[codeStart:i]
			// Strip the single trailing newline that separates last code line from "```".
			if len(codeContent) > 0 && codeContent[len(codeContent)-1] == '\n' {
				codeContent = codeContent[:len(codeContent)-1]
			}

			out.WriteString("<pre><code")
			if lang != "" {
				out.WriteString(` class="language-`)
				out.WriteString(escapeHTML(lang))
				out.WriteByte('"')
			}
			out.WriteByte('>')
			out.WriteString(escapeHTML(codeContent))
			out.WriteString("</code></pre>")

			// Return pointing at the '\n' after "```" (or EOF).
			return end
		}
		i = end
		if i < len(md) && md[i] == '\n' {
			i++
		}
	}

	// Unclosed fence: emit the raw remainder verbatim (fault-tolerant).
	out.WriteString(escapeHTML(md[start:]))
	return len(md)
}

// convertInline converts inline markdown in s to HTML.
// HTML entity escaping is applied to all text outside of inline code spans.
func convertInline(s string) string {
	if s == "" {
		return ""
	}
	var out strings.Builder
	i := 0
	for i < len(s) {
		b := s[i]

		switch {
		// HTML entity escaping.
		case b == '&':
			out.WriteString("&amp;")
			i++

		case b == '<':
			out.WriteString("&lt;")
			i++

		case b == '>':
			out.WriteString("&gt;")
			i++

		// Bold-italic: ***text***
		case b == '*' && i+2 < len(s) && s[i+1] == '*' && s[i+2] == '*':
			if j := findClose(s, i+3, "***", false); j >= 0 {
				out.WriteString("<b><i>")
				out.WriteString(convertInline(s[i+3 : j]))
				out.WriteString("</i></b>")
				i = j + 3
			} else {
				out.WriteString("***")
				i += 3
			}

		// Bold: **text**
		case b == '*' && i+1 < len(s) && s[i+1] == '*':
			if j := findClose(s, i+2, "**", false); j >= 0 && j > i+2 {
				out.WriteString("<b>")
				out.WriteString(convertInline(s[i+2 : j]))
				out.WriteString("</b>")
				i = j + 2
			} else {
				out.WriteString("**")
				i += 2
			}

		// Italic: *text* (single asterisk, not ** or ***)
		case b == '*':
			if j := findClose(s, i+1, "*", true); j >= 0 && j > i+1 {
				out.WriteString("<i>")
				out.WriteString(convertInline(s[i+1 : j]))
				out.WriteString("</i>")
				i = j + 1
			} else {
				out.WriteByte('*')
				i++
			}

		// Strikethrough: ~~text~~
		case b == '~' && i+1 < len(s) && s[i+1] == '~':
			if j := findClose(s, i+2, "~~", false); j >= 0 {
				out.WriteString("<s>")
				out.WriteString(convertInline(s[i+2 : j]))
				out.WriteString("</s>")
				i = j + 2
			} else {
				out.WriteString("~~")
				i += 2
			}

		// Inline code: `text`
		case b == '`':
			if j := findClose(s, i+1, "`", true); j >= 0 {
				out.WriteString("<code>")
				out.WriteString(escapeHTML(s[i+1 : j]))
				out.WriteString("</code>")
				i = j + 1
			} else {
				out.WriteByte('`')
				i++
			}

		// Link: [text](url)
		case b == '[':
			text, url, end := parseLink(s, i)
			if end > i {
				out.WriteString(`<a href="`)
				out.WriteString(escapeAttr(url))
				out.WriteString(`">`)
				out.WriteString(convertInline(text))
				out.WriteString("</a>")
				i = end
			} else {
				out.WriteByte('[')
				i++
			}

		default:
			out.WriteByte(b)
			i++
		}
	}
	return out.String()
}

// findClose searches for the closing marker in s starting at byte position start.
// If avoidDoubled is true (for single-char markers like * and `), a match is
// rejected if the same character immediately precedes or follows it — this
// prevents matching the * inside ** when looking for italic close.
// Returns the absolute byte position of the marker, or -1 if not found.
func findClose(s string, start int, marker string, avoidDoubled bool) int {
	ml := len(marker)
	for i := start; i <= len(s)-ml; i++ {
		if s[i:i+ml] != marker {
			continue
		}
		if avoidDoubled && ml == 1 {
			before := i > 0 && s[i-1] == marker[0]
			after := i+1 < len(s) && s[i+1] == marker[0]
			if before || after {
				continue
			}
		}
		return i
	}
	return -1
}

// parseLink attempts to parse a Markdown link [text](url) at s[start].
// Returns (text, url, end) where end is the byte position just past ')'.
// On failure returns ("", "", start).
// Only http and https URLs are allowed; other schemes are rejected for safety.
func parseLink(s string, start int) (text, url string, end int) {
	if start >= len(s) || s[start] != '[' {
		return "", "", start
	}

	// Find ']' that closes the link text.
	closeText := strings.IndexByte(s[start+1:], ']')
	if closeText < 0 {
		return "", "", start
	}
	closeText += start + 1 // absolute position of ']'

	// Must be followed by '('.
	if closeText+1 >= len(s) || s[closeText+1] != '(' {
		return "", "", start
	}

	// Find ')' that closes the URL.
	closeURL := strings.IndexByte(s[closeText+2:], ')')
	if closeURL < 0 {
		return "", "", start
	}
	closeURL += closeText + 2 // absolute position of ')'

	rawURL := s[closeText+2 : closeURL]
	if !isSafeURL(rawURL) {
		return "", "", start
	}

	return s[start+1 : closeText], rawURL, closeURL + 1
}

// isSafeURL returns true only for http and https URLs.
func isSafeURL(url string) bool {
	lower := strings.ToLower(url)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// escapeHTML escapes &, < and > for use in HTML text content.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// escapeAttr escapes &, <, > and " for use in an HTML attribute value.
func escapeAttr(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

// lineEnd returns the byte position of the '\n' that terminates the line
// starting at s[start], or len(s) if no newline is found.
func lineEnd(s string, start int) int {
	idx := strings.IndexByte(s[start:], '\n')
	if idx < 0 {
		return len(s)
	}
	return start + idx
}
