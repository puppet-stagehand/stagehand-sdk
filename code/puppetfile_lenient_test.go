package code

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

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

// ---------------------------------------------------------------------------
// The construct catalog (10-RESEARCH "Real-World Fixture Catalog / Puppetfile").
// One table row per real-world shape; kinds are referenced through the code
// package constants so a renamed kind fails to compile instead of passing.
// ---------------------------------------------------------------------------

type catalogCase struct {
	name        string
	text        string
	wantKinds   []string
	wantLines   []int    // line of each finding, parallel to wantKinds
	wantModules []string // names that must be in the model, in order
	absent      []string // names that must NOT be in the model (skipped whole)
	moduledir   string
	check       func(t *testing.T, pf *hostv1.Puppetfile)
}

func gitMod(name string) string {
	return "mod '" + name + "',\n  :git => 'https://example.com/" + name + ".git'"
}

func catalogCases() []catalogCase {
	cases := []catalogCase{
		{
			name:        "forge_directive",
			text:        "forge 'https://forge.puppet.com'\nmod 'puppetlabs/ntp'\n",
			wantKinds:   []string{FindingPuppetfileForgeDirective},
			wantLines:   []int{1},
			wantModules: []string{"puppetlabs/ntp"},
		},
		{
			name:        "forge_directive_double_quoted_in_parens",
			text:        "forge(\"https://forge.puppet.com\")\n",
			wantKinds:   []string{FindingPuppetfileForgeDirective},
			wantLines:   []int{1},
			wantModules: nil,
		},
		{
			name:        "forgery_is_not_the_directive",
			text:        "forgery 'x'\n",
			wantKinds:   []string{FindingPuppetfileUnsupportedRuby},
			wantLines:   []int{1},
			wantModules: nil,
		},
		{
			name:        "hash_form_forge_module",
			text:        "mod :version => '1.0'\nmod 'puppetlabs/ntp'\n",
			wantKinds:   []string{FindingPuppetfileUnmodelledAttribute},
			wantLines:   []int{1},
			wantModules: []string{"puppetlabs/ntp"},
		},
		{
			name:        "parenthesised_call",
			text:        "mod('ns/name', '1.0')\nmod 'puppetlabs/ntp'\n",
			wantKinds:   []string{FindingPuppetfileUnsupportedRuby},
			wantLines:   []int{1},
			wantModules: []string{"puppetlabs/ntp"},
		},
		{
			name:      "ruby_if_each_variable_require",
			text:      "if ENV['X']\n%w(a b).each do |n|\nversion = '1.0'\nrequire 'yaml'\nend\n",
			wantKinds: []string{FindingPuppetfileUnsupportedRuby, FindingPuppetfileUnsupportedRuby, FindingPuppetfileUnsupportedRuby, FindingPuppetfileUnsupportedRuby, FindingPuppetfileUnsupportedRuby},
			wantLines: []int{1, 2, 3, 4, 5},
		},
		{
			name:        "conflicting_ref_and_branch",
			text:        "mod 'conflict',\n  :git => 'https://example.com/c.git',\n  :ref => 'a',\n  :branch => 'b'\nmod 'puppetlabs/ntp'\n",
			wantKinds:   []string{FindingPuppetfileConflictingRef},
			wantLines:   []int{1},
			wantModules: []string{"puppetlabs/ntp"},
			absent:      []string{"conflict"},
		},
		{
			name:        "conflicting_ruby19_tag_and_branch",
			text:        "mod 'conflict', git: 'https://example.com/c.git', tag: 'v1', branch: 'b'\n",
			wantKinds:   []string{FindingPuppetfileConflictingRef},
			wantLines:   []int{1},
			wantModules: nil,
			absent:      []string{"conflict"},
		},
		{
			name:        "duplicate_module_keeps_first",
			text:        "mod 'puppetlabs/ntp', '1.0.0'\nmod 'puppetlabs/ntp', '2.0.0'\n",
			wantKinds:   []string{FindingPuppetfileDuplicateModule},
			wantLines:   []int{2},
			wantModules: []string{"puppetlabs/ntp"},
			check: func(t *testing.T, pf *hostv1.Puppetfile) {
				if v := pf.GetModules()[0].GetForge().GetVersion(); v != "1.0.0" {
					t.Fatalf("kept version %q, want the first occurrence 1.0.0", v)
				}
			},
		},
		{
			name:      "duplicate_moduledir_keeps_last",
			text:      "moduledir 'a'\nmoduledir 'b'\n",
			wantKinds: []string{FindingPuppetfileDuplicateModuledir},
			wantLines: []int{2},
			moduledir: "b",
		},
		{
			name:        "bare_forge_name",
			text:        "mod 'stdlib'\nmod 'puppetlabs/ntp'\n",
			wantKinds:   []string{FindingPuppetfileInvalidModule},
			wantLines:   []int{1},
			wantModules: []string{"puppetlabs/ntp"},
			absent:      []string{"stdlib"},
		},
		{
			name:        "absolute_git_path",
			text:        "mod 'abs_git', :git => '/srv/git/x.git'\n",
			wantKinds:   []string{FindingPuppetfileInvalidModule},
			wantLines:   []int{1},
			wantModules: nil,
			absent:      []string{"abs_git"},
		},
		{
			name:        "git_url_with_embedded_quote_does_not_survive_render",
			text:        "mod 'quoted', :git => \"https://example.com/a'b.git\"\n",
			wantKinds:   []string{FindingPuppetfileInvalidModule},
			wantLines:   []int{1},
			wantModules: nil,
			absent:      []string{"quoted"},
		},
		{
			name:        "invalid_utf8_module_name",
			text:        "mod 'caf\xe9', :git => 'https://example.com/x.git'\n",
			wantKinds:   []string{FindingPuppetfileInvalidModule},
			wantLines:   []int{1},
			wantModules: nil,
		},
		{
			name:        "git_ref_value_is_a_variable",
			text:        "mod 'varref',\n  :git => 'https://example.com/v.git',\n  :ref => some_var\n",
			wantKinds:   []string{FindingPuppetfileUnsupportedRuby},
			wantLines:   []int{1},
			wantModules: nil,
			absent:      []string{"varref"},
		},
		{
			name:        "bare_symbol_other_than_control_branch",
			text:        "mod 'sym',\n  :git => 'https://example.com/s.git',\n  :branch => :other\n",
			wantKinds:   []string{FindingPuppetfileUnsupportedRuby},
			wantLines:   []int{1},
			wantModules: nil,
			absent:      []string{"sym"},
		},
		{
			name:        "git_attr_value_is_an_expression",
			text:        "mod 'a',\n  :git => 'https://example.com/a.git' + system('x')\n",
			wantKinds:   []string{FindingPuppetfileUnsupportedRuby},
			wantLines:   []int{1},
			wantModules: nil,
			absent:      []string{"a"},
		},
		{
			name:        "git_attr_trailing_garbage",
			text:        "mod 'a',\n  :git => 'https://example.com/a.git' garbage\n",
			wantKinds:   []string{FindingPuppetfileUnsupportedRuby},
			wantLines:   []int{1},
			wantModules: nil,
			absent:      []string{"a"},
		},
		{
			name:        "git_attr_trailing_token_after_ref",
			text:        "mod 'a', :git => 'https://example.com/a.git', :tag => 'v1' trailing\n",
			wantKinds:   []string{FindingPuppetfileUnsupportedRuby},
			wantLines:   []int{1},
			wantModules: nil,
			absent:      []string{"a"},
		},
		{
			name:        "repeated_git_attribute",
			text:        "mod 'a', :git => 'https://example.com/a.git', :git => 'https://example.com/b.git'\n",
			wantKinds:   []string{FindingPuppetfileUnsupportedRuby},
			wantLines:   []int{1},
			wantModules: nil,
			absent:      []string{"a"},
		},
		{
			name:        "repeated_default_branch_attribute",
			text:        "mod 'a', :git => 'https://example.com/a.git', :default_branch => 'main', :default_branch => 'dev'\n",
			wantKinds:   []string{FindingPuppetfileUnsupportedRuby},
			wantLines:   []int{1},
			wantModules: nil,
			absent:      []string{"a"},
		},
		{
			name:      "moduledir_value_is_not_a_safe_scalar",
			text:      "moduledir 'a\\'\n",
			wantKinds: []string{FindingPuppetfileUnsupportedRuby},
			wantLines: []int{1},
			moduledir: "",
		},
		{
			name:      "moduledir_value_carries_nul",
			text:      "moduledir 'a\x00b'\n",
			wantKinds: []string{FindingPuppetfileUnsupportedRuby},
			wantLines: []int{1},
			moduledir: "",
		},
		{
			name:      "moduledir_value_carries_vertical_tab",
			text:      "moduledir 'a\vb'\n",
			wantKinds: []string{FindingPuppetfileUnsupportedRuby},
			wantLines: []int{1},
			moduledir: "",
		},
		{
			name:      "moduledir_value_carries_form_feed",
			text:      "moduledir 'a\fb'\n",
			wantKinds: []string{FindingPuppetfileUnsupportedRuby},
			wantLines: []int{1},
			moduledir: "",
		},
		{
			name:      "moduledir_value_carries_line_separator",
			text:      "moduledir 'a b'\n",
			wantKinds: []string{FindingPuppetfileUnsupportedRuby},
			wantLines: []int{1},
			moduledir: "",
		},
		{
			name:        "bom_is_not_a_finding",
			text:        "\xef\xbb\xbfmod 'puppetlabs/ntp'\n",
			wantModules: []string{"puppetlabs/ntp"},
		},
		{
			name:        "hash_inside_quoted_url_and_trailing_comment",
			text:        "mod 'frag',\n  :git => 'https://example.com/frag.git#branch', # trailing\n  :tag => 'v1' # another\nmod 'puppetlabs/ntp' # trailing\n",
			wantModules: []string{"frag", "puppetlabs/ntp"},
		},
		{
			name: "comment_only",
			text: "# nothing here\n\n   # nor here\n",
		},
		{
			name:        "ruby19_keys_in_a_forge_remainder_are_unmodelled",
			text:        "mod 'puppetlabs/apache', version: '1.0'\n",
			wantKinds:   []string{FindingPuppetfileUnmodelledAttribute},
			wantLines:   []int{1},
			wantModules: nil,
		},
	}

	// Every attribute the model cannot represent skips the WHOLE statement.
	for _, key := range []string{"install_path", "local", "exclude_spec", "type", "source", "svn", "rev", "version"} {
		cases = append(cases, catalogCase{
			name:        "unmodelled_attribute_" + key + "_on_git_module",
			text:        gitMod("skipme") + ",\n  :" + key + " => 'x'\n" + "mod 'puppetlabs/ntp'\n",
			wantKinds:   []string{FindingPuppetfileUnmodelledAttribute},
			wantLines:   []int{1},
			wantModules: []string{"puppetlabs/ntp"},
			absent:      []string{"skipme"},
		}, catalogCase{
			name:        "unmodelled_attribute_" + key + "_on_forge_module",
			text:        "mod 'puppetlabs/skipme', '1.0.0', :" + key + " => 'x'\nmod 'puppetlabs/ntp'\n",
			wantKinds:   []string{FindingPuppetfileUnmodelledAttribute},
			wantLines:   []int{1},
			wantModules: []string{"puppetlabs/ntp"},
			absent:      []string{"puppetlabs/skipme"},
		})
	}
	return cases
}

