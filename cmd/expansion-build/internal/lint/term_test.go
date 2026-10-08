package lint

import "testing"

func TestTerm(t *testing.T) {
	flag := []string{`const m = "Run converged";`, `<p>No-op run</p>`, `const m = "NO-OP";`, "const converged = true;"}
	pass := []string{`const m = "corrected";`, `const m = "dry run";`, "// converged and no-op in a comment\n", "/* no-op */ const a = 1;"}
	for _, s := range flag {
		if len(lintTerm("x.tsx", s)) == 0 {
			t.Errorf("not flagged: %s", s)
		}
	}
	for _, s := range pass {
		if fs := lintTerm("x.tsx", s); len(fs) != 0 {
			t.Errorf("flagged %s: %v", s, fs)
		}
	}
}

func TestTermFixNamesReplacement(t *testing.T) {
	if f := lintTerm("x.ts", `"converged"`)[0]; f.Fix == "" || f.Code != "ui_lint_term" {
		t.Fatalf("%+v", f)
	}
}
