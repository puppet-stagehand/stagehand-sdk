package code

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// kindSev renders findings as sorted "kind/SEVERITY" tokens, so a test pins
// both the catalog kind and the severity split (D-10) in one comparison.
func kindSev(fs []*hostv1.ImportFinding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		sev := "WARNING"
		if f.GetSeverity() == hostv1.ImportFinding_ERROR {
			sev = "ERROR"
		}
		out = append(out, f.GetKind()+"/"+sev)
	}
	sort.Strings(out)
	return out
}

func wantKindSev(t *testing.T, got []*hostv1.ImportFinding, want ...string) {
	t.Helper()
	w := append([]string(nil), want...)
	sort.Strings(w)
	g := kindSev(got)
	if strings.Join(g, ",") != strings.Join(w, ",") {
		t.Fatalf("findings = %v, want %v", g, w)
	}
}

func readHieraFixture(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "import", "hiera", dir, "hiera.yaml"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", dir, err)
	}
	return string(b)
}

func readBranchFixtureFile(t *testing.T, branch, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "import", "branches", branch, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read fixture %s/%s: %v", branch, rel, err)
	}
	return string(b)
}

const (
	sevW = "WARNING"
	sevE = "ERROR"
)

func ks(kind, sev string) string { return kind + "/" + sev }

