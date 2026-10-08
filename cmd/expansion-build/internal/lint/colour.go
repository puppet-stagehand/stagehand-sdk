package lint

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/puppet-stagehand/stagehand-sdk/manifest"
)

var (
	reHex      = `#(?:[0-9a-fA-F]{8}|[0-9a-fA-F]{6}|[0-9a-fA-F]{3,4})\b`
	reHexWhole = regexp.MustCompile(`^\s*` + reHex + `\s*$`)
	reHexCtx   = regexp.MustCompile(`[:(,]\s*` + reHex)
	reHexAny   = regexp.MustCompile(reHex)
	reColourFn = regexp.MustCompile(`\b(?:rgb|rgba|hsl|hsla)\s*\(`)
)

func colourFinding(path string, line int, what string) manifest.Finding {
	return manifest.Finding{
		Code:    "ui_lint_hex_colour",
		Path:    fmt.Sprintf("%s:%d", path, line),
		Message: "raw colour literal " + what,
		Fix:     "Use a theme token from @stagehand/console-ui (a var(--...) reference); never a hex, rgb(), rgba(), hsl() or hsla() literal.",
	}
}

func lintColour(path, text string) []manifest.Finding {
	code, strs := Scan(path, text)
	if strings.HasSuffix(strings.ToLower(path), ".css") {
		return cssColours(path, code)
	}
	var out []manifest.Finding
	for _, s := range strs {
		switch {
		case reHexWhole.MatchString(s.Text):
			out = append(out, colourFinding(path, s.Line, strings.TrimSpace(s.Text)))
		case reHexCtx.MatchString(s.Text):
			out = append(out, colourFinding(path, s.Line, reHexAny.FindString(s.Text)))
		case reColourFn.MatchString(s.Text):
			out = append(out, colourFinding(path, s.Line, reColourFn.FindString(s.Text)+"...)"))
		}
	}
	return out
}

// cssColours looks only inside { } blocks so id selectors such as #add pass.
func cssColours(path, code string) []manifest.Finding {
	var out []manifest.Finding
	depth, line := 0, 1
	for i := 0; i < len(code); i++ {
		switch code[i] {
		case '\n':
			line++
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				continue
			}
			if code[i] == '#' {
				if m := reHexAny.FindString(code[i:]); m != "" && strings.HasPrefix(code[i:], m) {
					out = append(out, colourFinding(path, line, m))
				}
			} else if loc := reColourFn.FindStringIndex(code[i:]); loc != nil && loc[0] == 0 && (i == 0 || !isWord(code[i-1])) {
				out = append(out, colourFinding(path, line, reColourFn.FindString(code[i:])+"...)"))
			}
		}
	}
	return out
}

func isWord(c byte) bool {
	return c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
