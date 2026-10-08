package lint

import "testing"

func TestEmoji(t *testing.T) {
	flag := []string{"const a = \"ok \U0001F600\";", "x = '✅'", "<p>rocket \U0001F680</p>", "a = \"❤️\""}
	pass := []string{"const a = \"✓ corrected\";", "const b = \"✗ failed\";", "// \U0001F600 in a comment\n", "const c = \"→\";"}
	for _, s := range flag {
		if len(lintEmoji("x.tsx", s)) == 0 {
			t.Errorf("not flagged: %q", s)
		}
	}
	for _, s := range pass {
		if fs := lintEmoji("x.tsx", s); len(fs) != 0 {
			t.Errorf("flagged %q: %v", s, fs)
		}
	}
}
