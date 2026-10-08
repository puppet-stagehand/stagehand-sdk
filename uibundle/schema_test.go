package uibundle

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/puppet-stagehand/stagehand-sdk/manifest"
)

// The JSON Schema cannot be executed here (the module has no schema engine),
// so this pins every constant it states to the Go constant it mirrors.
func TestSchemaMatchesGoConstants(t *testing.T) {
	raw, err := os.ReadFile("../schema/json/ui.manifest.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	props := s["properties"].(map[string]any)
	strs := func(v any) []string {
		var out []string
		for _, x := range v.([]any) {
			out = append(out, x.(string))
		}
		sort.Strings(out)
		return out
	}
	sorted := func(in []string) []string { c := append([]string(nil), in...); sort.Strings(c); return c }

	if got := props["format"].(map[string]any)["const"].(float64); int(got) != FormatVersion {
		t.Errorf("format const %v != %d", got, FormatVersion)
	}
	slots := props["slots"].(map[string]any)
	if got := strs(slots["propertyNames"].(map[string]any)["enum"]); !reflect.DeepEqual(got, sorted(manifest.Slots)) {
		t.Errorf("slot enum %v != manifest.Slots %v", got, manifest.Slots)
	}
	label := slots["additionalProperties"].(map[string]any)["properties"].(map[string]any)["label"].(map[string]any)
	if int(label["maxLength"].(float64)) != MaxLabelChars || int(label["minLength"].(float64)) != 1 {
		t.Errorf("label bounds %v", label)
	}
	files := props["files"].(map[string]any)
	if int(files["maxProperties"].(float64)) != MaxFiles {
		t.Errorf("maxProperties %v != %d", files["maxProperties"], MaxFiles)
	}
	fp := files["additionalProperties"].(map[string]any)["properties"].(map[string]any)
	if got := strs(fp["content_type"].(map[string]any)["enum"]); !reflect.DeepEqual(got, sorted(ContentTypes)) {
		t.Errorf("content types %v != %v", got, ContentTypes)
	}
	if int64(fp["size"].(map[string]any)["maximum"].(float64)) != MaxFileBytes {
		t.Errorf("size maximum %v != %d", fp["size"].(map[string]any)["maximum"], MaxFileBytes)
	}
	path := s["$defs"].(map[string]any)["path"].(map[string]any)
	if int(path["maxLength"].(float64)) != MaxPathChars {
		t.Errorf("path maxLength %v != %d", path["maxLength"], MaxPathChars)
	}
}
