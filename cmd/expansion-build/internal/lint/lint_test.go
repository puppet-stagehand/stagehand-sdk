package lint

import "testing"

func TestRunSortsAndCombines(t *testing.T) {
	fs := Run([]Source{
		{"b.ts", `const a = "#fff"; const t = "no-op";`},
		{"a.css", ".x { font-weight: 700; color: #fff }\n"},
		{"ok.ts", `export default 1;`},
	})
	if len(fs) != 4 {
		t.Fatalf("got %d: %v", len(fs), fs)
	}
	if fs[0].Path[:5] != "a.css" || fs[len(fs)-1].Path[:4] != "b.ts" {
		t.Fatalf("not sorted by path: %v", fs)
	}
	for _, f := range fs {
		if f.Fix == "" {
			t.Errorf("no fix: %+v", f)
		}
	}
}
