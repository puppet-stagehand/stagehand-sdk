package code

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// readPuppetfileFixture reads a committed Puppetfile fixture. The fixtures
// are hermetic: nothing here touches the network.
func readPuppetfileFixture(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "import", "puppetfile", dir, "Puppetfile"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", dir, err)
	}
	return string(b)
}

func findingKinds(fs []*hostv1.ImportFinding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.GetKind())
	}
	return out
}

func moduleByName(pf *hostv1.Puppetfile, name string) *hostv1.PuppetfileModule {
	for _, m := range pf.GetModules() {
		if m.GetName() == name {
			return m
		}
	}
	return nil
}

func TestParsePuppetfileLenient_CanonicalControlRepo(t *testing.T) {
	text := readPuppetfileFixture(t, "canonical-control-repo")
	lim := DefaultImportLimits()

	t.Run("imports_every_declared_module", func(t *testing.T) {
		pf, _ := ParsePuppetfileLenient(text, lim)
		if pf == nil {
			t.Fatal("ParsePuppetfileLenient returned a nil model")
		}
		wantNames := []string{"puppetlabs/inifile", "puppetlabs/stdlib", "puppetlabs/concat", "apache", "apache_docs"}
		if len(pf.GetModules()) != len(wantNames) {
			t.Fatalf("got %d modules, want %d: %v", len(pf.GetModules()), len(wantNames), pf.GetModules())
		}
		for i, n := range wantNames {
			if pf.GetModules()[i].GetName() != n {
				t.Fatalf("Modules[%d] = %q, want %q", i, pf.GetModules()[i].GetName(), n)
			}
		}
		if v := moduleByName(pf, "puppetlabs/stdlib").GetForge().GetVersion(); v != "7.0.1" {
			t.Fatalf("stdlib version = %q, want 7.0.1", v)
		}
		if !moduleByName(pf, "puppetlabs/concat").GetForge().GetLatest() {
			t.Fatal("concat must be the :latest Forge form")
		}
		if c := moduleByName(pf, "apache").GetGit().GetCommit(); c != "1b6f89afdde0df7f9433a163d5c4b5328eac5779" {
			t.Fatalf("apache commit = %q", c)
		}
		if b := moduleByName(pf, "apache_docs").GetGit().GetBranch(); b != "docs_experiment" {
			t.Fatalf("apache_docs branch = %q", b)
		}
	})

	t.Run("one_warning_for_the_forge_directive", func(t *testing.T) {
		_, fs := ParsePuppetfileLenient(text, lim)
		if len(fs) != 1 {
			t.Fatalf("got %d findings %v, want exactly 1", len(fs), findingKinds(fs))
		}
		f := fs[0]
		if f.GetKind() != FindingPuppetfileForgeDirective {
			t.Fatalf("kind = %q, want %q", f.GetKind(), FindingPuppetfileForgeDirective)
		}
		if f.GetSeverity() != hostv1.ImportFinding_WARNING {
			t.Fatalf("severity = %v, want WARNING", f.GetSeverity())
		}
		if f.GetLine() != 1 {
			t.Fatalf("line = %d, want 1", f.GetLine())
		}
		if !strings.Contains(f.GetExcerpt(), "forge 'https://forge.puppet.com'") {
			t.Fatalf("excerpt = %q, want the directive text", f.GetExcerpt())
		}
	})

	t.Run("strict_still_refuses_line_one", func(t *testing.T) {
		_, err := ParsePuppetfile(text)
		if !errors.Is(err, ErrPuppetfileParse) {
			t.Fatalf("strict error = %v, want ErrPuppetfileParse", err)
		}
		if !strings.Contains(err.Error(), "line 1") {
			t.Fatalf("strict error = %v, want it to name line 1", err)
		}
	})

	t.Run("round_trips_through_render_and_strict", func(t *testing.T) {
		pf, _ := ParsePuppetfileLenient(text, lim)
		rendered, err := RenderPuppetfile(pf)
		if err != nil {
			t.Fatalf("RenderPuppetfile(lenient result) error = %v", err)
		}
		back, err := ParsePuppetfile(rendered)
		if err != nil {
			t.Fatalf("strict ParsePuppetfile(rendered) error = %v\n%s", err, rendered)
		}
		if !proto.Equal(pf, back) {
			t.Fatalf("round trip mismatch:\nlenient: %v\nstrict:  %v", pf, back)
		}
	})
}

