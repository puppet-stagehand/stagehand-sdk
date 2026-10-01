package code

import (
	"strings"
	"testing"
	"unicode/utf8"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

func TestImportFindingDefaultLimits(t *testing.T) {
	lim := DefaultImportLimits()
	if lim.MaxFileBytes != 1<<20 || lim.MaxBranchBytes != 4<<20 || lim.MaxSnapshotBytes != 8<<20 {
		t.Fatalf("byte limits = %d/%d/%d, want 1MiB/4MiB/8MiB (DQ-14, DQ-8)", lim.MaxFileBytes, lim.MaxBranchBytes, lim.MaxSnapshotBytes)
	}
	if lim.ExcerptBytes != 200 || lim.MaxFindingsPerBranch != 200 {
		t.Fatalf("excerpt/findings = %d/%d, want 200/200 (DQ-14)", lim.ExcerptBytes, lim.MaxFindingsPerBranch)
	}
	if got := (ImportLimits{}).withDefaults(); got != lim {
		t.Fatalf("zero ImportLimits.withDefaults() = %+v, want %+v", got, lim)
	}
	custom := ImportLimits{ExcerptBytes: 5}.withDefaults()
	if custom.ExcerptBytes != 5 || custom.MaxFindingsPerBranch != DefaultMaxFindingsPerBranch {
		t.Fatalf("withDefaults must keep set fields and fill zero ones, got %+v", custom)
	}
}

func TestImportFindingConstructorSetsFields(t *testing.T) {
	f := newFinding(FindingPuppetfileForgeDirective, hostv1.ImportFinding_WARNING, 7, "forge 'x'", "msg", DefaultImportLimits())
	if f.GetKind() != FindingPuppetfileForgeDirective || f.GetSeverity() != hostv1.ImportFinding_WARNING ||
		f.GetLine() != 7 || f.GetExcerpt() != "forge 'x'" || f.GetMessage() != "msg" {
		t.Fatalf("finding = %v", f)
	}
	if f.GetBranch() != "" || f.GetFile() != "" {
		t.Fatalf("a parser-built finding must carry no branch or file, got %q/%q", f.GetBranch(), f.GetFile())
	}
}

func TestImportFindingExcerptTruncatesOnRuneBoundary(t *testing.T) {
	// "aé" is the bytes 'a' 0xC3 0xA9; a 2-byte cut lands inside the é.
	lim := ImportLimits{ExcerptBytes: 2}
	f := newFinding("k", hostv1.ImportFinding_WARNING, 1, "aé tail", "m", lim)
	if !utf8.ValidString(f.GetExcerpt()) {
		t.Fatalf("excerpt %q is not valid UTF-8 (T-10-39)", f.GetExcerpt())
	}
	if len(f.GetExcerpt()) > 2 {
		t.Fatalf("excerpt %q is %d bytes, want at most 2", f.GetExcerpt(), len(f.GetExcerpt()))
	}
	if f.GetExcerpt() != "a" {
		t.Fatalf("excerpt = %q, want the rune-safe prefix %q", f.GetExcerpt(), "a")
	}

	t.Run("cut_between_runes_keeps_both", func(t *testing.T) {
		g := newFinding("k", hostv1.ImportFinding_WARNING, 1, "aé tail", "m", ImportLimits{ExcerptBytes: 3})
		if g.GetExcerpt() != "aé" {
			t.Fatalf("excerpt = %q, want %q", g.GetExcerpt(), "aé")
		}
	})
	t.Run("short_excerpt_untouched", func(t *testing.T) {
		g := newFinding("k", hostv1.ImportFinding_WARNING, 1, "short", "m", DefaultImportLimits())
		if g.GetExcerpt() != "short" {
			t.Fatalf("excerpt = %q", g.GetExcerpt())
		}
	})
	t.Run("invalid_utf8_input_is_made_valid", func(t *testing.T) {
		g := newFinding("k", hostv1.ImportFinding_WARNING, 1, "ab\xffcd", "m", DefaultImportLimits())
		if !utf8.ValidString(g.GetExcerpt()) {
			t.Fatalf("excerpt %q is not valid UTF-8", g.GetExcerpt())
		}
	})
	t.Run("long_multibyte_stays_valid_at_every_cut", func(t *testing.T) {
		raw := strings.Repeat("日本語", 40)
		for n := 1; n <= 12; n++ {
			g := newFinding("k", hostv1.ImportFinding_WARNING, 1, raw, "m", ImportLimits{ExcerptBytes: n})
			if !utf8.ValidString(g.GetExcerpt()) || len(g.GetExcerpt()) > n {
				t.Fatalf("cut %d: excerpt %q invalid or too long", n, g.GetExcerpt())
			}
		}
	})
}

func TestImportFindingsCapAppendsOneTerminalFinding(t *testing.T) {
	lim := ImportLimits{MaxFindingsPerBranch: 3}
	fl := newFindingList(lim)
	for i := 1; i <= 5; i++ {
		fl.add(newFinding(FindingPuppetfileUnsupportedRuby, hostv1.ImportFinding_WARNING, i, "x", "m", lim))
	}
	got := fl.items
	if len(got) != 4 {
		t.Fatalf("len = %d, want cap(3)+1 terminal", len(got))
	}
	last := got[3]
	if last.GetKind() != FindingFindingsTruncated || last.GetSeverity() != hostv1.ImportFinding_WARNING {
		t.Fatalf("terminal finding = %v", last)
	}
	if last.GetLine() < 1 {
		t.Fatalf("terminal finding line = %d, want >= 1", last.GetLine())
	}
	for i := 0; i < 3; i++ {
		if got[i].GetKind() != FindingPuppetfileUnsupportedRuby {
			t.Fatalf("finding %d kind = %q", i, got[i].GetKind())
		}
	}

	t.Run("exactly_at_cap_has_no_terminal", func(t *testing.T) {
		fl := newFindingList(lim)
		for i := 1; i <= 3; i++ {
			fl.add(newFinding(FindingPuppetfileUnsupportedRuby, hostv1.ImportFinding_WARNING, i, "x", "m", lim))
		}
		if len(fl.items) != 3 {
			t.Fatalf("len = %d, want 3", len(fl.items))
		}
	})
}

func TestImportFindingStampSetsBranchAndFile(t *testing.T) {
	fs := []*hostv1.ImportFinding{
		newFinding("a", hostv1.ImportFinding_WARNING, 1, "", "", DefaultImportLimits()),
		newFinding("b", hostv1.ImportFinding_WARNING, 2, "", "", DefaultImportLimits()),
	}
	stampFindings(fs, "production", "Puppetfile")
	for _, f := range fs {
		if f.GetBranch() != "production" || f.GetFile() != "Puppetfile" {
			t.Fatalf("finding %v not stamped", f)
		}
	}
}
