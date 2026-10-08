package lint

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/puppet-stagehand/stagehand-sdk/manifest"
)

var (
	// CSS-style declarations: in .css files, and inside string literals elsewhere.
	reFamily = regexp.MustCompile(`(?i)font-family\s*:\s*([^;}\n]+)`)
	reWeight = regexp.MustCompile(`(?i)font-weight\s*:\s*["']?\s*(700|bold)\b`)
	// Object-style declarations in JS/TS; only a string-literal value is judged,
	// so `fontFamily: tokens.mono` and `fontFamily: string` are left alone.
	reObjFamily = regexp.MustCompile(`fontFamily\s*:\s*(["'` + "`" + `])([^"'` + "`" + `\n]*)["'` + "`" + `]`)
	reObjWeight = regexp.MustCompile(`fontWeight\s*:\s*["']?\s*(700|bold)\b`)

	genericFam = map[string]bool{"sans-serif": true, "serif": true, "monospace": true, "system-ui": true,
		"ui-monospace": true, "ui-sans-serif": true, "inherit": true, "initial": true, "unset": true, "revert": true}
	plexFam = map[string]bool{"ibm plex sans": true, "ibm plex mono": true}
)

func fontFinding(path string, line int, msg string) manifest.Finding {
	return manifest.Finding{
		Code: "ui_lint_font", Path: fmt.Sprintf("%s:%d", path, line), Message: msg,
		Fix: "Use IBM Plex Sans or IBM Plex Mono through the theme tokens (var(--font-brand), var(--font-mono)); weights are 400 and 600 only.",
	}
}

// familyFindings judges one font-family value (the text after the colon).
func familyFindings(path string, line int, val string) []manifest.Finding {
	val = strings.Trim(strings.TrimSpace(val), `"'`+"`,")
	if strings.HasPrefix(val, "var(") {
		return nil
	}
	var out []manifest.Finding
	for _, f := range strings.Split(val, ",") {
		f = strings.ToLower(strings.Trim(strings.TrimSpace(f), `"'`+"`"))
		if f == "" || genericFam[f] || plexFam[f] {
			continue
		}
		out = append(out, fontFinding(path, line, "font family "+f+" is not IBM Plex"))
	}
	return out
}

func declFindings(path string, line int, text string) []manifest.Finding {
	var out []manifest.Finding
	for _, m := range reFamily.FindAllStringSubmatch(text, -1) {
		out = append(out, familyFindings(path, line, m[1])...)
	}
	for _, m := range reWeight.FindAllStringSubmatch(text, -1) {
		out = append(out, fontFinding(path, line, "font weight "+m[1]+" is not allowed"))
	}
	return out
}

func lintFont(path, text string) []manifest.Finding {
	code, strs := Scan(path, text)
	var out []manifest.Finding
	if strings.HasSuffix(strings.ToLower(path), ".css") {
		for ln, l := range strings.Split(code, "\n") {
			out = append(out, declFindings(path, ln+1, l)...)
		}
		return out
	}
	for _, s := range strs { // CSS written inside a string
		out = append(out, declFindings(path, s.Line, s.Text)...)
	}
	for ln, l := range strings.Split(code, "\n") { // object-style
		for _, m := range reObjFamily.FindAllStringSubmatch(l, -1) {
			out = append(out, familyFindings(path, ln+1, m[2])...)
		}
		for _, m := range reObjWeight.FindAllStringSubmatch(l, -1) {
			out = append(out, fontFinding(path, ln+1, "font weight "+m[1]+" is not allowed"))
		}
	}
	return out
}
