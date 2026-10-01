package code

import (
	"errors"
	"fmt"
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

// snapshotBytes is the stored size of a snapshot's texts: the bytes the
// total-snapshot budget bounds.
func snapshotBytes(s *hostv1.ImportBranchSnapshot) int64 {
	n := int64(len(s.GetPuppetfileText()) + len(s.GetHieraYaml()))
	for _, d := range s.GetDataFiles() {
		n += int64(len(d.GetYaml()))
	}
	return n
}

// dataBranch builds a branch whose hierarchy has no levels, so it reads the
// default datadir, with n ten-byte data files named a.yaml, b.yaml, ...
func dataBranch(n int) *memFS {
	m := newMemFS().add("hiera.yaml", "version: 5\nhierarchy: []\n")
	for i := 0; i < n; i++ {
		m.add(fmt.Sprintf("data/%c.yaml", 'a'+i), "k: 123456\n") // exactly 10 bytes
	}
	return m
}

func TestAnalyzeBranch_DataBudget(t *testing.T) {
	t.Run("per_branch_data_cap_stops_the_walk_with_one_warning_and_keeps_what_was_imported", func(t *testing.T) {
		m := dataBranch(4)
		snap, fs := AnalyzeBranch("production", m, ImportLimits{MaxBranchBytes: 25})
		if got := strings.Join(dataPaths(snap), ","); got != "a.yaml,b.yaml" {
			t.Fatalf("data files = %q, want the two that fit in 25 bytes", got)
		}
		wantKindSev(t, fs, ks(FindingBranchDataCapExceeded, sevW))
		if !strings.Contains(fs[0].GetMessage(), "MaxBranchBytes") || !strings.Contains(fs[0].GetMessage(), "25") {
			t.Errorf("the warning must name the cap and its value: %q", fs[0].GetMessage())
		}
		if fs[0].GetBranch() != "production" {
			t.Errorf("finding not stamped: %v", fs[0])
		}
	})

	t.Run("the_file_that_would_exceed_the_budget_is_not_read", func(t *testing.T) {
		m := dataBranch(3)
		AnalyzeBranch("production", m, ImportLimits{MaxBranchBytes: 15})
		if !m.wasRead("data/a.yaml") {
			t.Error("the first file fits and must be read")
		}
		if m.wasRead("data/b.yaml") || m.wasRead("data/c.yaml") {
			t.Error("the budget must be enforced from the reported size, before any read (D-12, DQ-8)")
		}
	})

	t.Run("total_snapshot_cap_is_enforced_the_same_way", func(t *testing.T) {
		m := dataBranch(4)
		lim := ImportLimits{MaxSnapshotBytes: 45} // 26 bytes of hiera.yaml leave room for one 10-byte file
		snap, fs := AnalyzeBranch("production", m, lim)
		wantKindSev(t, fs, ks(FindingBranchDataCapExceeded, sevW))
		if !strings.Contains(fs[0].GetMessage(), "MaxSnapshotBytes") {
			t.Errorf("the warning must name the total cap: %q", fs[0].GetMessage())
		}
		if got := snapshotBytes(snap); got > 45 {
			t.Fatalf("snapshot is %d bytes, over the 45-byte total cap", got)
		}
		if len(snap.GetDataFiles()) != 1 {
			t.Fatalf("data files = %v", dataPaths(snap))
		}
	})

	t.Run("a_branch_inside_both_caps_has_no_cap_finding", func(t *testing.T) {
		snap, fs := AnalyzeBranch("production", dataBranch(4), DefaultImportLimits())
		wantKindSev(t, fs)
		if len(snap.GetDataFiles()) != 4 {
			t.Fatalf("data files = %v", dataPaths(snap))
		}
	})

	t.Run("budgets_are_read_from_the_limits_value", func(t *testing.T) {
		// Zero fields fall back to the defaults, so a zero ImportLimits must
		// not make every file exceed a zero budget.
		snap, fs := AnalyzeBranch("production", dataBranch(2), ImportLimits{})
		wantKindSev(t, fs)
		if len(snap.GetDataFiles()) != 2 {
			t.Fatalf("data files = %v", dataPaths(snap))
		}
	})
}

func TestAnalyzeBranch_FindingsCap(t *testing.T) {
	m := newMemFS().add("environment.conf", "a = 1\nb = 2\nc = 3\nd = 4\ne = 5\nf = 6\n")
	snap, fs := AnalyzeBranch("production", m, ImportLimits{MaxFindingsPerBranch: 3})
	if len(fs) != 4 {
		t.Fatalf("findings = %v, want cap(3) plus one terminal", kindSev(fs))
	}
	last := fs[3]
	if last.GetKind() != FindingFindingsTruncated || last.GetSeverity() != hostv1.ImportFinding_WARNING {
		t.Fatalf("terminal finding = %v", last)
	}
	if len(findingsOfKind(fs, FindingFindingsTruncated)) != 1 {
		t.Errorf("exactly one terminal finding is allowed, got %v", kindSev(fs))
	}
	for _, f := range fs {
		if f.GetBranch() != "production" {
			t.Errorf("finding %v is not stamped with the branch, the terminal one included", f)
		}
	}
	if len(snap.GetFindings()) != 4 {
		t.Errorf("snapshot carries %d findings", len(snap.GetFindings()))
	}
}

func TestAnalyzeBranch_RootFileGates(t *testing.T) {
	t.Run("symlinked_root_files_are_never_read", func(t *testing.T) {
		m := newMemFS()
		m.addFile("Puppetfile", memFile{content: []byte("/etc/passwd"), mode: "120000"})
		m.addFile("hiera.yaml", memFile{content: []byte("/etc/passwd"), mode: "120000"})
		m.addFile("environment.conf", memFile{content: []byte("/etc/passwd"), mode: "160000"})
		snap, fs := AnalyzeBranch("production", m, DefaultImportLimits())
		wantKindSev(t, fs, ks(FindingBranchFileUnreadable, sevE), ks(FindingBranchFileUnreadable, sevE), ks(FindingBranchFileUnreadable, sevE))
		if len(m.reads) != 0 {
			t.Fatalf("a symlink or gitlink was read: %v", m.reads)
		}
		if snap.GetPuppetfileText() != "" || snap.GetHieraYaml() != "" || snap.GetSettings() != nil {
			t.Fatalf("nothing may be imported from them, got %v", snap)
		}
	})

	t.Run("oversized_root_file_is_not_read", func(t *testing.T) {
		m := newMemFS()
		m.addFile("hiera.yaml", memFile{content: []byte("version: 5\n"), size: 5000})
		snap, fs := AnalyzeBranch("production", m, ImportLimits{MaxFileBytes: 1000})
		wantKindSev(t, fs, ks(FindingBranchFileUnreadable, sevE))
		if m.wasRead("hiera.yaml") || snap.GetHieraYaml() != "" {
			t.Fatal("an oversized root file must not be read or imported")
		}
	})

	t.Run("a_listing_failure_is_a_branch_level_error_and_the_branch_is_not_importable", func(t *testing.T) {
		m := newMemFS()
		m.listErr = errors.New("boom")
		snap, fs := AnalyzeBranch("production", m, DefaultImportLimits())
		wantKindSev(t, fs, ks(FindingBranchFileUnreadable, sevE))
		if snap.GetImportable() {
			t.Error("a branch that could not be listed is not importable")
		}
	})

	t.Run("an_environment_conf_that_is_all_comments_writes_no_settings", func(t *testing.T) {
		snap, fs := AnalyzeBranch("production", newMemFS().add("environment.conf", "# nothing\n"), DefaultImportLimits())
		wantKindSev(t, fs)
		if snap.GetSettings() != nil {
			t.Fatalf("settings = %v, want absent", snap.GetSettings())
		}
	})
}

func TestReadPathGuards(t *testing.T) {
	t.Run("settings_with_a_newline_fail_the_round_trip", func(t *testing.T) {
		bad := &hostv1.EnvironmentSettings{Modulepath: strPtr("a\nb = c")}
		if err := settingsReadPath(bad); err == nil {
			t.Fatal("settingsReadPath accepted a value RenderEnvConf refuses")
		}
	})
	t.Run("a_hierarchy_that_reads_back_different_fails", func(t *testing.T) {
		h := &hostv1.HieraHierarchy{Version: 5, DefaultDatadir: "other"}
		if err := hierarchyReadPath("version: 5\n", h); err == nil {
			t.Fatal("hierarchyReadPath accepted a text that reads back as a different model")
		}
		if err := hierarchyReadPath("- not a mapping\n", h); err == nil {
			t.Fatal("hierarchyReadPath accepted unparseable text")
		}
	})
	t.Run("a_puppetfile_that_does_not_render_fails", func(t *testing.T) {
		pf := &hostv1.Puppetfile{Modules: []*hostv1.PuppetfileModule{{Name: "stdlib"}}}
		if _, err := puppetfileReadPath(pf); err == nil {
			t.Fatal("puppetfileReadPath accepted a model that cannot render")
		}
	})
}