func TestParsePuppetfileLenient_Catalog(t *testing.T) {
	for _, tc := range catalogCases() {
		t.Run(tc.name, func(t *testing.T) {
			pf, fs := ParsePuppetfileLenient(tc.text, DefaultImportLimits())
			if pf == nil {
				t.Fatal("nil model")
			}
			if got := findingKinds(fs); strings.Join(got, ",") != strings.Join(tc.wantKinds, ",") {
				t.Fatalf("finding kinds = %v, want %v\n%v", got, tc.wantKinds, fs)
			}
			for i, f := range fs {
				if i < len(tc.wantLines) && int(f.GetLine()) != tc.wantLines[i] {
					t.Errorf("finding %d (%s) line = %d, want %d", i, f.GetKind(), f.GetLine(), tc.wantLines[i])
				}
				// D-10: a Puppetfile is never unparseable as a whole, so no
				// construct is an error.
				if f.GetSeverity() != hostv1.ImportFinding_WARNING {
					t.Errorf("finding %s severity = %v, want WARNING only", f.GetKind(), f.GetSeverity())
				}
				if f.GetExcerpt() == "" && f.GetKind() != FindingFindingsTruncated {
					t.Errorf("finding %s has an empty excerpt", f.GetKind())
				}
				if f.GetMessage() == "" {
					t.Errorf("finding %s has an empty message", f.GetKind())
				}
			}
			var names []string
			for _, m := range pf.GetModules() {
				names = append(names, m.GetName())
			}
			if strings.Join(names, ",") != strings.Join(tc.wantModules, ",") {
				t.Fatalf("modules = %v, want %v", names, tc.wantModules)
			}
			// A skipped statement must be ABSENT, not imported minus the
			// attribute (T-10-18).
			for _, n := range tc.absent {
				if moduleByName(pf, n) != nil {
					t.Fatalf("module %q is in the model but its statement should have been skipped whole", n)
				}
			}
			if pf.GetModuledir() != tc.moduledir {
				t.Fatalf("moduledir = %q, want %q", pf.GetModuledir(), tc.moduledir)
			}
			// Whatever is imported must render and strict-re-parse equal, so
			// ApplyImport's second pass can be infallible.
			rendered, err := RenderPuppetfile(pf)
			if err != nil {
				t.Fatalf("RenderPuppetfile(lenient result) error = %v", err)
			}
			back, err := ParsePuppetfile(rendered)
			if err != nil || !proto.Equal(pf, back) {
				t.Fatalf("strict re-parse of %q: err=%v equal=%v", rendered, err, err == nil && proto.Equal(pf, back))
			}
			if tc.check != nil {
				tc.check(t, pf)
			}
		})
	}
}

