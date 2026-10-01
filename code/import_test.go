package code

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

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

// memFile is one entry of the in-memory ImportFS double.
type memFile struct {
	content []byte
	// mode defaults to "100644".
	mode string
	// size, when non-zero, is the size ImportFS reports instead of
	// len(content), so a size cap can be tested without megabytes of content.
	size    int64
	readErr error
}

// memFS is an in-memory ImportFS over a map, so AnalyzeBranch is tested with
// no git. It records every List pathspec and every Read so a test can assert
// what was and was not touched.
type memFS struct {
	files map[string]memFile
	// leaky makes List ignore its pathspecs and return every entry, which is
	// how a sloppy adapter would behave.
	leaky   bool
	listErr error
	listed  [][]string
	reads   []string
}

func newMemFS() *memFS { return &memFS{files: map[string]memFile{}} }

func (m *memFS) add(path, content string) *memFS {
	m.files[path] = memFile{content: []byte(content)}
	return m
}

func (m *memFS) addFile(path string, f memFile) *memFS {
	m.files[path] = f
	return m
}

func (m *memFS) List(pathspecs ...string) ([]ImportFile, error) {
	m.listed = append(m.listed, append([]string(nil), pathspecs...))
	if m.listErr != nil {
		return nil, m.listErr
	}
	var out []ImportFile
	for p, f := range m.files {
		if !m.leaky && len(pathspecs) > 0 {
			match := false
			for _, ps := range pathspecs {
				if p == ps || strings.HasPrefix(p, strings.TrimSuffix(ps, "/")+"/") {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		mode := f.mode
		if mode == "" {
			mode = "100644"
		}
		size := f.size
		if size == 0 {
			size = int64(len(f.content))
		}
		out = append(out, ImportFile{Path: p, Mode: mode, Size: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (m *memFS) Read(path string) ([]byte, error) {
	m.reads = append(m.reads, path)
	f, ok := m.files[path]
	if !ok {
		return nil, errors.New("memFS: no such file " + path)
	}
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.content, nil
}

func (m *memFS) wasRead(path string) bool {
	for _, r := range m.reads {
		if r == path {
			return true
		}
	}
	return false
}

// loadBranchFixture loads testdata/import/branches/<dir> into a memFS with
// repo-relative forward-slash paths.
func loadBranchFixture(t *testing.T, dir string) *memFS {
	t.Helper()
	root := filepath.Join("testdata", "import", "branches", dir)
	m := newMemFS()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		m.add(filepath.ToSlash(rel), string(b))
		return nil
	})
	if err != nil {
		t.Fatalf("load branch fixture %s: %v", dir, err)
	}
	return m
}

func findingsOfKind(fs []*hostv1.ImportFinding, kind string) []*hostv1.ImportFinding {
	var out []*hostv1.ImportFinding
	for _, f := range fs {
		if f.GetKind() == kind {
			out = append(out, f)
		}
	}
	return out
}

func TestAnalyzeBranch_CanonicalBranch(t *testing.T) {
	m := loadBranchFixture(t, "canonical")
	// The Puppetfile is the one 10-04 committed; reference it rather than
	// duplicating its bytes.
	pfText := readPuppetfileFixture(t, "canonical-control-repo")
	m.add("Puppetfile", pfText)

	snap, findings := AnalyzeBranch("production", m, DefaultImportLimits())
	if snap == nil {
		t.Fatal("AnalyzeBranch returned a nil snapshot")
	}

	t.Run("snapshot_is_populated", func(t *testing.T) {
		if snap.GetBranch() != "production" {
			t.Errorf("branch = %q", snap.GetBranch())
		}
		if !snap.GetImportable() {
			t.Error("a clean branch must be importable")
		}
		if snap.GetPuppetfileText() == "" {
			t.Error("puppetfile_text is empty")
		}
		if snap.GetHieraYaml() == "" {
			t.Error("hiera_yaml is empty")
		}
		if len(snap.GetDataFiles()) != 1 {
			t.Fatalf("data files = %d, want 1", len(snap.GetDataFiles()))
		}
		if snap.GetDataFiles()[0].GetPath() != "common.yaml" {
			t.Errorf("data file path = %q, want the datadir-relative common.yaml", snap.GetDataFiles()[0].GetPath())
		}
		if snap.GetSettings() == nil || snap.GetSettings().Modulepath == nil {
			t.Fatalf("settings = %v, want modulepath populated", snap.GetSettings())
		}
		if got := snap.GetSettings().GetModulepath(); got != "site-modules:modules:$basemodulepath" {
			t.Errorf("modulepath = %q", got)
		}
	})

	t.Run("exactly_two_warnings_no_error", func(t *testing.T) {
		kinds := findingKinds(findings)
		sort.Strings(kinds)
		want := []string{FindingHieraUnmodelledKey, FindingPuppetfileForgeDirective}
		sort.Strings(want)
		if strings.Join(kinds, ",") != strings.Join(want, ",") {
			t.Fatalf("finding kinds = %v, want %v", kinds, want)
		}
		for _, f := range findings {
			if f.GetSeverity() != hostv1.ImportFinding_WARNING {
				t.Errorf("finding %v is not a warning", f)
			}
		}
		if um := findingsOfKind(findings, FindingHieraUnmodelledKey); len(um) != 1 || um[0].GetExcerpt() != "plan_hierarchy" {
			t.Errorf("the unmodelled-key finding must name plan_hierarchy, got %v", um)
		}
	})

	t.Run("read_path_revalidation", func(t *testing.T) {
		// Puppetfile: the stored text strict-parses to the lenient model.
		lenient, _ := ParsePuppetfileLenient(pfText, DefaultImportLimits())
		strict, err := ParsePuppetfile(snap.GetPuppetfileText())
		if err != nil {
			t.Fatalf("strict ParsePuppetfile on the stored text: %v", err)
		}
		if !proto.Equal(strict, lenient) {
			t.Errorf("strict(stored text) != lenient model:\n%v\n%v", strict, lenient)
		}
		// Hierarchy.
		h, err := ParseHierarchy(snap.GetHieraYaml())
		if err != nil {
			t.Fatalf("ParseHierarchy on the stored hierarchy: %v", err)
		}
		if h.GetVersion() != 5 || len(h.GetLevels()) != 1 {
			t.Errorf("hierarchy = %v", h)
		}
		// Data files.
		for _, df := range snap.GetDataFiles() {
			if err := ValidateDataPath(df.GetPath()); err != nil {
				t.Errorf("ValidateDataPath(%q): %v", df.GetPath(), err)
			}
			if _, err := ParseDataFile(df.GetYaml()); err != nil {
				t.Errorf("ParseDataFile(%q): %v", df.GetPath(), err)
			}
		}
		// Settings.
		text, err := RenderEnvConf(snap.GetSettings())
		if err != nil {
			t.Fatalf("RenderEnvConf: %v", err)
		}
		back, err := ParseEnvConf(text)
		if err != nil {
			t.Fatalf("ParseEnvConf: %v", err)
		}
		if !proto.Equal(back, snap.GetSettings()) {
			t.Errorf("settings do not survive render+parse:\n%v\n%v", back, snap.GetSettings())
		}
	})

	t.Run("findings_are_stamped", func(t *testing.T) {
		if len(findings) == 0 {
			t.Fatal("no findings to inspect; the stamping assertions would be vacuous")
		}
		for _, f := range findings {
			if f.GetBranch() != "production" {
				t.Errorf("finding %v has no branch", f)
			}
			if f.GetFile() == "" {
				t.Errorf("file-scoped finding %v has no file", f)
			}
		}
		if f := findingsOfKind(findings, FindingPuppetfileForgeDirective); len(f) == 1 && f[0].GetFile() != "Puppetfile" {
			t.Errorf("forge-directive finding file = %q", f[0].GetFile())
		}
		if f := findingsOfKind(findings, FindingHieraUnmodelledKey); len(f) == 1 && f[0].GetFile() != "hiera.yaml" {
			t.Errorf("unmodelled-key finding file = %q", f[0].GetFile())
		}
		if len(snap.GetFindings()) != len(findings) {
			t.Errorf("snapshot carries %d findings, returned %d", len(snap.GetFindings()), len(findings))
		}
	})
}

func TestAnalyzeBranch_EmptyBranch(t *testing.T) {
	snap, findings := AnalyzeBranch("empty", newMemFS(), DefaultImportLimits())
	if snap == nil {
		t.Fatal("nil snapshot")
	}
	if snap.GetPuppetfileText() != "" || snap.GetHieraYaml() != "" || snap.GetSettings() != nil || len(snap.GetDataFiles()) != 0 {
		t.Errorf("an empty branch must import nothing, got %v", snap)
	}
	if len(findings) != 0 {
		t.Errorf("an empty branch must produce no findings, got %v", findingKinds(findings))
	}
	if !snap.GetImportable() {
		t.Error("an empty branch is importable: it makes an environment with nothing in it")
	}
}

func TestAnalyzeBranch_ModuleTreesAreNeverWalked(t *testing.T) {
	m := loadBranchFixture(t, "canonical")
	m.add("modules/apache/hiera.yaml", "version: 5\nhierarchy:\n  - name: x\n    path: x.yaml\n")
	m.add("site-modules/profile/hiera.yaml", ":backends:\n  - yaml\n")
	m.add("modules/apache/data/common.yaml", "a: 1\n")
	snap, findings := AnalyzeBranch("production", m, DefaultImportLimits())
	for _, f := range findings {
		if strings.HasPrefix(f.GetFile(), "modules/") || strings.HasPrefix(f.GetFile(), "site-modules/") {
			t.Errorf("finding for a module tree: %v", f)
		}
	}
	for _, df := range snap.GetDataFiles() {
		if strings.Contains(df.GetPath(), "apache") {
			t.Errorf("a module data file was imported: %v", df)
		}
	}
	for _, r := range m.reads {
		if strings.HasPrefix(r, "modules/") || strings.HasPrefix(r, "site-modules/") {
			t.Errorf("a module tree file was read: %s", r)
		}
	}
	for _, spec := range m.listed {
		for _, ps := range spec {
			if strings.HasPrefix(ps, "modules") || strings.HasPrefix(ps, "site-modules") {
				t.Errorf("a module tree was listed: %v", spec)
			}
		}
	}
}
