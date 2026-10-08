package lint

import "testing"

func TestFont(t *testing.T) {
	flag := []string{
		"a { font-family: Arial, sans-serif; }\n",
		"a { font-family: 'Helvetica Neue'; }\n",
		"a { font-weight: 700; }\n", "a { font-weight: bold; }\n",
		`const s = {fontFamily: "Comic Sans MS"};`, `const s = {fontWeight: 700};`,
		`const s = "font-family: Georgia";`, `const s = {fontWeight: "bold"};`,
	}
	pass := []string{
		"a { font-family: 'IBM Plex Sans', sans-serif; }\n", "a { font-family: \"IBM Plex Mono\", monospace; }\n",
		"a { font-family: var(--font-brand); }\n", "a { font-weight: 600; font-weight: 400; }\n",
		`const s = {fontFamily: "var(--font-mono)"};`, "/* font-family: Arial */\n",
		`const s = {fontWeight: 600};`, "a { font-family: inherit; }\n",
	}
	for _, s := range flag {
		if len(lintFont("x.css", s)) == 0 && len(lintFont("x.ts", s)) == 0 {
			t.Errorf("not flagged: %s", s)
		}
	}
	for _, s := range pass {
		if fs := lintFont("x.css", s); len(fs) != 0 {
			t.Errorf("css flagged %s: %v", s, fs)
		}
		if fs := lintFont("x.ts", s); len(fs) != 0 {
			t.Errorf("ts flagged %s: %v", s, fs)
		}
	}
}

func TestFontIgnoresNonLiteralValuesInTS(t *testing.T) {
	pass := []string{
		"const s = { fontFamily: tokens.mono, fontWeight: 600 };",
		"interface Props { fontFamily: string; fontWeight: number }",
		"const s = { fontFamily: theme.fonts.brand };",
	}
	for _, s := range pass {
		if fs := lintFont("x.ts", s); len(fs) != 0 {
			t.Errorf("flagged %q: %v", s, fs)
		}
	}
}
