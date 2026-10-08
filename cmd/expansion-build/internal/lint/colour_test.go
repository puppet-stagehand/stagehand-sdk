package lint

import "testing"

func TestColourJS(t *testing.T) {
	flag := []string{
		`const a = "#fff";`, `const a = "#FF00AA";`, `const a = "#ff00aa80";`,
		`const s = "color: #abc";`, `const s = "linear-gradient(#fff, #000)";`,
		`const s = "rgb(1,2,3)";`, `const s = "hsla(1,2%,3%,.5)";`,
	}
	pass := []string{
		`const a = "/runtime/v1/react.js";`, `const a = "var(--primary)";`,
		`// color: #fff in a comment` + "\n", `const id = "page#section";`,
		`class A { #priv = 1 }`, `const a = "#zzz";`,
	}
	for _, s := range flag {
		if n := len(lintColour("x.ts", s)); n == 0 {
			t.Errorf("not flagged: %s", s)
		}
	}
	for _, s := range pass {
		if fs := lintColour("x.ts", s); len(fs) != 0 {
			t.Errorf("flagged %s: %v", s, fs)
		}
	}
}

func TestColourCSS(t *testing.T) {
	flag := ".a { color: #fff; }\n.b { background: rgba(0,0,0,.1) }\n"
	if fs := lintColour("a.css", flag); len(fs) != 2 || fs[0].Path != "a.css:1" || fs[1].Path != "a.css:2" {
		t.Fatalf("got %v", fs)
	}
	pass := "#add { color: var(--text); }\n.x:hover { margin: 0 }\n/* #fff */\n"
	if fs := lintColour("a.css", pass); len(fs) != 0 {
		t.Fatalf("id selector or comment flagged: %v", fs)
	}
}

func TestColourFindingHasFix(t *testing.T) {
	f := lintColour("x.ts", `const a = "#fff";`)[0]
	if f.Code != "ui_lint_hex_colour" || f.Fix == "" || f.Path != "x.ts:1" {
		t.Fatalf("%+v", f)
	}
}
