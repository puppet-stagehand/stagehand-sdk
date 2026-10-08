package build

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puppet-stagehand/stagehand-sdk/manifest"
	"github.com/puppet-stagehand/stagehand-sdk/uibundle"
)

// work copies a fixture into a temp dir laid out like a pack and returns Options for it.
func work(t *testing.T, fixture string) (Options, string) {
	t.Helper()
	root := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", fixture), filepath.Join(root, "ui", "src"))
	copyTree(t, filepath.Join("..", "..", "testdata", "node_modules"), filepath.Join(root, "ui", "node_modules"))
	b, err := os.ReadFile(filepath.Join(root, "ui", "src", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(root, "ui", "src", "manifest.json"))
	return Options{Src: filepath.Join(root, "ui", "src"), Out: filepath.Join(root, "dist", "ui"), Manifest: filepath.Join(root, "manifest.json")}, root
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func hasCode(fs []manifest.Finding, c string) bool {
	for _, f := range fs {
		if f.Code == c {
			return true
		}
	}
	return false
}

func TestRunBuildsAVerifiedBundle(t *testing.T) {
	o, _ := work(t, "hello-src")
	res, fs, err := Run(o)
	if err != nil || len(fs) != 0 {
		t.Fatalf("err=%v fs=%v", err, fs)
	}
	raw, _ := os.ReadFile(filepath.Join(o.Out, uibundle.ManifestPath))
	if res.Digest != uibundle.DigestOf(raw) {
		t.Fatalf("digest %s vs %s", res.Digest, uibundle.DigestOf(raw))
	}
	mraw, _ := os.ReadFile(o.Manifest)
	pm, _ := manifest.Parse(mraw)
	if pm.UIDigest != res.Digest {
		t.Fatalf("manifest.json ui_digest %s, want %s", pm.UIDigest, res.Digest)
	}
	if fs := uibundle.Verify(o.Out, pm); len(fs) != 0 {
		t.Fatalf("uibundle.Verify: %v", fs)
	}
	if !bytes.HasSuffix(raw, []byte("}\n")) || !bytes.Contains(raw, []byte("\n  \"entry\": \"index.js\"")) {
		t.Fatalf("ui.manifest.json is not 2-space indented with a trailing newline:\n%s", raw)
	}
}

func TestOnlyUIDigestChangesInManifest(t *testing.T) {
	o, _ := work(t, "hello-src")
	before, _ := os.ReadFile(o.Manifest)
	res, fs, err := Run(o)
	if err != nil || len(fs) != 0 {
		t.Fatalf("%v %v", err, fs)
	}
	after, _ := os.ReadFile(o.Manifest)
	zero := "sha256:" + strings.Repeat("0", 64)
	if string(after) == string(before) {
		t.Fatal("manifest.json was not rewritten")
	}
	if got := strings.Replace(string(after), res.Digest, zero, 1); got != string(before) {
		t.Fatalf("more than ui_digest changed:\n--- before\n%s\n--- after (digest reset)\n%s", before, got)
	}
}

func TestSandboxEntryIsListed(t *testing.T) {
	o, _ := work(t, "sandbox-src")
	if _, fs, err := Run(o); err != nil || len(fs) != 0 {
		t.Fatalf("%v %v", err, fs)
	}
	raw, _ := os.ReadFile(filepath.Join(o.Out, uibundle.ManifestPath))
	um, fs := uibundle.Parse(raw)
	if len(fs) != 0 || um.SandboxEntry != "sandbox.js" || um.Files["sandbox.js"].ContentType != "text/javascript" {
		t.Fatalf("%+v %v", um, fs)
	}
}

func TestDeterministic(t *testing.T) {
	a, _ := work(t, "sandbox-src")
	b, _ := work(t, "sandbox-src")
	ra, _, _ := Run(a)
	rb, _, _ := Run(b)
	if ra == nil || rb == nil || ra.Digest != rb.Digest {
		t.Fatalf("digests differ or build failed: %+v %+v", ra, rb)
	}
}

func TestEntryMissing(t *testing.T) {
	o, _ := work(t, "hello-src")
	_ = os.Remove(filepath.Join(o.Src, "index.tsx"))
	_, fs, err := Run(o)
	if err != nil || len(fs) != 1 || fs[0].Code != "ui_src_entry_missing" || !strings.Contains(fs[0].Message, "index.tsx") {
		t.Fatalf("%v %+v", err, fs)
	}
}

func TestSlotsJSONProblems(t *testing.T) {
	cases := []struct{ name, body, code string }{
		{"invalid json", `{`, "ui_slots_json_invalid"},
		{"extra slot", `{"nodeDetailTab":"Hello","page":"X"}`, "ui_slot_not_declared"},
		{"missing slot", `{}`, "ui_slot_missing"},
		{"bad label", `{"nodeDetailTab":""}`, "ui_slot_label_invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, _ := work(t, "hello-src")
			_ = os.WriteFile(filepath.Join(o.Src, "slots.json"), []byte(c.body), 0o644)
			_, fs, err := Run(o)
			if err != nil || !hasCode(fs, c.code) {
				t.Fatalf("want %s: %v %+v", c.code, err, fs)
			}
			for _, f := range fs {
				if f.Code == c.code && (!strings.HasPrefix(f.Path, "slots.json") || strings.Contains(f.Message, "builder bug")) {
					t.Errorf("finding must come from slots.json, not the final self-check: %+v", f)
				}
			}
			if _, statErr := os.Stat(o.Out); statErr == nil {
				t.Fatal("--out was created by a failing build")
			}
		})
	}
}

func TestLintFailureLeavesNothing(t *testing.T) {
	o, _ := work(t, "hello-src")
	_ = os.WriteFile(filepath.Join(o.Src, "index.tsx"), []byte(`const c = "#fff"; export default { contract_version: 1, slots: {} };`), 0o644)
	before, _ := os.ReadFile(o.Manifest)
	_, fs, err := Run(o)
	if err != nil || !hasCode(fs, "ui_lint_hex_colour") {
		t.Fatalf("%v %+v", err, fs)
	}
	after, _ := os.ReadFile(o.Manifest)
	if _, statErr := os.Stat(o.Out); statErr == nil || string(before) != string(after) {
		t.Fatal("a failed build touched --out or manifest.json")
	}
}

func TestDigestKeyMissingForAHeadlessManifest(t *testing.T) {
	o, _ := work(t, "hello-src")
	raw, _ := os.ReadFile(o.Manifest)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "ui_digest")
	m["slots"] = []string{}
	m["nav"] = nil
	out, _ := json.Marshal(m)
	_ = os.WriteFile(o.Manifest, out, 0o644)
	_, fs, err := Run(o)
	if err != nil || !hasCode(fs, "ui_digest_key_missing") {
		t.Fatalf("%v %+v", err, fs)
	}
}