// TestParseHierarchyLenient is the hiera.yaml half of the RESEARCH catalog:
// one row per construct, asserting the findings, their severities, and
// whether the hierarchy is present in the result.
func TestParseHierarchyLenient(t *testing.T) {
	unmodelled := ks(FindingHieraUnmodelledKey, sevW)
	versionErr := ks(FindingHieraVersionUnsupported, sevE)
	unparseable := ks(FindingHieraUnparseable, sevE)
	datadirBad := ks(FindingHieraDatadirUnresolvable, sevW)

	rows := []struct {
		name       string
		text       string
		importable bool
		want       []string
		// excerpts, when set, are the sorted finding excerpts.
		excerpts []string
	}{
		{name: "canonical_control_repo", text: readBranchFixtureFile(t, "canonical", "hiera.yaml"), importable: true,
			want: []string{unmodelled}, excerpts: []string{"plan_hierarchy"}},
		{name: "hiera_v3_fixture", text: readHieraFixture(t, "v3"), importable: false, want: []string{versionErr}},
		{name: "version_4", text: "version: 4\nhierarchy: []\n", importable: false, want: []string{versionErr}},
		{name: "no_version_key", text: "hierarchy:\n  - name: x\n    path: x.yaml\n", importable: false, want: []string{versionErr}},
		{name: "version_is_a_quoted_string", text: "version: \"5\"\n", importable: false, want: []string{versionErr}},
		{name: "version_is_a_float", text: "version: 5.0\n", importable: false, want: []string{versionErr}},
		{name: "empty_text", text: "", importable: false, want: []string{versionErr}},
		{name: "comment_only", text: "# nothing here\n", importable: false, want: []string{versionErr}},
		{name: "eyaml_level", text: readHieraFixture(t, "eyaml"), importable: true,
			want: []string{unmodelled, unmodelled}, excerpts: []string{"lookup_key", "options"}},
		{name: "globs_uri_data_dig_hiera3_backend_default_hierarchy", text: readHieraFixture(t, "globs"), importable: true,
			want:     []string{unmodelled, unmodelled, unmodelled, unmodelled, unmodelled, unmodelled},
			excerpts: []string{"data_dig", "default_hierarchy", "globs", "hiera3_backend", "uri", "uris"}},
		{name: "top_level_plan_hierarchy", text: "version: 5\nplan_hierarchy:\n  - name: p\n    path: p.yaml\n", importable: true,
			want: []string{unmodelled}, excerpts: []string{"plan_hierarchy"}},
		{name: "defaults_lookup_key_and_options", text: "version: 5\ndefaults:\n  datadir: data\n  lookup_key: eyaml_lookup_key\n  options:\n    a: b\n", importable: true,
			want: []string{unmodelled, unmodelled}, excerpts: []string{"lookup_key", "options"}},
		{name: "level_without_name", text: "version: 5\nhierarchy:\n  - path: x.yaml\n", importable: true,
			want: []string{ks(FindingHieraLevelUnnamed, sevW)}},
		{name: "duplicate_level_names", text: "version: 5\nhierarchy:\n  - name: a\n    path: a.yaml\n  - name: a\n    path: b.yaml\n", importable: true,
			want: []string{ks(FindingHieraDuplicateLevel, sevW)}},
		{name: "paths_is_a_scalar", text: "version: 5\nhierarchy:\n  - name: a\n    paths: x.yaml\n", importable: false, want: []string{unparseable}},
		{name: "hierarchy_is_a_mapping", text: "version: 5\nhierarchy:\n  name: a\n", importable: false, want: []string{unparseable}},
		{name: "level_is_not_a_mapping", text: "version: 5\nhierarchy:\n  - just-a-string\n", importable: false, want: []string{unparseable}},
		{name: "top_level_is_a_sequence", text: "- a\n- b\n", importable: false, want: []string{unparseable}},
		{name: "not_yaml", text: "version: 5\n\t- broken: [\n", importable: false, want: []string{unparseable}},
		{name: "datadir_with_interpolation", text: "version: 5\nhierarchy:\n  - name: a\n    datadir: \"data/%{environment}\"\n    path: x.yaml\n", importable: true,
			want: []string{datadirBad}},
		{name: "datadir_absolute", text: "version: 5\nhierarchy:\n  - name: a\n    datadir: /etc/puppet/data\n    path: x.yaml\n", importable: true,
			want: []string{datadirBad}},
		{name: "datadir_with_dotdot", text: "version: 5\nhierarchy:\n  - name: a\n    datadir: ../data\n    path: x.yaml\n", importable: true,
			want: []string{datadirBad}},
		{name: "defaults_datadir_unresolvable", text: "version: 5\ndefaults:\n  datadir: /abs\nhierarchy:\n  - name: a\n    path: x.yaml\n", importable: true,
			want: []string{datadirBad}},
		{name: "datadir_resolves_to_the_environment_root", text: "version: 5\nhierarchy:\n  - name: a\n    datadir: .\n    path: x.yaml\n", importable: true,
			want: []string{datadirBad}},
		{name: "datadir_inside_a_module_tree", text: "version: 5\nhierarchy:\n  - name: a\n    datadir: modules/x/data\n    path: x.yaml\n", importable: true,
			want: []string{datadirBad}},
		{name: "bom_prefix", text: "\xEF\xBB\xBFversion: 5\nhierarchy:\n  - name: a\n    path: a.yaml\n", importable: true, want: nil},
		{name: "version_only", text: "version: 5\n", importable: true, want: nil},
		{name: "plain_valid_hierarchy", text: "version: 5\ndefaults:\n  datadir: data\n  data_hash: yaml_data\nhierarchy:\n  - name: a\n    path: a.yaml\n  - name: b\n    paths:\n      - b.yaml\n    mapped_paths: [x, y, \"z\"]\n    glob: \"g/*.yaml\"\n    data_hash: yaml_data\n", importable: true, want: nil},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			h, raw, fs, ok := ParseHierarchyLenient(tc.text, DefaultImportLimits())
			wantKindSev(t, fs, tc.want...)
			if ok != tc.importable {
				t.Fatalf("importable = %v, want %v (findings %v)", ok, tc.importable, kindSev(fs))
			}
			if !tc.importable {
				// An error row must be ABSENT from the result, not only reported.
				if h != nil || raw != "" {
					t.Fatalf("a hierarchy that is not importable must contribute nothing, got model %v text %q (T-10-19)", h, raw)
				}
				return
			}
			if h == nil || raw == "" {
				t.Fatalf("importable hierarchy missing: model %v text %q", h, raw)
			}
			if strings.HasPrefix(raw, utf8BOM) {
				t.Errorf("retained text still starts with a BOM")
			}
			// The retained text is what the facet will read back.
			if err := hierarchyReadPath(raw, h); err != nil {
				t.Errorf("retained text fails its own read path: %v", err)
			}
			if tc.excerpts != nil {
				var got []string
				for _, f := range fs {
					got = append(got, f.GetExcerpt())
				}
				sort.Strings(got)
				if strings.Join(got, ",") != strings.Join(tc.excerpts, ",") {
					t.Errorf("finding excerpts = %v, want %v", got, tc.excerpts)
				}
			}
		})
	}

	t.Run("every_finding_has_a_line", func(t *testing.T) {
		_, _, fs, _ := ParseHierarchyLenient(readHieraFixture(t, "globs"), DefaultImportLimits())
		for _, f := range fs {
			if f.GetLine() < 1 {
				t.Errorf("finding %v has no line", f)
			}
		}
	})

	t.Run("v3_is_never_an_importable_empty_hierarchy", func(t *testing.T) {
		// The strict parser accepts this file as version 0 with no levels:
		// that silent success is the failure this plan exists to close.
		strict, err := ParseHierarchy(readHieraFixture(t, "v3"))
		if err != nil || strict.GetVersion() != 0 || len(strict.GetLevels()) != 0 {
			t.Fatalf("test premise broken: strict = %v, %v", strict, err)
		}
		_, _, fs, ok := ParseHierarchyLenient(readHieraFixture(t, "v3"), DefaultImportLimits())
		if ok || len(fs) != 1 {
			t.Fatalf("lenient accepted a v3 file or stayed silent: ok=%v findings=%v", ok, kindSev(fs))
		}
		if !strings.Contains(fs[0].GetMessage(), "v3") {
			t.Errorf("message should say it looks like Hiera v3: %q", fs[0].GetMessage())
		}
	})

	t.Run("eyaml_text_is_retained_verbatim_and_message_names_the_rewrite_risk", func(t *testing.T) {
		text := readHieraFixture(t, "eyaml")
		_, raw, fs, ok := ParseHierarchyLenient(text, DefaultImportLimits())
		if !ok || raw != text {
			t.Fatalf("eyaml hierarchy must be retained verbatim (DQ-4); ok=%v equal=%v", ok, raw == text)
		}
		if !strings.Contains(raw, "lookup_key: eyaml_lookup_key") || !strings.Contains(raw, "options:") {
			t.Fatal("the stored text lost the backend keys")
		}
		for _, f := range fs {
			if !strings.Contains(f.GetMessage(), "PutHieraLevel") || !strings.Contains(f.GetMessage(), "drop") {
				t.Errorf("level-scoped message must say a PutHieraLevel rewrite would drop the key: %q", f.GetMessage())
			}
			if !strings.Contains(f.GetMessage(), f.GetExcerpt()) {
				t.Errorf("message must name the key %q: %q", f.GetExcerpt(), f.GetMessage())
			}
		}
	})

	t.Run("duplicate_and_unnamed_levels_keep_the_hierarchy", func(t *testing.T) {
		h, _, _, ok := ParseHierarchyLenient("version: 5\nhierarchy:\n  - name: a\n    path: a.yaml\n  - name: a\n    path: b.yaml\n", DefaultImportLimits())
		if !ok || len(h.GetLevels()) != 2 {
			t.Fatalf("both levels must be kept; ok=%v levels=%d", ok, len(h.GetLevels()))
		}
	})
}

