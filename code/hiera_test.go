package code

import (
	"errors"
	"strings"
	"testing"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestHiera_ParseHierarchy(t *testing.T) {
	text := `# Example Hiera hierarchy for the test environment
version: 5
defaults:
  datadir: data
  data_hash: yaml_data

hierarchy:
  - name: "Per-node data"
    path: "nodes/%{trusted.certname}.yaml"

  - name: "Per-OS defaults" # os-specific overrides
    path: "os/%{facts.os.family}.yaml"

  - name: "Common data"
    path: "common.yaml"
`
	h, err := ParseHierarchy(text)
	if err != nil {
		t.Fatalf("ParseHierarchy: %v", err)
	}
	if h.Version != 5 {
		t.Errorf("Version = %d, want 5", h.Version)
	}
	if h.DefaultDatadir != "data" {
		t.Errorf("DefaultDatadir = %q, want data", h.DefaultDatadir)
	}
	if h.DefaultDataHash != "yaml_data" {
		t.Errorf("DefaultDataHash = %q, want yaml_data", h.DefaultDataHash)
	}
	if len(h.Levels) != 3 {
		t.Fatalf("len(Levels) = %d, want 3", len(h.Levels))
	}
	wantNames := []string{"Per-node data", "Per-OS defaults", "Common data"}
	wantPaths := []string{"nodes/%{trusted.certname}.yaml", "os/%{facts.os.family}.yaml", "common.yaml"}
	for i, lvl := range h.Levels {
		if lvl.Name != wantNames[i] {
			t.Errorf("Levels[%d].Name = %q, want %q", i, lvl.Name, wantNames[i])
		}
		if lvl.Path != wantPaths[i] {
			t.Errorf("Levels[%d].Path = %q, want %q", i, lvl.Path, wantPaths[i])
		}
	}
}

func TestHiera_ParseHierarchyAlternateLevelForms(t *testing.T) {
	text := `version: 5
hierarchy:
  - name: "Paths example"
    paths:
      - "a.yaml"
      - "b.yaml"
  - name: "Glob example"
    glob: "network/**/*.yaml"
  - name: "Mapped example"
    mapped_paths: [services, svc, "service/%{svc}/common.yaml"]
`
	h, err := ParseHierarchy(text)
	if err != nil {
		t.Fatalf("ParseHierarchy: %v", err)
	}
	if len(h.Levels) != 3 {
		t.Fatalf("len(Levels) = %d, want 3", len(h.Levels))
	}

	pathsLevel := h.Levels[0]
	if len(pathsLevel.Paths) != 2 || pathsLevel.Paths[0] != "a.yaml" || pathsLevel.Paths[1] != "b.yaml" {
		t.Errorf("Paths level.Paths = %v", pathsLevel.Paths)
	}
	if pathsLevel.Path != "" || pathsLevel.Glob != "" || len(pathsLevel.MappedPaths) != 0 {
		t.Errorf("Paths level leaked into other fields: %+v", pathsLevel)
	}

	globLevel := h.Levels[1]
	if globLevel.Glob != "network/**/*.yaml" {
		t.Errorf("Glob = %q", globLevel.Glob)
	}
	if globLevel.Path != "" || len(globLevel.Paths) != 0 || len(globLevel.MappedPaths) != 0 {
		t.Errorf("Glob level leaked into other fields: %+v", globLevel)
	}

	mappedLevel := h.Levels[2]
	want := []string{"services", "svc", "service/%{svc}/common.yaml"}
	if len(mappedLevel.MappedPaths) != 3 {
		t.Fatalf("MappedPaths len = %d, want 3", len(mappedLevel.MappedPaths))
	}
	for i, w := range want {
		if mappedLevel.MappedPaths[i] != w {
			t.Errorf("MappedPaths[%d] = %q, want %q", i, mappedLevel.MappedPaths[i], w)
		}
	}
	if mappedLevel.Path != "" || len(mappedLevel.Paths) != 0 || mappedLevel.Glob != "" {
		t.Errorf("Mapped level leaked into other fields: %+v", mappedLevel)
	}
}

func TestHiera_ParseEmptyHierarchy(t *testing.T) {
	h, err := ParseHierarchy("")
	if err != nil {
		t.Fatalf(`ParseHierarchy(""): %v`, err)
	}
	if len(h.Levels) != 0 {
		t.Errorf("len(Levels) = %d, want 0", len(h.Levels))
	}

	skeleton := EmptyHierarchy()
	h2, err := ParseHierarchy(skeleton)
	if err != nil {
		t.Fatalf("ParseHierarchy(EmptyHierarchy()): %v", err)
	}
	if h2.Version != 5 {
		t.Errorf("EmptyHierarchy() Version = %d, want 5", h2.Version)
	}
	if len(h2.Levels) != 0 {
		t.Errorf("EmptyHierarchy() len(Levels) = %d, want 0", len(h2.Levels))
	}
}

func TestHiera_PutLevelPreservesComments(t *testing.T) {
	text := `# top of file comment
version: 5
hierarchy:
  - name: "Level Zero" # trailing comment on level zero
    path: "zero.yaml"
  - name: "Level One"
    path: "one.yaml"
`
	newLevel := &hostv1.HieraLevel{Name: "Inserted", Path: "inserted.yaml"}
	out, err := PutLevel(text, newLevel, 1, true)
	if err != nil {
		t.Fatalf("PutLevel: %v", err)
	}
	for _, want := range []string{
		"# top of file comment",
		"# trailing comment on level zero",
		"Level Zero",
		"Level One",
		"Inserted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	h, err := ParseHierarchy(out)
	if err != nil {
		t.Fatalf("ParseHierarchy(output): %v", err)
	}
	if len(h.Levels) != 3 {
		t.Fatalf("len(Levels) = %d, want 3", len(h.Levels))
	}
	wantOrder := []string{"Level Zero", "Inserted", "Level One"}
	for i, w := range wantOrder {
		if h.Levels[i].Name != w {
			t.Errorf("Levels[%d].Name = %q, want %q", i, h.Levels[i].Name, w)
		}
	}
}

func TestHiera_PutLevelOrdering(t *testing.T) {
	text := `version: 5
hierarchy:
  - name: "L0"
    path: "l0.yaml"
  - name: "L1"
    path: "l1.yaml"
`
	out, err := PutLevel(text, &hostv1.HieraLevel{Name: "L0.5", Path: "l05.yaml"}, 1, true)
	if err != nil {
		t.Fatalf("PutLevel insert: %v", err)
	}
	h, err := ParseHierarchy(out)
	if err != nil {
		t.Fatalf("ParseHierarchy: %v", err)
	}
	wantNames := []string{"L0", "L0.5", "L1"}
	for i, w := range wantNames {
		if h.Levels[i].Name != w {
			t.Errorf("Levels[%d].Name = %q, want %q", i, h.Levels[i].Name, w)
		}
	}

	out2, err := PutLevel(out, &hostv1.HieraLevel{Name: "L1", Path: "l1-replaced.yaml"}, 0, false)
	if err != nil {
		t.Fatalf("PutLevel replace: %v", err)
	}
	h2, err := ParseHierarchy(out2)
	if err != nil {
		t.Fatalf("ParseHierarchy(replace): %v", err)
	}
	if len(h2.Levels) != 3 {
		t.Fatalf("len(Levels) after replace = %d, want 3", len(h2.Levels))
	}
	if h2.Levels[2].Name != "L1" || h2.Levels[2].Path != "l1-replaced.yaml" {
		t.Errorf("L1 not replaced in place: %+v", h2.Levels[2])
	}

	if _, err := PutLevel(out2, &hostv1.HieraLevel{Name: "Nope"}, 0, false); !errors.Is(err, ErrHieraInvalid) {
		t.Errorf("PutLevel replace unknown name: err = %v, want ErrHieraInvalid", err)
	}

	empty, err := PutLevel("", &hostv1.HieraLevel{Name: "Only", Path: "only.yaml"}, 0, true)
	if err != nil {
		t.Fatalf("PutLevel against empty text: %v", err)
	}
	h3, err := ParseHierarchy(empty)
	if err != nil {
		t.Fatalf("ParseHierarchy(empty put result): %v", err)
	}
	if len(h3.Levels) != 1 || h3.Levels[0].Name != "Only" {
		t.Errorf("PutLevel against empty text produced %+v", h3.Levels)
	}
	if h3.Version != 5 {
		t.Errorf("PutLevel against empty text: Version = %d, want 5 (from EmptyHierarchy skeleton)", h3.Version)
	}
}

func TestHiera_RemoveAndReorderLevels(t *testing.T) {
	text := `version: 5
hierarchy:
  - name: "A"
    path: "a.yaml"
  - name: "B"
    path: "b.yaml"
  - name: "C"
    path: "c.yaml"
`
	out, err := RemoveLevel(text, "B")
	if err != nil {
		t.Fatalf("RemoveLevel: %v", err)
	}
	h, err := ParseHierarchy(out)
	if err != nil {
		t.Fatalf("ParseHierarchy: %v", err)
	}
	if len(h.Levels) != 2 || h.Levels[0].Name != "A" || h.Levels[1].Name != "C" {
		t.Errorf("after RemoveLevel(B): %+v", h.Levels)
	}

	if _, err := RemoveLevel(text, "Nope"); !errors.Is(err, ErrHieraInvalid) {
		t.Errorf("RemoveLevel unknown: err = %v, want ErrHieraInvalid", err)
	}

	reordered, err := ReorderLevels(text, []string{"C", "A", "B"})
	if err != nil {
		t.Fatalf("ReorderLevels: %v", err)
	}
	h2, err := ParseHierarchy(reordered)
	if err != nil {
		t.Fatalf("ParseHierarchy(reordered): %v", err)
	}
	wantOrder := []string{"C", "A", "B"}
	for i, w := range wantOrder {
		if h2.Levels[i].Name != w {
			t.Errorf("Levels[%d] = %q, want %q", i, h2.Levels[i].Name, w)
		}
	}

	for _, bad := range [][]string{
		{"A", "B"},           // missing C
		{"A", "B", "C", "D"}, // extra D, D unknown
		{"A", "A", "C"},      // duplicate A, missing B
	} {
		out, err := ReorderLevels(text, bad)
		if !errors.Is(err, ErrHieraInvalid) {
			t.Errorf("ReorderLevels(%v): err = %v, want ErrHieraInvalid", bad, err)
		}
		if out != "" {
			t.Errorf("ReorderLevels(%v) returned non-empty text on error", bad)
		}
	}

	// A rejected reorder must not have mutated any shared state that a
	// subsequent parse of the original text would observe.
	h3, err := ParseHierarchy(text)
	if err != nil {
		t.Fatalf("ParseHierarchy(text) after rejected reorders: %v", err)
	}
	wantStillOriginal := []string{"A", "B", "C"}
	for i, w := range wantStillOriginal {
		if h3.Levels[i].Name != w {
			t.Errorf("original text mutated: Levels[%d] = %q, want %q", i, h3.Levels[i].Name, w)
		}
	}
}

func TestHiera_EnvironmentLint(t *testing.T) {
	level := &hostv1.HieraLevel{
		Path:        "nodes/%{environment}/data.yaml",
		Paths:       []string{"a.yaml", "b/%{::environment}/c.yaml"},
		Glob:        "network/%{environment}/**/*.yaml",
		MappedPaths: []string{"services", "svc", "service/%{svc}/%{environment}.yaml"},
	}
	warnings := LintLevelPaths(level)
	if len(warnings) == 0 {
		t.Fatal("LintLevelPaths returned no warnings for a level carrying %{environment} in every field")
	}
	fields := map[string]bool{}
	for _, w := range warnings {
		fields[w.Field] = true
		if w.Code == "" {
			t.Errorf("warning missing code: %+v", w)
		}
		if w.Message == "" {
			t.Errorf("warning missing message: %+v", w)
		}
	}
	for _, want := range []string{"path", "paths[1]", "glob", "mapped_paths[2]"} {
		if !fields[want] {
			t.Errorf("missing warning for field %q, got %v", want, fields)
		}
	}
	if fields["paths[0]"] {
		t.Errorf("paths[0] (\"a.yaml\") should not be flagged: %v", warnings)
	}

	clean := &hostv1.HieraLevel{
		Path:  "os/%{facts.os.family}.yaml",
		Paths: []string{"%{environments}/x.yaml", "%{trusted.certname}.yaml"},
	}
	if got := LintLevelPaths(clean); len(got) != 0 {
		t.Errorf("LintLevelPaths(clean) = %v, want empty", got)
	}
}

// jsonScalar builds a *hostv1.Json using the repo's single-field "v"
// wrapper convention, mirroring host/local/inventory.go's jsonScalar
// without importing host/local (this package has no gRPC dependency).
func jsonScalar(t *testing.T, v any) *hostv1.Json {
	t.Helper()
	s, err := structpb.NewStruct(map[string]any{"v": v})
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return &hostv1.Json{Value: s}
}

func jsonObject(t *testing.T, m map[string]any) *hostv1.Json {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return &hostv1.Json{Value: s}
}

func TestHiera_ParseDataFileAndLookupOptions(t *testing.T) {
	df, err := ParseDataFile("")
	if err != nil {
		t.Fatalf(`ParseDataFile(""): %v`, err)
	}
	if df.Values == nil || len(df.Values) != 0 {
		t.Errorf("ParseDataFile(\"\").Values = %v, want empty non-nil", df.Values)
	}
	if df.LookupOptions == nil || len(df.LookupOptions) != 0 {
		t.Errorf("ParseDataFile(\"\").LookupOptions = %v, want empty non-nil", df.LookupOptions)
	}

	text := `classes:
  - apache
  - mysql
port: 8080
site_name: "example"
`
	df2, err := ParseDataFile(text)
	if err != nil {
		t.Fatalf("ParseDataFile: %v", err)
	}
	if len(df2.Values) != 3 {
		t.Fatalf("len(Values) = %d, want 3", len(df2.Values))
	}
	if len(df2.LookupOptions) != 0 {
		t.Errorf("LookupOptions = %v, want empty (no lookup_options block)", df2.LookupOptions)
	}
	portJSON, ok := df2.Values["port"]
	if !ok {
		t.Fatal("Values[\"port\"] missing")
	}
	if got := portJSON.Value.AsMap()["v"]; got != float64(8080) {
		t.Errorf("Values[\"port\"] = %v, want 8080", got)
	}

	textWithOptions := `classes:
  - apache
db_merge:
  a: 1
lookup_options:
  classes:
    merge: unique
  db_merge:
    merge:
      strategy: deep
`
	df3, err := ParseDataFile(textWithOptions)
	if err != nil {
		t.Fatalf("ParseDataFile with lookup_options: %v", err)
	}
	if _, ok := df3.Values["lookup_options"]; ok {
		t.Error("Values must not include the lookup_options key itself")
	}
	if len(df3.Values) != 2 {
		t.Errorf("len(Values) = %d, want 2 (classes, db_merge)", len(df3.Values))
	}
	if df3.LookupOptions["classes"] != "unique" {
		t.Errorf("LookupOptions[\"classes\"] = %q, want unique (scalar merge form)", df3.LookupOptions["classes"])
	}
	if df3.LookupOptions["db_merge"] != "deep" {
		t.Errorf("LookupOptions[\"db_merge\"] = %q, want deep (merge.strategy mapping form)", df3.LookupOptions["db_merge"])
	}
}

func TestHiera_PutDataKeyPreservesStructure(t *testing.T) {
	text := `# head comment on the document
classes:
  - apache
  - mysql
nested:
  cpu:
    cores: 8
port: 8080 # trailing comment on port
site_name: "example"
`
	out, err := PutDataKey(text, "new_key", jsonScalar(t, "new_value"))
	if err != nil {
		t.Fatalf("PutDataKey: %v", err)
	}

	for _, want := range []string{
		"# head comment on the document",
		"classes:",
		"- apache",
		"- mysql",
		"nested:",
		"cpu:",
		"cores: 8",
		"port: 8080 # trailing comment on port",
		`site_name: "example"`,
		"new_key: new_value",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// Original relative order must be preserved, with the new key
	// appended at the end rather than sorted into place.
	idx := func(s string) int { return strings.Index(out, s) }
	keysInOrder := []string{"classes:", "nested:", "port:", "site_name:", "new_key:"}
	for i := 1; i < len(keysInOrder); i++ {
		if idx(keysInOrder[i-1]) >= idx(keysInOrder[i]) {
			t.Errorf("key order violated: %q did not come before %q in:\n%s", keysInOrder[i-1], keysInOrder[i], out)
		}
	}

	if strings.Count(out, "\n    ") > 0 && strings.Count(out, "\n  ") == 0 {
		t.Error("output appears to use four-space indentation, not two")
	}

	df, err := ParseDataFile(out)
	if err != nil {
		t.Fatalf("ParseDataFile(output): %v", err)
	}
	if len(df.Values) != 5 {
		t.Fatalf("len(Values) = %d, want 5", len(df.Values))
	}
	if v := df.Values["new_key"].Value.AsMap()["v"]; v != "new_value" {
		t.Errorf("new_key = %v, want new_value", v)
	}

	// Replacing an existing key changes only that key's value.
	out2, err := PutDataKey(out, "port", jsonScalar(t, float64(9090)))
	if err != nil {
		t.Fatalf("PutDataKey replace: %v", err)
	}
	df2, err := ParseDataFile(out2)
	if err != nil {
		t.Fatalf("ParseDataFile(replaced): %v", err)
	}
	if v := df2.Values["port"].Value.AsMap()["v"]; v != float64(9090) {
		t.Errorf("port after replace = %v, want 9090", v)
	}
	if v := df2.Values["site_name"].Value.AsMap()["v"]; v != "example" {
		t.Errorf("site_name changed by an unrelated put: %v", v)
	}
	if !strings.Contains(out2, "# trailing comment on port") {
		t.Error("trailing comment on port lost after replacing an unrelated key's sibling")
	}

	// Put against empty text creates a one-key document.
	fromEmpty, err := PutDataKey("", "only_key", jsonScalar(t, "only_value"))
	if err != nil {
		t.Fatalf("PutDataKey against empty text: %v", err)
	}
	dfEmpty, err := ParseDataFile(fromEmpty)
	if err != nil {
		t.Fatalf("ParseDataFile(fromEmpty): %v", err)
	}
	if len(dfEmpty.Values) != 1 {
		t.Fatalf("len(Values) = %d, want 1", len(dfEmpty.Values))
	}
}

func TestHiera_PutDataKeyRefusesLookupOptions(t *testing.T) {
	text := "existing: value\n"
	out, err := PutDataKey(text, "lookup_options", jsonObject(t, map[string]any{"classes": map[string]any{"merge": "unique"}}))
	if !errors.Is(err, ErrHieraInvalid) {
		t.Errorf("PutDataKey(lookup_options): err = %v, want ErrHieraInvalid", err)
	}
	if out != "" {
		t.Errorf("PutDataKey(lookup_options) emitted non-empty output %q", out)
	}
}

func TestHiera_DataFileEncoding(t *testing.T) {
	text := `unicode_key: "café — 日本語"
colon_value: "10:30 AM meeting"
percent_value: "%{not_an_interpolation_if_quoted}"
block_scalar: |
  line one
  line two
  line three
untouched_other: original
`
	out, err := PutDataKey(text, "untouched_other", jsonScalar(t, "changed"))
	if err != nil {
		t.Fatalf("PutDataKey: %v", err)
	}

	df, err := ParseDataFile(out)
	if err != nil {
		t.Fatalf("ParseDataFile(output): %v", err)
	}
	if v := df.Values["unicode_key"].Value.AsMap()["v"]; v != "café — 日本語" {
		t.Errorf("unicode_key = %v", v)
	}
	if v := df.Values["colon_value"].Value.AsMap()["v"]; v != "10:30 AM meeting" {
		t.Errorf("colon_value = %v", v)
	}
	if v := df.Values["percent_value"].Value.AsMap()["v"]; v != "%{not_an_interpolation_if_quoted}" {
		t.Errorf("percent_value = %v", v)
	}
	if v := df.Values["block_scalar"].Value.AsMap()["v"]; v != "line one\nline two\nline three\n" {
		t.Errorf("block_scalar = %q", v)
	}
	if v := df.Values["untouched_other"].Value.AsMap()["v"]; v != "changed" {
		t.Errorf("untouched_other = %v, want changed", v)
	}

	// Byte-intact in the raw output text too, not just after re-parsing.
	for _, want := range []string{
		"café — 日本語",
		"10:30 AM meeting",
		"%{not_an_interpolation_if_quoted}",
		"line one",
		"line two",
		"line three",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestHiera_ValidateDataPath(t *testing.T) {
	for _, bad := range []string{
		"",
		"   ",
		"/etc/passwd",
		"../secrets.yaml",
		"nodes/../../etc/x.yaml",
		"nodes\\web01.yaml",
		"nodes/web01\n.yaml",
		"nodes/web01\x00.yaml",
	} {
		if err := ValidateDataPath(bad); !errors.Is(err, ErrHieraInvalid) {
			t.Errorf("ValidateDataPath(%q): err = %v, want ErrHieraInvalid", bad, err)
		}
	}
	for _, good := range []string{
		"common.yaml",
		"nodes/web01.example.test.yaml",
	} {
		if err := ValidateDataPath(good); err != nil {
			t.Errorf("ValidateDataPath(%q): unexpected error %v", good, err)
		}
	}
}
