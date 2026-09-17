package code

import (
	"errors"
	"strings"
	"testing"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
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