// TestParseDataFileLenient is the data-file half of the RESEARCH catalog
// that a single file's text decides.
func TestParseDataFileLenient(t *testing.T) {
	unparseable := ks(FindingDataFileUnparseable, sevE)
	deep := "a: " + strings.Repeat("[", 100) + strings.Repeat("]", 100) + "\n"
	bomb := "a: &a [x,x,x,x,x,x,x,x,x,x]\n"
	prev := "a"
	for i := 0; i < 12; i++ {
		cur := fmt.Sprintf("l%d", i)
		bomb += fmt.Sprintf("%s: &%s [*%s,*%s,*%s,*%s,*%s,*%s,*%s,*%s,*%s]\n", cur, cur, prev, prev, prev, prev, prev, prev, prev, prev, prev)
		prev = cur
	}

	rows := []struct {
		name     string
		path     string
		text     string
		imported bool
		want     []string
		cause    string
	}{
		{name: "ordinary_mapping", path: "common.yaml", text: "ntp::servers:\n  - a\nport: 8080\n", imported: true},
		{name: "only_document_marker", path: "common.yaml", text: "---\n", imported: true},
		{name: "only_comments", path: "common.yaml", text: "# nothing yet\n", imported: true},
		{name: "null_document", path: "common.yaml", text: "~\n", imported: true},
		{name: "whitespace_only", path: "common.yaml", text: "  \n\n", imported: true},
		{name: "integer_keys", path: "common.yaml", text: "1: a\n2: b\n", imported: true},
		{name: "bom_prefix", path: "common.yaml", text: "\xEF\xBB\xBFa: 1\n", imported: true},
		{name: "nested_path", path: "nodes/web01.example.com.yaml", text: "a: 1\n", imported: true},
		{name: "multi_document", path: "common.yaml", text: "a: 1\n---\nb: 2\n", imported: true,
			want: []string{ks(FindingDataFileExtraDocuments, sevW)}},
		{name: "top_level_sequence", path: "common.yaml", text: "- a\n- b\n", want: []string{unparseable}, cause: "sequence or a scalar"},
		{name: "top_level_scalar", path: "common.yaml", text: "just text\n", want: []string{unparseable}, cause: "sequence or a scalar"},
		{name: "git_lfs_pointer", path: "common.yaml", text: "version https://git-lfs.github.com/spec/v1\noid sha256:4d7a\nsize 12345\n", want: []string{unparseable}, cause: "LFS"},
		{name: "unquoted_timestamp", path: "common.yaml", text: "released: 2020-01-01\n", want: []string{unparseable}, cause: "timestamp"},
		{name: "invalid_utf8", path: "common.yaml", text: "a: \xff\xfe\n", want: []string{unparseable}, cause: "UTF-8"},
		{name: "alias_bomb", path: "common.yaml", text: bomb, want: []string{unparseable}, cause: "alias"},
		{name: "deeply_nested", path: "common.yaml", text: deep, want: []string{unparseable}, cause: "nested"},
		{name: "deeply_nested_past_the_library_limit", path: "common.yaml", text: "a: " + strings.Repeat("[", 20000) + strings.Repeat("]", 20000) + "\n", want: []string{unparseable}, cause: "deeper"},
		{name: "not_yaml", path: "common.yaml", text: "a: [unterminated\n", want: []string{unparseable}},
		{name: "path_with_dotdot", path: "../escape.yaml", text: "a: 1\n", want: []string{unparseable}, cause: "path"},
		{name: "absolute_path", path: "/etc/x.yaml", text: "a: 1\n", want: []string{unparseable}, cause: "path"},
		{name: "backslash_path", path: `a\b.yaml`, text: "a: 1\n", want: []string{unparseable}, cause: "path"},
		{name: "empty_path", path: "", text: "a: 1\n", want: []string{unparseable}, cause: "path"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			df, fs := ParseDataFileLenient(tc.path, tc.text, DefaultImportLimits())
			wantKindSev(t, fs, tc.want...)
			if tc.imported {
				if df == nil || df.GetPath() != tc.path || df.GetYaml() != tc.text {
					t.Fatalf("file must be imported verbatim under %q, got %v", tc.path, df)
				}
				if _, err := ParseDataFile(df.GetYaml()); err != nil {
					t.Fatalf("imported file fails the facet's own read path: %v", err)
				}
				return
			}
			// An error row must be ABSENT, not only reported.
			if df != nil {
				t.Fatalf("an unparseable data file must be absent from the result, got %v", df)
			}
			if tc.cause != "" && !strings.Contains(fs[0].GetMessage(), tc.cause) {
				t.Errorf("message must name the cause %q: %q", tc.cause, fs[0].GetMessage())
			}
		})
	}

	t.Run("a_decode_error_is_not_confused_with_ErrHieraInvalid", func(t *testing.T) {
		// The timestamp case really is the strict parser's own failure.
		if _, err := ParseDataFile("released: 2020-01-01\n"); !errors.Is(err, ErrHieraInvalid) {
			t.Fatalf("test premise broken: %v", err)
		}
	})
}

