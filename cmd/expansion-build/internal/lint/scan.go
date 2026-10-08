// Package lint holds the static design lints expansion-build runs over a pack's
// UI sources. The lints are text scans, not parsers: they skip comments, accept
// a few missed exotic cases, and aim for no false positives.
package lint

import "strings"

// Span is the content of one string literal and the line it starts on.
type Span struct {
	Text string // content without surrounding quotes
	Line int    // 1-based
}

// Scan returns the code text with comments blanked to spaces (newlines kept, so
// offsets and line numbers line up), and the contents of every string literal.
// CSS files (.css) only know /* */ comments. String literals stay in the
// returned code unchanged.
func Scan(path, text string) (string, []Span) {
	css := strings.HasSuffix(strings.ToLower(path), ".css")
	b := []byte(text)
	var strs []Span
	line := 1
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			j := i + 2
			for j+1 < len(b) && !(b[j] == '*' && b[j+1] == '/') {
				j++
			}
			end := min(j+2, len(b))
			for k := i; k < end; k++ {
				if b[k] == '\n' {
					line++
				} else {
					b[k] = ' '
				}
			}
			i = end
		case !css && c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				b[i] = ' '
				i++
			}
		case c == '"' || c == '\'' || (c == '`' && !css):
			start, startLine := i+1, line
			j := start
			for j < len(b) && b[j] != c {
				if b[j] == '\\' && j+1 < len(b) {
					j++ // an escaped character, including a line continuation
				} else if b[j] == '\n' && c != '`' {
					break // a raw newline ends '...' and "..." (a stray apostrophe in JSX text)
				}
				if b[j] == '\n' {
					line++
				}
				j++
			}
			end := min(j, len(b))
			strs = append(strs, Span{Text: string(b[start:end]), Line: startLine})
			if end < len(b) && b[end] == c {
				end++
			}
			i = end
		default:
			i++
		}
	}
	return string(b), strs
}