func TestParsePuppetfileLenient_DuplicateModuledirStrictStillErrors(t *testing.T) {
	_, err := ParsePuppetfile("moduledir 'a'\nmoduledir 'b'\n")
	if !errors.Is(err, ErrPuppetfileParse) || !strings.Contains(err.Error(), "line 2: duplicate moduledir") {
		t.Fatalf("strict error = %v, want its current duplicate-moduledir message", err)
	}
}

func TestParsePuppetfileLenient_CRLFMatchesLF(t *testing.T) {
	lf := "mod 'puppetlabs/ntp'\nforge 'x'\nmod 'a',\n  :git => 'https://example.com/a.git',\n  :tag => 'v1'\nrequire 'y'\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	pfLF, fsLF := ParsePuppetfileLenient(lf, DefaultImportLimits())
	pfCR, fsCR := ParsePuppetfileLenient(crlf, DefaultImportLimits())
	if !proto.Equal(pfLF, pfCR) {
		t.Fatalf("models differ:\nLF   %v\nCRLF %v", pfLF, pfCR)
	}
	if len(fsLF) != len(fsCR) || len(fsLF) != 2 {
		t.Fatalf("findings LF=%v CRLF=%v, want the same 2", findingKinds(fsLF), findingKinds(fsCR))
	}
	for i := range fsLF {
		if fsLF[i].GetKind() != fsCR[i].GetKind() || fsLF[i].GetLine() != fsCR[i].GetLine() {
			t.Fatalf("finding %d differs: %v vs %v", i, fsLF[i], fsCR[i])
		}
	}
}