func TestParsePuppetfileLenient_CleanInputHasNoFindings(t *testing.T) {
	text := "mod 'puppetlabs/stdlib', '7.0.1'\n" +
		"mod 'profiles',\n  :git => 'https://example.com/profiles.git',\n  :tag => 'v1'\n"
	pf, fs := ParsePuppetfileLenient(text, DefaultImportLimits())
	if len(fs) != 0 {
		t.Fatalf("findings = %v, want none", findingKinds(fs))
	}
	strict, err := ParsePuppetfile(text)
	if err != nil {
		t.Fatalf("strict error = %v", err)
	}
	if !proto.Equal(pf, strict) {
		t.Fatalf("strict and lenient differ:\nlenient: %v\nstrict:  %v", pf, strict)
	}
}

func TestParsePuppetfileLenient_Ruby19KeysNormalise(t *testing.T) {
	ruby19 := "mod 'apache',\n  git:    'https://example.com/apache.git',\n  commit: '1b6f89afdde0df7f9433a163d5c4b5328eac5779'\n"
	rocket := "mod 'apache',\n  :git => 'https://example.com/apache.git',\n  :commit => '1b6f89afdde0df7f9433a163d5c4b5328eac5779'\n"

	got, fs := ParsePuppetfileLenient(ruby19, DefaultImportLimits())
	if len(fs) != 0 {
		t.Fatalf("findings = %v, want none for the Ruby-1.9 key form", findingKinds(fs))
	}
	want, err := ParsePuppetfile(rocket)
	if err != nil {
		t.Fatalf("strict hash-rocket parse: %v", err)
	}
	if !proto.Equal(got, want) {
		t.Fatalf("Ruby-1.9 model != hash-rocket model:\n%v\n%v", got, want)
	}

	_, err = ParsePuppetfile(ruby19)
	if !errors.Is(err, ErrPuppetfileParse) || !strings.Contains(err.Error(), "unrecognized mod attributes") {
		t.Fatalf("strict on Ruby-1.9 keys = %v, want its current 'unrecognized mod attributes' error", err)
	}
}

func TestParsePuppetfileLenient_EmptyText(t *testing.T) {
	pf, fs := ParsePuppetfileLenient("", DefaultImportLimits())
	if pf == nil || len(pf.GetModules()) != 0 || pf.GetModuledir() != "" {
		t.Fatalf("empty text model = %v, want empty non-nil", pf)
	}
	if len(fs) != 0 {
		t.Fatalf("findings = %v, want none", findingKinds(fs))
	}
}

func TestParsePuppetfileLenient_FindingsAreUnstamped(t *testing.T) {
	_, fs := ParsePuppetfileLenient("forge 'x'\nrequire 'y'\n", DefaultImportLimits())
	if len(fs) == 0 {
		t.Fatal("expected findings")
	}
	for _, f := range fs {
		if f.GetBranch() != "" || f.GetFile() != "" {
			t.Fatalf("parser stamped %q/%q; the stamping helper owns that", f.GetBranch(), f.GetFile())
		}
	}
}

func TestParsePuppetfileLenient_FindingsCapComesFromLimits(t *testing.T) {
	text := strings.Repeat("forge 'x'\n", 10)
	_, fs := ParsePuppetfileLenient(text, ImportLimits{MaxFindingsPerBranch: 3})
	if len(fs) != 4 {
		t.Fatalf("got %d findings, want cap 3 plus one terminal: %v", len(fs), findingKinds(fs))
	}
	if fs[3].GetKind() != FindingFindingsTruncated {
		t.Fatalf("last kind = %q, want %q", fs[3].GetKind(), FindingFindingsTruncated)
	}
}

func TestParsePuppetfileLenient_ExcerptComesFromLimits(t *testing.T) {
	text := "forge '" + strings.Repeat("é", 50) + "'\n"
	_, fs := ParsePuppetfileLenient(text, ImportLimits{ExcerptBytes: 11})
	if len(fs) != 1 {
		t.Fatalf("findings = %v", findingKinds(fs))
	}
	if len(fs[0].GetExcerpt()) > 11 {
		t.Fatalf("excerpt is %d bytes, want <= 11", len(fs[0].GetExcerpt()))
	}
}
