package lint

import "testing"

func TestScanBlanksCommentsKeepsLines(t *testing.T) {
	src := "a // #fff\nb /* converged\nstill */ c\n"
	code, _ := Scan("x.ts", src)
	if len(code) != len(src) {
		t.Fatalf("length changed: %d vs %d", len(code), len(src))
	}
	if code != "a        \nb             \n         c\n" {
		t.Fatalf("got %q", code)
	}
}

func TestScanStringsAndURLs(t *testing.T) {
	code, strs := Scan("x.ts", "const u = \"http://x/#fff\"; // tail\nconst v = 'it\\'s'; const t = `a${1}b`;\n")
	if len(strs) != 3 || strs[0].Text != "http://x/#fff" || strs[0].Line != 1 || strs[1].Text != `it\'s` || strs[2].Line != 2 {
		t.Fatalf("strs %+v", strs)
	}
	if want := "const u = \"http://x/#fff\";        \n"; code[:len(want)] != want {
		t.Fatalf("a // inside a string must not start a comment: %q", code)
	}
}

func TestScanCSS(t *testing.T) {
	code, strs := Scan("a.css", "/* #fff */ a { color: red; content: \"/* no */\"; }\n")
	if code[:10] != "          " || len(strs) != 1 || strs[0].Text != "/* no */" {
		t.Fatalf("code %q strs %+v", code, strs)
	}
	code, _ = Scan("a.css", "a { background: url(//cdn/x.png); }\n")
	if code != "a { background: url(//cdn/x.png); }\n" {
		t.Fatalf("got %q", code)
	}
}

func TestScanStrayApostropheDoesNotDesync(t *testing.T) {
	src := "<p>Don't panic</p>\n// color: #fff was the old value\nconst c = \"#fff\";\n"
	code, strs := Scan("x.tsx", src)
	if code[len("<p>Don't panic</p>\n"):len("<p>Don't panic</p>\n// color: #fff was the old value")] != "                                " {
		t.Fatalf("the comment after a JSX apostrophe was not blanked: %q", code)
	}
	last := strs[len(strs)-1]
	if last.Text != "#fff" || last.Line != 3 {
		t.Fatalf("the real literal on line 3 was not found: %+v", strs)
	}
	if fs := lintColour("x.tsx", src); len(fs) != 1 || fs[0].Path != "x.tsx:3" {
		t.Fatalf("want exactly the real literal on line 3, got %v", fs)
	}
}