func TestOutSafety(t *testing.T) {
	o, root := work(t, "hello-src")
	stray := filepath.Join(root, "precious")
	_ = os.MkdirAll(stray, 0o755)
	_ = os.WriteFile(filepath.Join(stray, "keep.txt"), []byte("x"), 0o644)
	for name, out := range map[string]string{"not a previous build": stray, "contains src": filepath.Join(root, "ui"), "contains src and manifest": root} {
		t.Run(name, func(t *testing.T) {
			oo := o
			oo.Out = out
			_, fs, err := Run(oo)
			if err != nil || !hasCode(fs, "ui_out_unsafe") {
				t.Fatalf("%v %+v", err, fs)
			}
			if _, e := os.Stat(filepath.Join(stray, "keep.txt")); e != nil {
				t.Fatal("a refused build deleted files")
			}
			if _, e := os.Stat(o.Src); e != nil {
				t.Fatal("a refused build deleted the sources")
			}
		})
	}
}

func TestRebuildReplacesPreviousOutput(t *testing.T) {
	o, _ := work(t, "hello-src")
	if _, fs, err := Run(o); err != nil || len(fs) != 0 {
		t.Fatalf("%v %v", err, fs)
	}
	stale := filepath.Join(o.Out, "stale.js")
	_ = os.WriteFile(stale, []byte("x"), 0o644)
	if _, fs, err := Run(o); err != nil || len(fs) != 0 {
		t.Fatalf("%v %v", err, fs)
	}
	if _, e := os.Stat(stale); e == nil {
		t.Fatal("stale file survived the rebuild")
	}
}

func TestPackCheckAcceptsOutput(t *testing.T) {
	o, _ := work(t, "hello-src")
	if _, fs, err := Run(o); err != nil || len(fs) != 0 {
		t.Fatalf("%v %v", err, fs)
	}
	cmd := exec.Command("go", "run", "github.com/puppet-stagehand/stagehand-sdk/cmd/pack-check", "--ui", o.Out, o.Manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pack-check --ui failed: %v\n%s", err, out)
	}
}

func TestOutWithTrailingSeparator(t *testing.T) {
	o, _ := work(t, "hello-src")
	if _, fs, err := Run(o); err != nil || len(fs) != 0 {
		t.Fatalf("first build: %v %v", err, fs)
	}
	o.Out += string(filepath.Separator)
	if _, fs, err := Run(o); err != nil || len(fs) != 0 {
		t.Fatalf("rebuild with a trailing separator: %v %v", err, fs)
	}
	if _, err := os.Stat(filepath.Join(o.Out, uibundle.ManifestPath)); err != nil {
		t.Fatalf("previous build was lost: %v", err)
	}
}
