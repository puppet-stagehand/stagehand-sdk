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

// unrepresentableData holds a value ParseDataFile cannot convert (a YAML
// timestamp) and one with integer map keys, next to an ordinary key.
const unrepresentableData = "when: 2020-01-02T03:04:05Z\nports:\n  80: http\n  443: https\nname: web\n"

func TestHiera_ExistenceHelpersDoNotConvertValues(t *testing.T) {
	// The premise: the full-file parser rejects this file outright.
	if _, err := ParseDataFile(unrepresentableData); err == nil {
		t.Fatal("test premise broken: ParseDataFile accepted a file with unrepresentable values")
	}

	for _, tc := range []struct {
		key  string
		want bool
	}{
		{"name", true},
		{"when", true},
		{"ports", true},
		{"absent", false},
		{"nam", false},
		{"lookup_options", false},
	} {
		got, err := DataKeyExists(unrepresentableData, tc.key)
		if err != nil {
			t.Fatalf("DataKeyExists(%q): %v", tc.key, err)
		}
		if got != tc.want {
			t.Errorf("DataKeyExists(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}

	if ok, err := DataKeyExists("", "name"); err != nil || ok {
		t.Errorf("DataKeyExists on empty text = %v, %v; want false, nil", ok, err)
	}
	if _, err := DataKeyExists("- not\n- a mapping\n", "name"); !errors.Is(err, ErrHieraParse) {
		t.Errorf("DataKeyExists on a non-mapping document: got %v, want ErrHieraParse", err)
	}

	// DataKeyValue converts only the asked-about key.
	v, ok, err := DataKeyValue(unrepresentableData, "name")
	if err != nil || !ok || v.GetValue().AsMap()["v"] != "web" {
		t.Errorf("DataKeyValue(name) = %v, %v, %v", v, ok, err)
	}
	if v, ok, err := DataKeyValue(unrepresentableData, "when"); err != nil || !ok || v != nil {
		t.Errorf("DataKeyValue(when) = %v, %v, %v; want nil, true, nil (present but unrepresentable)", v, ok, err)
	}
	if _, ok, _ := DataKeyValue(unrepresentableData, "absent"); ok {
		t.Error("DataKeyValue(absent) reported present")
	}

	// PutDataKey adds an unrelated key to that same file without complaint.
	if _, err := PutDataKey(unrepresentableData, "fresh", &hostv1.Json{Value: mustStructForTest(t, map[string]any{"v": "x"})}); err != nil {
		t.Errorf("PutDataKey on an unrepresentable file: %v", err)
	}
}

// TestDecodeDocEmptyDocument pins the one boundary decodeDoc has: a document
// with no content (whitespace only, a bare "---", comments only, a null root)
// is the legitimately-empty state, and a sequence root or a scalar root is
// still a parse error. Both halves sit side by side so the relaxation cannot
// widen unnoticed (RESEARCH Pitfall 4, DQ-6).
func TestDecodeDocEmptyDocument(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"empty", ""},
		{"whitespace_only", "   \n"},
		{"bare_document_marker", "---\n"},
		{"comment_only", "# just a comment\n"},
		{"document_marker_and_comments", "---\n# a placeholder\n"},
		{"null_tilde", "~\n"},
		{"null_word", "null\n"},
	} {
		t.Run("empty/"+tc.name, func(t *testing.T) {
			doc, err := decodeDoc(tc.text)
			if err != nil || doc != nil {
				t.Fatalf("decodeDoc(%q) = %v, %v; want nil, nil", tc.text, doc, err)
			}
			df, err := ParseDataFile(tc.text)
			if err != nil || df == nil || len(df.GetValues()) != 0 {
				t.Fatalf("ParseDataFile(%q) = %v, %v; want an empty data file", tc.text, df, err)
			}
		})
	}
	for _, tc := range []struct{ name, text string }{
		{"sequence_root", "- a\n- b\n"},
		{"scalar_root", "scalar\n"},
		{"quoted_null_is_a_string", "\"null\"\n"},
		{"number_root", "42\n"},
	} {
		t.Run("still_rejected/"+tc.name, func(t *testing.T) {
			if _, err := decodeDoc(tc.text); !errors.Is(err, ErrHieraParse) {
				t.Fatalf("decodeDoc(%q) err = %v, want ErrHieraParse", tc.text, err)
			}
		})
	}
}

func TestHiera_LevelExistsIgnoresUnrelatedLevels(t *testing.T) {
	// A non-mapping element makes ParseHierarchy fail for the whole file.
	text := "version: 5\nhierarchy:\n  - name: role\n    path: roles/x.yaml\n  - just-a-string\n  - name: common\n    path: common.yaml\n"
	if _, err := ParseHierarchy(text); err == nil {
		t.Fatal("test premise broken: ParseHierarchy accepted a non-mapping level")
	}

	for _, tc := range []struct {
		name string
		want bool
	}{{"role", true}, {"common", true}, {"comm", false}, {"Common", false}, {"absent", false}} {
		got, err := LevelExists(text, tc.name)
		if err != nil {
			t.Fatalf("LevelExists(%q): %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("LevelExists(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	if ok, err := LevelExists("", "role"); err != nil || ok {
		t.Errorf("LevelExists on empty text = %v, %v; want false, nil", ok, err)
	}
	if ok, err := LevelExists("version: 5\n", "role"); err != nil || ok {
		t.Errorf("LevelExists with no hierarchy = %v, %v; want false, nil", ok, err)
	}

	lvl, ok, err := LevelByName(text, "common")
	if err != nil || !ok || lvl.GetPath() != "common.yaml" {
		t.Errorf("LevelByName(common) = %v, %v, %v", lvl, ok, err)
	}
	if _, ok, _ := LevelByName(text, "absent"); ok {
		t.Error("LevelByName(absent) reported present")
	}
}

func mustStructForTest(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// contentFreeCorpus is the exact input list TestDecodeDocEmptyDocument's "empty"
// half uses, so the read-path relaxation and the write-path guard below cannot
// drift apart. TestPutDataKeyContentFreeCorpusMatchesDecodeDoc asserts the tie.
var contentFreeCorpus = []struct{ name, text string }{
	{"empty", ""},
	{"whitespace_only", "   \n"},
	{"bare_document_marker", "---\n"},
	{"comment_only", "# just a comment\n"},
	{"document_marker_and_comments", "---\n# a placeholder\n"},
	{"null_tilde", "~\n"},
	{"null_word", "null\n"},
}

// commentLines returns every line of text that starts a comment, in order.
func commentLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "#") {
			out = append(out, t)
		}
	}
	return out
}

func TestPutDataKeyContentFreeCorpusMatchesDecodeDoc(t *testing.T) {
	for _, tc := range contentFreeCorpus {
		if doc, err := decodeDoc(tc.text); err != nil || doc != nil {
			t.Errorf("%s: decodeDoc(%q) = %v, %v; the corpus must stay content-free", tc.name, tc.text, doc, err)
		}
	}
}

// TestPutDataKeyKeepsContentFreeComments pins WR-01: decodeDoc's empty-document
// relaxation is a read-path relaxation, and before the guard PutDataKey treated
// "no document" as "unauthored" and re-encoded from scratch, silently deleting a
// comment-only data file's text (Phase 6 HIERA-04, DQ-6-R). Flattening the
// doc == nil branch in PutDataKey again fails this test.
func TestPutDataKeyKeepsContentFreeComments(t *testing.T) {
	// Every shape the read path accepts as empty is writable, and every comment in
	// the input survives into the output.
	for _, tc := range contentFreeCorpus {
		t.Run("corpus/"+tc.name, func(t *testing.T) {
			out, err := PutDataKey(tc.text, "written", jsonScalar(t, "v"))
			if err != nil {
				t.Fatalf("PutDataKey(%q): %v", tc.text, err)
			}
			want, got := commentLines(tc.text), commentLines(out)
			if strings.Join(want, "|") != strings.Join(got, "|") {
				t.Errorf("comments = %q, want %q; output:\n%s", got, want, out)
			}
			df, err := ParseDataFile(out)
			if err != nil {
				t.Fatalf("ParseDataFile(output): %v\n%s", err, out)
			}
			if len(df.Values) != 1 || df.Values["written"] == nil {
				t.Errorf("Values = %v, want exactly the written key", df.Values)
			}
		})
	}

	t.Run("blank_text_has_no_preamble", func(t *testing.T) {
		for _, in := range []string{"", "   \n", "\n\n"} {
			out, err := PutDataKey(in, "k", jsonScalar(t, "v"))
			if err != nil || out != "k: v\n" {
				t.Errorf("PutDataKey(%q) = %q, %v; want a bare one-key document", in, out, err)
			}
		}
	})

	t.Run("multi_line_header_keeps_order_and_spacing", func(t *testing.T) {
		in := "# IMPORTANT: do not edit by hand\n# owner: team-a\n\n# second paragraph\n"
		out, err := PutDataKey(in, "k", jsonScalar(t, "v"))
		if err != nil {
			t.Fatalf("PutDataKey: %v", err)
		}
		if !strings.HasPrefix(out, in) {
			t.Errorf("output does not begin with the original text verbatim:\n%s", out)
		}
		df, err := ParseDataFile(out)
		if err != nil || len(df.Values) != 1 {
			t.Fatalf("ParseDataFile = %v, %v", df, err)
		}
	})

	t.Run("comment_above_marker_and_null_root", func(t *testing.T) {
		for _, in := range []string{
			"# header\n---\n",
			"# header\n~\n",
			"# header\nnull\n",
			"# header\n---\n~\n# footer\n",
		} {
			out, err := PutDataKey(in, "k", jsonScalar(t, "v"))
			if err != nil {
				t.Fatalf("PutDataKey(%q): %v", in, err)
			}
			if !strings.Contains(out, "# header") {
				t.Errorf("PutDataKey(%q) lost the header:\n%s", in, out)
			}
			if strings.Contains(out, "---") || strings.Contains(out, "~") || strings.Contains(out, "null") {
				t.Errorf("PutDataKey(%q) kept a token line:\n%s", in, out)
			}
			if _, err := ParseDataFile(out); err != nil {
				t.Errorf("ParseDataFile(%q output): %v", in, err)
			}
		}
	})

	t.Run("trailing_comment_on_token_line_is_kept", func(t *testing.T) {
		for _, tc := range []struct{ in, comment string }{
			{"~ # nothing here yet\n", "# nothing here yet"},
			{"--- # placeholder\n", "# placeholder"},
			{"null   # unset\n", "# unset"},
		} {
			out, err := PutDataKey(tc.in, "k", jsonScalar(t, "v"))
			if err != nil {
				t.Fatalf("PutDataKey(%q): %v", tc.in, err)
			}
			if !strings.HasPrefix(out, tc.comment+"\n") {
				t.Errorf("PutDataKey(%q) = %q, want it to begin with the kept comment %q on its own line", tc.in, out, tc.comment)
			}
			if _, err := ParseDataFile(out); err != nil {
				t.Errorf("ParseDataFile(%q output): %v", tc.in, err)
			}
		}
	})

	t.Run("byte_order_mark_is_tolerated_and_kept", func(t *testing.T) {
		in := "\ufeff# header\n"
		out, err := PutDataKey(in, "k", jsonScalar(t, "v"))
		if err != nil {
			t.Fatalf("PutDataKey: %v", err)
		}
		if !strings.HasPrefix(out, "\ufeff# header\n") {
			t.Errorf("BOM or header lost: %q", out)
		}
		if _, err := ParseDataFile(out); err != nil {
			t.Errorf("ParseDataFile(output): %v", err)
		}
	})

	t.Run("crlf_comment_file", func(t *testing.T) {
		out, err := PutDataKey("# header\r\n# more\r\n", "k", jsonScalar(t, "v"))
		if err != nil {
			t.Fatalf("PutDataKey: %v", err)
		}
		if got := commentLines(out); len(got) != 2 || got[0] != "# header" || got[1] != "# more" {
			t.Errorf("comments = %q", got)
		}
	})

	t.Run("a_second_write_keeps_the_preserved_comments", func(t *testing.T) {
		first, err := PutDataKey("# header\n", "a", jsonScalar(t, "1"))
		if err != nil {
			t.Fatalf("first PutDataKey: %v", err)
		}
		second, err := PutDataKey(first, "b", jsonScalar(t, "2"))
		if err != nil {
			t.Fatalf("second PutDataKey: %v", err)
		}
		if !strings.Contains(second, "# header") {
			t.Errorf("header lost on the second write:\n%s", second)
		}
	})

	// Content-free text the classifier does not understand is refused, never
	// rewritten, and the return is empty. These all decode to "no document" in
	// decodeDoc (a tagged or anchored null root) but are not a token the
	// preserving helper recognises, so dropping the line would be a guess.
	for _, tc := range []struct{ name, text string }{
		{"tagged_null_after_marker", "# c\n--- !!null\n"},
		{"tagged_null_alone", "!!null\n"},
		{"anchored_null", "# c\n&a ~\n"},
		{"tagged_null_with_comment", "--- !!null # c\n"},
	} {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			if doc, err := decodeDoc(tc.text); err != nil || doc != nil {
				t.Fatalf("test premise broken: decodeDoc(%q) = %v, %v; want content-free", tc.text, doc, err)
			}
			out, err := PutDataKey(tc.text, "k", jsonScalar(t, "v"))
			if !errors.Is(err, ErrHieraInvalid) {
				t.Fatalf("PutDataKey(%q) err = %v, want ErrHieraInvalid", tc.text, err)
			}
			if out != "" {
				t.Errorf("PutDataKey(%q) returned %q alongside an error", tc.text, out)
			}
			if !strings.Contains(err.Error(), "no content") || !strings.Contains(err.Error(), "refused") {
				t.Errorf("error does not explain the refusal: %v", err)
			}
		})
	}

	// Text with real (but non-mapping) content never reaches the helper: it is
	// still the read path's parse error, unchanged.
	for _, in := range []string{"# c\n--- foo\n", "---# c\n", "# c\nnull foo\n"} {
		if _, err := PutDataKey(in, "k", jsonScalar(t, "v")); !errors.Is(err, ErrHieraParse) {
			t.Errorf("PutDataKey(%q) err = %v, want ErrHieraParse", in, err)
		}
	}
}