func TestParsePuppetfileLenient_ConstructsFixture(t *testing.T) {
	text := readPuppetfileFixture(t, "constructs")
	lines := strings.Split(text, "\n")
	lineOf := func(needle string) int {
		t.Helper()
		for i, l := range lines {
			if strings.HasPrefix(l, needle) {
				return i + 1
			}
		}
		t.Fatalf("fixture has no line starting %q", needle)
		return 0
	}
	pf, fs := ParsePuppetfileLenient(text, DefaultImportLimits())

	want := []struct {
		kind string
		line int
	}{
		{FindingPuppetfileForgeDirective, lineOf("forge ")},
		{FindingPuppetfileDuplicateModuledir, lineOf("moduledir 'second'")},
		{FindingPuppetfileUnmodelledAttribute, lineOf("mod 'skip_install_path'")},
		{FindingPuppetfileUnmodelledAttribute, lineOf("mod 'puppetlabs/apache'")},
		{FindingPuppetfileUnmodelledAttribute, lineOf("mod :version")},
		{FindingPuppetfileUnsupportedRuby, lineOf("mod('puppetlabs/ntp'")},
		{FindingPuppetfileUnsupportedRuby, lineOf("if ENV")},
		{FindingPuppetfileUnsupportedRuby, lineOf("%w(a b)")},
		{FindingPuppetfileUnsupportedRuby, lineOf("version = ")},
		{FindingPuppetfileUnsupportedRuby, lineOf("require 'yaml'")},
		{FindingPuppetfileUnsupportedRuby, lineOf("end")},
		{FindingPuppetfileConflictingRef, lineOf("mod 'conflict'")},
		{FindingPuppetfileDuplicateModule, lineOf("mod 'puppetlabs/stdlib', '9.9.9'")},
		{FindingPuppetfileInvalidModule, lineOf("mod 'stdlib'")},
		{FindingPuppetfileInvalidModule, lineOf("mod 'abs_git'")},
	}
	if len(fs) != len(want) {
		t.Fatalf("got %d findings, want %d:\n%v", len(fs), len(want), fs)
	}
	for i, w := range want {
		if fs[i].GetKind() != w.kind || int(fs[i].GetLine()) != w.line {
			t.Errorf("finding %d = %s@%d, want %s@%d", i, fs[i].GetKind(), fs[i].GetLine(), w.kind, w.line)
		}
		if fs[i].GetSeverity() != hostv1.ImportFinding_WARNING {
			t.Errorf("finding %d severity = %v", i, fs[i].GetSeverity())
		}
	}
	var names []string
	for _, m := range pf.GetModules() {
		names = append(names, m.GetName())
	}
	if strings.Join(names, ",") != "puppetlabs/stdlib,profiles" {
		t.Fatalf("modules = %v, want [puppetlabs/stdlib profiles]", names)
	}
	if v := moduleByName(pf, "puppetlabs/stdlib").GetForge().GetVersion(); v != "7.0.1" {
		t.Fatalf("stdlib version = %q, want the first occurrence 7.0.1", v)
	}
	if pf.GetModuledir() != "second" {
		t.Fatalf("moduledir = %q, want the last value", pf.GetModuledir())
	}
}