// branchWith builds a branch with a hiera.yaml and the given data files.
func branchWith(hiera string, files map[string]string) *memFS {
	m := newMemFS()
	if hiera != "" {
		m.add("hiera.yaml", hiera)
	}
	for p, c := range files {
		m.add(p, c)
	}
	return m
}

func dataPaths(snap *hostv1.ImportBranchSnapshot) []string {
	var out []string
	for _, d := range snap.GetDataFiles() {
		out = append(out, d.GetPath())
	}
	sort.Strings(out)
	return out
}

// TestParseDataFileLenient_BranchGates covers the gates that need the file's
// listing entry or its place in the hierarchy rather than only its text.
func TestParseDataFileLenient_BranchGates(t *testing.T) {
	v5 := func(body string) string { return "version: 5\n" + body }

	t.Run("default_datadir_is_data_next_to_hiera_yaml", func(t *testing.T) {
		m := branchWith(v5("hierarchy:\n  - name: c\n    path: common.yaml\n"), map[string]string{"data/common.yaml": "a: 1\n", "other/x.yaml": "b: 2\n"})
		snap, _ := AnalyzeBranch("production", m, DefaultImportLimits())
		if got := strings.Join(dataPaths(snap), ","); got != "common.yaml" {
			t.Fatalf("data files = %q, want only data/common.yaml as common.yaml", got)
		}
	})

	t.Run("level_datadir_overrides_defaults_and_each_file_imports_by_relative_path", func(t *testing.T) {
		m := branchWith(v5("defaults:\n  datadir: data\nhierarchy:\n  - name: a\n    path: a.yaml\n  - name: b\n    datadir: extra\n    path: b.yaml\n"),
			map[string]string{"data/a.yaml": "a: 1\n", "data/nodes/n.yaml": "n: 1\n", "extra/b.yaml": "b: 1\n"})
		snap, fs := AnalyzeBranch("production", m, DefaultImportLimits())
		if got := strings.Join(dataPaths(snap), ","); got != "a.yaml,b.yaml,nodes/n.yaml" {
			t.Fatalf("data files = %q", got)
		}
		wantKindSev(t, fs)
	})

	t.Run("dot_slash_and_trailing_slash_datadirs_are_normalised", func(t *testing.T) {
		m := branchWith(v5("defaults:\n  datadir: ./data/\nhierarchy:\n  - name: a\n    path: a.yaml\n"), map[string]string{"data/a.yaml": "a: 1\n"})
		snap, _ := AnalyzeBranch("production", m, DefaultImportLimits())
		if got := strings.Join(dataPaths(snap), ","); got != "a.yaml" {
			t.Fatalf("data files = %q", got)
		}
	})

	for _, tc := range []struct{ name, datadir string }{
		{"interpolated", `"data/%{environment}"`},
		{"absolute", "/srv/hieradata"},
		{"dotdot", "../data"},
	} {
		t.Run("unresolvable_datadir_imports_nothing_for_that_level_only/"+tc.name, func(t *testing.T) {
			m := branchWith(v5("defaults:\n  datadir: data\nhierarchy:\n  - name: bad\n    datadir: "+tc.datadir+"\n    path: x.yaml\n  - name: good\n    path: a.yaml\n"),
				map[string]string{"data/a.yaml": "a: 1\n", "srv/hieradata/x.yaml": "x: 1\n"})
			snap, fs := AnalyzeBranch("production", m, DefaultImportLimits())
			wantKindSev(t, fs, ks(FindingHieraDatadirUnresolvable, sevW))
			if got := strings.Join(dataPaths(snap), ","); got != "a.yaml" {
				t.Fatalf("data files = %q, want only the good level's a.yaml", got)
			}
			if m.wasRead("srv/hieradata/x.yaml") {
				t.Error("an unresolvable datadir was followed")
			}
		})
	}

	t.Run("multi_datadir_collision_keeps_the_first_in_hierarchy_order", func(t *testing.T) {
		m := branchWith(readHieraFixture(t, "multi-datadir"), map[string]string{
			"data/common.yaml":      "winner: first\n",
			"site-data/common.yaml": "winner: second\n",
			"site-data/only.yaml":   "only: 1\n",
		})
		snap, fs := AnalyzeBranch("production", m, DefaultImportLimits())
		wantKindSev(t, fs, ks(FindingDataFileCollision, sevW))
		if got := strings.Join(dataPaths(snap), ","); got != "common.yaml,only.yaml" {
			t.Fatalf("data files = %q", got)
		}
		for _, d := range snap.GetDataFiles() {
			if d.GetPath() == "common.yaml" && d.GetYaml() != "winner: first\n" {
				t.Errorf("kept content = %q, want the first datadir's", d.GetYaml())
			}
		}
		if fs[0].GetFile() != "site-data/common.yaml" {
			t.Errorf("the collision finding must name the dropped file, got %q", fs[0].GetFile())
		}
		if !strings.Contains(fs[0].GetMessage(), "datadir") {
			t.Errorf("message must explain the model has no datadir dimension: %q", fs[0].GetMessage())
		}
	})

	t.Run("non_yaml_files_are_skipped_with_a_warning_and_never_read", func(t *testing.T) {
		m := branchWith(v5("hierarchy: []\n"), map[string]string{
			"data/a.json": "{}", "data/b.eyaml": "ENC[x]", "data/noext": "a: 1\n", "data/ok.yaml": "a: 1\n", "data/also.yml": "a: 1\n",
		})
		snap, fs := AnalyzeBranch("production", m, DefaultImportLimits())
		wantKindSev(t, fs, ks(FindingDataFileNotYAML, sevW), ks(FindingDataFileNotYAML, sevW), ks(FindingDataFileNotYAML, sevW))
		for _, p := range []string{"data/a.json", "data/b.eyaml", "data/noext"} {
			if m.wasRead(p) {
				t.Errorf("%s was read", p)
			}
		}
		if got := strings.Join(dataPaths(snap), ","); got != "also.yml,ok.yaml" {
			t.Fatalf("data files = %q (both .yaml and .yml are YAML)", got)
		}
	})

	t.Run("symlink_and_gitlink_are_never_read_through", func(t *testing.T) {
		m := branchWith(v5("hierarchy: []\n"), nil)
		m.addFile("data/evil.yaml", memFile{content: []byte("/etc/passwd"), mode: "120000"})
		m.addFile("data/sub.yaml", memFile{content: []byte("deadbeef"), mode: "160000"})
		m.add("data/ok.yaml", "a: 1\n")
		snap, fs := AnalyzeBranch("production", m, DefaultImportLimits())
		wantKindSev(t, fs, ks(FindingDataFileSymlink, sevW), ks(FindingDataFileGitlink, sevW))
		if m.wasRead("data/evil.yaml") || m.wasRead("data/sub.yaml") {
			t.Fatal("the mode check must precede the read (T-10-21)")
		}
		if got := strings.Join(dataPaths(snap), ","); got != "ok.yaml" {
			t.Fatalf("data files = %q", got)
		}
	})

	t.Run("oversized_file_is_not_read_and_the_message_names_the_cap", func(t *testing.T) {
		m := branchWith(v5("hierarchy: []\n"), nil)
		m.addFile("data/big.yaml", memFile{content: []byte("a: 1\n"), size: 5000})
		m.add("data/small.yaml", "a: 1\n")
		snap, fs := AnalyzeBranch("production", m, ImportLimits{MaxFileBytes: 1000})
		wantKindSev(t, fs, ks(FindingDataFileTooLarge, sevW))
		if m.wasRead("data/big.yaml") {
			t.Fatal("the size check must use the reported size, before any read (D-12)")
		}
		if !strings.Contains(fs[0].GetMessage(), "1000") {
			t.Errorf("message must name the cap: %q", fs[0].GetMessage())
		}
		if got := strings.Join(dataPaths(snap), ","); got != "small.yaml" {
			t.Fatalf("data files = %q", got)
		}
	})

	t.Run("file_outside_every_datadir_is_a_warning_and_not_imported", func(t *testing.T) {
		m := branchWith(v5("hierarchy: []\n"), map[string]string{"data/in.yaml": "a: 1\n", "elsewhere/out.yaml": "b: 2\n"})
		m.leaky = true // an adapter that ignores pathspecs returns everything
		snap, fs := AnalyzeBranch("production", m, DefaultImportLimits())
		wantKindSev(t, fs, ks(FindingDataFileOutsideDatadir, sevW))
		if fs[0].GetFile() != "elsewhere/out.yaml" {
			t.Errorf("finding file = %q", fs[0].GetFile())
		}
		if m.wasRead("elsewhere/out.yaml") {
			t.Error("a file outside every datadir was read")
		}
		if got := strings.Join(dataPaths(snap), ","); got != "in.yaml" {
			t.Fatalf("data files = %q", got)
		}
	})

	t.Run("unparseable_data_file_is_an_error_naming_the_file_and_is_absent", func(t *testing.T) {
		m := branchWith(v5("hierarchy: []\n"), map[string]string{"data/seq.yaml": "- a\n", "data/ok.yaml": "a: 1\n"})
		snap, fs := AnalyzeBranch("production", m, DefaultImportLimits())
		wantKindSev(t, fs, ks(FindingDataFileUnparseable, sevE))
		if fs[0].GetFile() != "data/seq.yaml" || fs[0].GetBranch() != "production" {
			t.Errorf("finding = %v", fs[0])
		}
		if got := strings.Join(dataPaths(snap), ","); got != "ok.yaml" {
			t.Fatalf("data files = %q", got)
		}
	})

	t.Run("placeholder_data_file_imports_as_an_empty_mapping", func(t *testing.T) {
		m := branchWith(v5("hierarchy: []\n"), map[string]string{"data/common.yaml": "---\n"})
		snap, fs := AnalyzeBranch("production", m, DefaultImportLimits())
		wantKindSev(t, fs)
		if got := strings.Join(dataPaths(snap), ","); got != "common.yaml" {
			t.Fatalf("data files = %q", got)
		}
	})

	t.Run("unusable_hierarchy_imports_no_data_files", func(t *testing.T) {
		m := branchWith(readHieraFixture(t, "v3"), map[string]string{"data/common.yaml": "a: 1\n"})
		snap, fs := AnalyzeBranch("production", m, DefaultImportLimits())
		wantKindSev(t, fs, ks(FindingHieraVersionUnsupported, sevE))
		if snap.GetHieraYaml() != "" || len(snap.GetDataFiles()) != 0 {
			t.Fatalf("a v3 hierarchy must import nothing, got %v", snap)
		}
	})

	t.Run("no_hiera_yaml_still_imports_the_default_data_dir", func(t *testing.T) {
		m := branchWith("", map[string]string{"data/common.yaml": "a: 1\n"})
		snap, fs := AnalyzeBranch("production", m, DefaultImportLimits())
		wantKindSev(t, fs)
		if got := strings.Join(dataPaths(snap), ","); got != "common.yaml" {
			t.Fatalf("data files = %q", got)
		}
	})
}
