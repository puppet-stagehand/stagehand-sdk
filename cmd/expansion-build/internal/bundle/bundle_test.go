package bundle

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/puppet-stagehand/stagehand-sdk/uibundle"
)

func td(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "testdata"}, parts...)...)
}

func TestEntryRewritesShims(t *testing.T) {
	files, fs := Entry(td("hello-src"), "index.tsx")
	if len(fs) != 0 {
		t.Fatal(fs)
	}
	js := string(files["index.js"])
	for _, want := range []string{`from "/runtime/v1/console-ui.js"`, `from "/runtime/v1/jsx-runtime.js"`, "contract_version: 1"} {
		if !strings.Contains(js, want) {
			t.Errorf("index.js lacks %s:\n%s", want, js)
		}
	}
	if regexp.MustCompile(`from "(react|react-dom|@stagehand/[^"]+)"`).MatchString(js) {
		t.Errorf("a bare specifier survived:\n%s", js)
	}
	if strings.Contains(js, "/Users/") || strings.Contains(js, "testdata") {
		t.Errorf("an absolute or machine-specific path leaked:\n%s", js)
	}
}

func TestSandboxIsSelfContained(t *testing.T) {
	files, fs := Sandbox(td("sandbox-src"), "sandbox.tsx")
	if len(fs) != 0 {
		t.Fatal(fs)
	}
	if len(files) != 1 {
		t.Fatalf("want one file, got %v", keys(files))
	}
	js := string(files["sandbox.js"])
	if regexp.MustCompile(`(?m)^\s*import\s`).MatchString(js) || strings.Contains(js, "import(") {
		t.Errorf("sandbox bundle has an import:\n%s", js)
	}
	if !strings.Contains(js, "createRoot") {
		t.Errorf("react-dom stub was not bundled in:\n%s", js)
	}
}

func TestMissingPackageIsAFinding(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.tsx"), []byte(`import x from "left-pad"; export default x;`), 0o644)
	files, fs := Entry(dir, "index.tsx")
	if files != nil || len(fs) != 1 || fs[0].Code != "ui_build_error" || fs[0].Fix == "" || !strings.Contains(fs[0].Message, "left-pad") {
		t.Fatalf("files=%v fs=%+v", files, fs)
	}
}

func TestCSSOutputIsRefused(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.css"), []byte(".a { margin: 0 }\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "index.tsx"), []byte(`import "./a.css"; export default 1;`), 0o644)
	_, fs := Entry(dir, "index.tsx")
	if len(fs) != 1 || fs[0].Code != "ui_build_output_unsupported" {
		t.Fatalf("%+v", fs)
	}
}

func TestDeterministicAcrossWorkingDirs(t *testing.T) {
	a, afs := Entry(td("hello-src"), "index.tsx")
	copyDir := t.TempDir()
	b, _ := os.ReadFile(td("hello-src", "index.tsx"))
	_ = os.WriteFile(filepath.Join(copyDir, "index.tsx"), b, 0o644)
	c, cfs := Entry(copyDir, "index.tsx")
	if len(afs)+len(cfs) != 0 {
		t.Fatalf("findings: %+v %+v", afs, cfs)
	}
	if string(a["index.js"]) != string(c["index.js"]) {
		t.Fatalf("output depends on where the source lives:\n%s\n---\n%s", a["index.js"], c["index.js"])
	}
}

func keys(f Files) []string {
	var k []string
	for n := range f {
		k = append(k, n)
	}
	return k
}

func TestChunkNamesNeverCarryAuthorFileNames(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "my chunk \u00fcber.tsx"), []byte(`export const x = 1;`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "index.tsx"), []byte(`export default { load: () => import("./my chunk \u00fcber") };`), 0o644)
	files, fs := Entry(dir, "index.tsx")
	if len(fs) != 0 {
		t.Fatal(fs)
	}
	chunks := 0
	for name := range files {
		if !uibundle.ValidPath(name) {
			t.Errorf("output path %q is not a legal bundle path", name)
		}
		if strings.HasPrefix(name, "chunks/") {
			chunks++
		}
	}
	if chunks == 0 {
		t.Fatalf("expected a chunk, got %v", keys(files))
	}
}

func TestEntryRefusesNonShimSubpaths(t *testing.T) {
	// Under testdata so the stub node_modules resolve: the import would succeed
	// (and inline a second copy of react-dom) if the entry build did not refuse it.
	dir, err := os.MkdirTemp(td(), "tmp-entry-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	_ = os.WriteFile(filepath.Join(dir, "index.tsx"), []byte(`import { createRoot } from "react-dom/client"; export default createRoot;`), 0o644)
	files, fs := Entry(dir, "index.tsx")
	if files != nil || len(fs) != 1 || fs[0].Code != "ui_build_error" ||
		!strings.Contains(fs[0].Message, "react-dom/client") || !strings.Contains(fs[0].Message, "in-process") {
		t.Fatalf("files=%v fs=%+v", files, fs)
	}
	for _, spec := range []string{"react/other", "@stagehand/console-ui/internal"} {
		d := t.TempDir()
		_ = os.WriteFile(filepath.Join(d, "index.tsx"), []byte(`import x from "`+spec+`"; export default x;`), 0o644)
		_, fs := Entry(d, "index.tsx")
		if len(fs) != 1 || !strings.Contains(fs[0].Message, "in-process") {
			t.Errorf("%s: %+v", spec, fs)
		}
	}
}

func TestSandboxStillResolvesReactSubpaths(t *testing.T) {
	if _, fs := Sandbox(td("sandbox-src"), "sandbox.tsx"); len(fs) != 0 {
		t.Fatal(fs)
	}
}