func TestParsePuppetfileLenient_Ruby19Fixture(t *testing.T) {
	pf, fs := ParsePuppetfileLenient(readPuppetfileFixture(t, "ruby19"), DefaultImportLimits())
	if len(fs) != 0 {
		t.Fatalf("findings = %v, want none", fs)
	}
	want, err := ParsePuppetfile("mod 'apache',\n  :git => 'https://github.com/puppetlabs/puppetlabs-apache',\n  :commit => '1b6f89afdde0df7f9433a163d5c4b5328eac5779'\n" +
		"mod 'concat',\n  :git => 'https://github.com/puppetlabs/puppetlabs-concat',\n  :branch => 'main'\n" +
		"mod 'ntp',\n  :git => 'https://example.com/ntp.git',\n  :tag => 'v1.2.3'\n" +
		"mod 'profiles',\n  :git => 'git@git.example.com:puppet/profiles.git',\n  :branch => :control_branch,\n  :default_branch => 'main'\n")
	if err != nil {
		t.Fatalf("strict hash-rocket equivalent: %v", err)
	}
	if !proto.Equal(pf, want) {
		t.Fatalf("Ruby-1.9 fixture model differs from its hash-rocket equivalent:\n%v\n%v", pf, want)
	}
	if _, err := ParsePuppetfile(readPuppetfileFixture(t, "ruby19")); err == nil {
		t.Fatal("strict must still reject the Ruby-1.9 fixture")
	}
}

// The BOM/CRLF fixture is byte-level: a repository line-ending normalisation
// (a .gitattributes text=auto rule, an editor save) would invalidate it, which
// is why the first bytes are asserted here.
func TestParsePuppetfileLenient_BOMCRLFFixture(t *testing.T) {
	text := readPuppetfileFixture(t, "bom-crlf")
	if !strings.HasPrefix(text, "\xef\xbb\xbf") || !strings.Contains(text, "\r\n") {
		t.Fatal("bom-crlf fixture lost its BOM or CRLF endings; do not normalise it")
	}
	pf, fs := ParsePuppetfileLenient(text, DefaultImportLimits())
	if len(fs) != 1 || fs[0].GetKind() != FindingPuppetfileForgeDirective || fs[0].GetLine() != 5 {
		t.Fatalf("findings = %v, want only the forge directive on line 5", fs)
	}
	if len(pf.GetModules()) != 2 || pf.GetModules()[0].GetName() != "puppetlabs/stdlib" || pf.GetModules()[1].GetName() != "profiles" {
		t.Fatalf("modules = %v", pf.GetModules())
	}
	if _, err := ParsePuppetfile(text); err == nil {
		t.Fatal("strict must refuse the BOM fixture (U+FEFF before mod)")
	}
}

// ---------------------------------------------------------------------------
// One grammar, proven: strict and lenient agree, rendered models round-trip,
// and the lenient output is always renderable and strict-re-parseable.
// ---------------------------------------------------------------------------

// strictCorpusAccepted is every input the strict tests in puppetfile_test.go
// accept, plus the rendered form of its roundTripFixtures. That file's inline
// inputs are not exported as a list and the file is the strict-behaviour
// contract that must stay unmodified (T-10-16), so the inline ones are
// mirrored here; roundTripFixtures is the one list shared by reference, so the
// two files cannot drift on it.
func strictCorpusAccepted(t *testing.T) []string {
	t.Helper()
	out := []string{
		"mod 'puppetlabs/ntp'\n",
		"mod 'puppetlabs/apache', '0.10.0'\n",
		"mod 'puppetlabs/stdlib', :latest\n",
		"moduledir 'thirdparty'\n",
		"moduledir \"thirdparty\"\n",
		"",
		"# just a comment\n\n# another\n",
		"mod 'puppetlabs/ntp'\nmod 'puppetlabs/apache'\nmod 'puppetlabs/apache_extra'\nmod 'puppetlabs/stdlib'\n",
		"mod 'puppetlabs/ntp', '1.0.0'\n",
		"mod \"puppetlabs/ntp\", \"1.0.0\"\n",
		"mod 'apache',\n  :git => 'https://github.com/puppetlabs/puppetlabs-apache'\n",
		"mod 'apache',\n  :git => 'url',\n  :ref => 'docs_experiment'\n",
		"mod 'apache',\n  :git => 'url',\n  :tag => '0.9.0'\n",
		"mod 'apache',\n  :git => 'url',\n  :branch => 'docs_experiment'\n",
		"mod 'apache',\n  :git => 'url',\n  :commit => '83401079053dca11d61945bd9beef9ecf7576cbf'\n",
		"mod 'apache',\n  :git => 'url',\n  :branch => :control_branch\n",
		"mod 'apache',\n  :git => 'url',\n  :default_branch => 'main'\n",
		"mod 'apache',\n  :git => 'url',\n  :branch => :control_branch,\n  :default_branch => 'main'\n",
	}
	for i, m := range roundTripFixtures {
		rendered, err := RenderPuppetfile(m)
		if err != nil {
			t.Fatalf("roundTripFixtures[%d]: %v", i, err)
		}
		out = append(out, rendered)
	}
	return out
}

// strictCorpusRejected mirrors the inputs the strict tests reject.
func strictCorpusRejected() []string {
	return []string{
		"mod 'puppetlabs/apache',\n  :type => 'forge'\n",
		"mod 'apache',\n  :git => 'url',\n  :tag => '0.9.0',\n  :branch => 'main'\n",
		"mod 'apache',\n  :git => 'url',\n  :ref => 'x',\n  :commit => 'y'\n",
		"moduledir 'a'\nmoduledir 'b'\n",
		"forge 'https://forge.puppet.com'\n",
		"mod 'a',\n  :git => 'https://example.com/a.git' + system('x')\n",
		"mod 'a',\n  :git => 'https://example.com/a.git' garbage\n",
		"mod 'a', :git => 'https://example.com/a.git', :tag => 'v1' trailing\n",
		"mod 'a', :git => 'https://example.com/a.git', :git => 'https://example.com/b.git'\n",
		"mod 'a', :git => 'https://example.com/a.git', :default_branch => 'main', :default_branch => 'dev'\n",
	}
}

// TestLenientAgreesWithStrict is what makes "one grammar" (D-11) a checked
// property rather than a design intention: on every strict-valid text the
// lenient parse returns the same model with zero findings. The one carve-out
// is a module strict accepts but ValidateModule refuses (the strict tests use
// the placeholder url 'url'); lenient then reports puppetfile_invalid_module
// and drops exactly that module, which is the point of the lenient validation
// (RESEARCH Pitfall 2).
func TestLenientAgreesWithStrict(t *testing.T) {
	lim := DefaultImportLimits()
	t.Run("strict_valid_text_agrees", func(t *testing.T) {
		for i, text := range strictCorpusAccepted(t) {
			strict, err := ParsePuppetfile(text)
			if err != nil {
				t.Fatalf("corpus[%d] %q is not strict-valid: %v", i, text, err)
			}
			lenient, fs := ParsePuppetfileLenient(text, lim)
			allRenderable := true
			for _, m := range strict.GetModules() {
				if ValidateModule(m) != nil {
					allRenderable = false
				}
			}
			if allRenderable {
				if len(fs) != 0 {
					t.Errorf("corpus[%d] %q: lenient findings %v, want none", i, text, findingKinds(fs))
				}
				if !proto.Equal(strict, lenient) {
					t.Errorf("corpus[%d] %q: strict and lenient differ:\n%v\n%v", i, text, strict, lenient)
				}
				continue
			}
			if len(fs) == 0 {
				t.Errorf("corpus[%d] %q holds a module ValidateModule refuses but lenient reported nothing", i, text)
			}
			for _, f := range fs {
				if f.GetKind() != FindingPuppetfileInvalidModule {
					t.Errorf("corpus[%d] %q: finding %s, want only %s", i, text, f.GetKind(), FindingPuppetfileInvalidModule)
				}
			}
			if len(lenient.GetModules()) >= len(strict.GetModules()) {
				t.Errorf("corpus[%d] %q: lenient kept %d modules of %d despite an invalid one", i, text, len(lenient.GetModules()), len(strict.GetModules()))
			}
		}
	})
	t.Run("strict_rejected_text_yields_findings", func(t *testing.T) {
		for i, text := range strictCorpusRejected() {
			if _, err := ParsePuppetfile(text); err == nil {
				t.Fatalf("corpus[%d] %q is not strict-rejected", i, text)
			}
			_, fs := ParsePuppetfileLenient(text, lim)
			if len(fs) == 0 {
				t.Errorf("corpus[%d] %q: strict refuses it but lenient reported no finding", i, text)
			}
		}
	})
}

// TestLenientRoundTripsRenderedModels is the PF-05 property Phase 6
// established, inherited by whatever import produces: lenient(Render(m)) == m
// with zero findings.
func TestLenientRoundTripsRenderedModels(t *testing.T) {
	git := func(name string, g *hostv1.GitSource) *hostv1.PuppetfileModule {
		return &hostv1.PuppetfileModule{Name: name, Source: &hostv1.PuppetfileModule_Git{Git: g}}
	}
	forge := func(name string, f *hostv1.ForgeSource) *hostv1.PuppetfileModule {
		return &hostv1.PuppetfileModule{Name: name, Source: &hostv1.PuppetfileModule_Forge{Forge: f}}
	}
	const u = "https://example.com/m.git"
	models := map[string]*hostv1.Puppetfile{
		"forge_explicit_version": {Modules: []*hostv1.PuppetfileModule{forge("puppetlabs/apache", &hostv1.ForgeSource{Version: "0.10.0"})}},
		"forge_latest":           {Modules: []*hostv1.PuppetfileModule{forge("puppetlabs/stdlib", &hostv1.ForgeSource{Latest: true})}},
		"git_ref":                {Modules: []*hostv1.PuppetfileModule{git("m", &hostv1.GitSource{Url: u, RefKind: &hostv1.GitSource_Ref{Ref: "r"}})}},
		"git_tag":                {Modules: []*hostv1.PuppetfileModule{git("m", &hostv1.GitSource{Url: u, RefKind: &hostv1.GitSource_Tag{Tag: "v1"}})}},
		"git_branch":             {Modules: []*hostv1.PuppetfileModule{git("m", &hostv1.GitSource{Url: u, RefKind: &hostv1.GitSource_Branch{Branch: "main"}})}},
		"git_commit":             {Modules: []*hostv1.PuppetfileModule{git("m", &hostv1.GitSource{Url: u, RefKind: &hostv1.GitSource_Commit{Commit: "83401079053dca11d61945bd9beef9ecf7576cbf"}})}},
		"git_control_branch": {Modules: []*hostv1.PuppetfileModule{git("m", &hostv1.GitSource{Url: u,
			RefKind: &hostv1.GitSource_ControlBranch{ControlBranch: &hostv1.ControlBranch{}}})}},
		"git_default_branch_only": {Modules: []*hostv1.PuppetfileModule{git("m", &hostv1.GitSource{Url: u, DefaultBranch: "main"})}},
		"git_control_branch_with_default_branch": {Modules: []*hostv1.PuppetfileModule{git("m", &hostv1.GitSource{Url: u,
			RefKind: &hostv1.GitSource_ControlBranch{ControlBranch: &hostv1.ControlBranch{}}, DefaultBranch: "main"})}},
		"moduledir_set": {Moduledir: "thirdparty", Modules: []*hostv1.PuppetfileModule{forge("puppetlabs/ntp", &hostv1.ForgeSource{})}},
		// Phase 6 proved the non-ASCII guarantee on git-sourced modules: Forge
		// names are an ASCII slug.
		"git_non_ascii_name": {Modules: []*hostv1.PuppetfileModule{git("ntp-café", &hostv1.GitSource{Url: "https://example.com/ntp-café.git"})}},
	}
	for i, m := range roundTripFixtures {
		models["phase6_fixture_"+string(rune('a'+i))] = m
	}
	for name, m := range models {
		t.Run(name, func(t *testing.T) {
			rendered, err := RenderPuppetfile(m)
			if err != nil {
				t.Fatalf("RenderPuppetfile: %v", err)
			}
			got, fs := ParsePuppetfileLenient(rendered, DefaultImportLimits())
			if len(fs) != 0 {
				t.Fatalf("findings = %v for rendered %q, want none", findingKinds(fs), rendered)
			}
			if !proto.Equal(got, m) {
				t.Fatalf("lenient(Render(m)) != m:\n%v\n%v", got, m)
			}
		})
	}
}

func FuzzParsePuppetfileLenient(f *testing.F) {
	seed := func(dir string) string {
		b, err := os.ReadFile(filepath.Join("testdata", "import", "puppetfile", dir, "Puppetfile"))
		if err != nil {
			f.Fatalf("seed fixture %s: %v", dir, err)
		}
		return string(b)
	}
	f.Add(seed("canonical-control-repo"))
	f.Add(seed("ruby19"))
	f.Add(seed("constructs"))
	f.Add(seed("bom-crlf"))
	f.Add("")
	f.Add("\xef\xbb\xbf")
	f.Add("# only a comment")
	f.Add("mod 'unterminated")
	f.Add("mod 'a',\n  git: 'https://example.com/a.git',\n  branch: :control_branch\n")
	f.Add("moduledir 'x'\nmoduledir 'y'\nmod 'puppetlabs/ntp', '1.0'\nmod 'puppetlabs/ntp'\n")

	lim := DefaultImportLimits()
	f.Fuzz(func(t *testing.T, text string) {
		pf, fs := ParsePuppetfileLenient(text, lim)
		if pf == nil {
			t.Fatal("nil model")
		}

		// Invariant 2: finding lines stay inside the input (the findings cap
		// is not exercised by line, so the terminal finding obeys it too).
		nLines := strings.Count(text, "\n") + 1
		for _, fd := range fs {
			if fd.GetLine() < 1 || int(fd.GetLine()) > nLines {
				t.Fatalf("finding %s line %d outside 1..%d", fd.GetKind(), fd.GetLine(), nLines)
			}
			if !utf8.ValidString(fd.GetExcerpt()) || len(fd.GetExcerpt()) > lim.ExcerptBytes {
				t.Fatalf("finding %s excerpt invalid or over the cap: %q", fd.GetKind(), fd.GetExcerpt())
			}
			if fd.GetSeverity() != hostv1.ImportFinding_WARNING {
				t.Fatalf("finding %s has severity %v; a Puppetfile construct is never an error", fd.GetKind(), fd.GetSeverity())
			}
		}
		if len(fs) > lim.MaxFindingsPerBranch+1 {
			t.Fatalf("%d findings exceeds the cap %d plus one terminal", len(fs), lim.MaxFindingsPerBranch)
		}

		// Invariant 3: the load-bearing one. Every model the lenient parser
		// can produce renders, and the rendering strict-re-parses to the same
		// model, so ApplyImport's second pass can be infallible.
		rendered, err := RenderPuppetfile(pf)
		if err != nil {
			t.Fatalf("lenient output does not render: %v\ninput: %q", err, text)
		}
		back, err := ParsePuppetfile(rendered)
		if err != nil {
			t.Fatalf("rendered lenient output does not strict-parse: %v\nrendered: %q\ninput: %q", err, rendered, text)
		}
		if !proto.Equal(pf, back) {
			t.Fatalf("strict re-parse differs:\n%v\n%v\ninput: %q", pf, back, text)
		}
	})
}
