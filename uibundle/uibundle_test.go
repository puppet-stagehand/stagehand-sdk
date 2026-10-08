package uibundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puppet-stagehand/stagehand-sdk/manifest"
)

const helloDir = "testdata/hello/ui"

func readHello(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(helloDir, ManifestPath))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func hasCode(fs []manifest.Finding, code string) bool {
	for _, f := range fs {
		if f.Code == code {
			return true
		}
	}
	return false
}

func codes(fs []manifest.Finding) []string {
	var c []string
	for _, f := range fs {
		c = append(c, f.Code)
	}
	return c
}

// helloPack is the example pack with the real digest of the fixture.
func helloPack(t *testing.T) *manifest.Manifest {
	t.Helper()
	raw, err := os.ReadFile("../examples/hello/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	pm, fs := manifest.Parse(raw)
	if len(fs) > 0 {
		t.Fatal(fs)
	}
	pm.UIDigest = DigestOf(readHello(t))
	return pm
}

func TestParseAcceptsHelloFixture(t *testing.T) {
	um, fs := Parse(readHello(t))
	if len(fs) != 0 {
		t.Fatalf("findings: %v", fs)
	}
	if um.Entry != "index.js" || um.Slots["nodeDetailTab"].Label != "Hello" {
		t.Fatalf("unexpected parse: %+v", um)
	}
}

// mutate decodes the hello index, applies fn, and re-encodes it.
func mutate(t *testing.T, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(readHello(t), &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func files(m map[string]any) map[string]any { return m["files"].(map[string]any) }

func TestParseRejects(t *testing.T) {
	okFile := map[string]any{"sha256": strings.Repeat("a", 64), "size": 1, "content_type": "text/javascript"}
	cases := []struct {
		name string
		raw  func() []byte
		code string
	}{
		{"format 2", func() []byte { return mutate(t, func(m map[string]any) { m["format"] = 2 }) }, "ui_format_unsupported"},
		{"unknown top-level field", func() []byte { return mutate(t, func(m map[string]any) { m["extra"] = 1 }) }, "ui_manifest_unparseable"},
		{"entry not listed", func() []byte { return mutate(t, func(m map[string]any) { m["entry"] = "other.js" }) }, "ui_entry_not_listed"},
		{"entry missing", func() []byte { return mutate(t, func(m map[string]any) { delete(m, "entry") }) }, "ui_entry_missing"},
		{"entry not javascript", func() []byte {
			return mutate(t, func(m map[string]any) { files(m)["index.js"].(map[string]any)["content_type"] = "text/css" })
		}, "ui_entry_not_javascript"},
		{"sandbox entry not listed", func() []byte { return mutate(t, func(m map[string]any) { m["sandbox_entry"] = "sb.js" }) }, "ui_entry_not_listed"},
		{"slot outside the manifest set", func() []byte {
			return mutate(t, func(m map[string]any) { m["slots"].(map[string]any)["sidebar"] = map[string]any{"label": "X"} })
		}, "ui_slot_unknown"},
		{"label of 33 characters", func() []byte {
			return mutate(t, func(m map[string]any) {
				m["slots"].(map[string]any)["nodeDetailTab"] = map[string]any{"label": strings.Repeat("x", 33)}
			})
		}, "ui_slot_label_invalid"},
		{"empty label", func() []byte {
			return mutate(t, func(m map[string]any) { m["slots"].(map[string]any)["nodeDetailTab"] = map[string]any{"label": ""} })
		}, "ui_slot_label_invalid"},
		{"label with markup", func() []byte {
			return mutate(t, func(m map[string]any) {
				m["slots"].(map[string]any)["nodeDetailTab"] = map[string]any{"label": "<b>x</b>"}
			})
		}, "ui_slot_label_invalid"},
		{"path with ..", func() []byte { return mutate(t, func(m map[string]any) { files(m)["../x.js"] = okFile }) }, "ui_path_invalid"},
		{"leading slash", func() []byte { return mutate(t, func(m map[string]any) { files(m)["/x.js"] = okFile }) }, "ui_path_invalid"},
		{"backslash", func() []byte { return mutate(t, func(m map[string]any) { files(m)[`a\x.js`] = okFile }) }, "ui_path_invalid"},
		{"NUL", func() []byte { return mutate(t, func(m map[string]any) { files(m)["a\x00.js"] = okFile }) }, "ui_path_invalid"},
		{"dot segment", func() []byte { return mutate(t, func(m map[string]any) { files(m)["a/./x.js"] = okFile }) }, "ui_path_invalid"},
		{"257 files", func() []byte {
			return mutate(t, func(m map[string]any) {
				for i := 0; i < 256; i++ {
					files(m)[fmt.Sprintf("f%03d.js", i)] = okFile
				}
			})
		}, "ui_too_many_files"},
		{"file over 8 MiB", func() []byte {
			return mutate(t, func(m map[string]any) { files(m)["index.js"].(map[string]any)["size"] = MaxFileBytes + 1 })
		}, "ui_file_too_large"},
		{"bundle over 16 MiB", func() []byte {
			return mutate(t, func(m map[string]any) {
				for i := 0; i < 3; i++ {
					files(m)[fmt.Sprintf("big%d.js", i)] = map[string]any{"sha256": strings.Repeat("a", 64), "size": MaxFileBytes, "content_type": "text/javascript"}
				}
			})
		}, "ui_bundle_too_large"},
		{"svg", func() []byte {
			return mutate(t, func(m map[string]any) {
				files(m)["a.svg"] = map[string]any{"sha256": strings.Repeat("a", 64), "size": 1, "content_type": "image/svg+xml"}
			})
		}, "ui_content_type_not_allowed"},
		{"uppercase sha256", func() []byte {
			return mutate(t, func(m map[string]any) { files(m)["index.js"].(map[string]any)["sha256"] = strings.Repeat("A", 64) })
		}, "ui_file_sha256_invalid"},
		{"no slots", func() []byte { return mutate(t, func(m map[string]any) { m["slots"] = map[string]any{} }) }, "ui_slots_empty"},
		{"index over 256 KiB", func() []byte { return []byte(strings.Repeat(" ", MaxManifestBytes+1)) }, "ui_manifest_too_large"},
		{"trailing data", func() []byte { return append(readHello(t), []byte(" {}")...) }, "ui_manifest_unparseable"},
		{"not json", func() []byte { return []byte("nope") }, "ui_manifest_unparseable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, fs := Parse(c.raw())
			if !hasCode(fs, c.code) {
				t.Fatalf("want %s, got %v", c.code, codes(fs))
			}
			for _, f := range fs {
				if f.Fix == "" {
					t.Errorf("finding %s has no fix line", f.Code)
				}
			}
		})
	}
}

func TestSandboxEntryAccepted(t *testing.T) {
	raw := mutate(t, func(m map[string]any) {
		m["sandbox_entry"] = "sandbox.js"
		files(m)["sandbox.js"] = map[string]any{"sha256": strings.Repeat("b", 64), "size": 10, "content_type": "text/javascript"}
	})
	if _, fs := Parse(raw); len(fs) != 0 {
		t.Fatalf("findings: %v", fs)
	}
}

func TestValidPath(t *testing.T) {
	good := []string{"index.js", "a/b/c.css", "chunk-1.2_x.js", strings.Repeat("a", 200)}
	bad := []string{"", "/a", "a//b", "a/../b", "..", ".", "a\\b", "a b", "a\x00b", "é.js", strings.Repeat("a", 201), "a/", "./a"}
	for _, p := range good {
		if !ValidPath(p) {
			t.Errorf("ValidPath(%q) = false", p)
		}
	}
	for _, p := range bad {
		if ValidPath(p) {
			t.Errorf("ValidPath(%q) = true", p)
		}
	}
}

func TestDigestIsOverExactBytes(t *testing.T) {
	raw := readHello(t)
	want := sha256.Sum256(raw)
	if got := DigestOf(raw); got != "sha256:"+hex.EncodeToString(want[:]) {
		t.Fatalf("digest %s", got)
	}
	// Reformatting valid JSON changes the digest: no canonical form exists.
	if DigestOf(append([]byte(" "), raw...)) == DigestOf(raw) {
		t.Fatal("digest ignored a whitespace change")
	}
	// The pack manifest validator must accept the digest we produce.
	pm := helloPack(t)
	for _, f := range manifest.Validate(pm) {
		if strings.HasPrefix(f.Code, "ui_digest") {
			t.Fatalf("manifest validator rejects the digest: %v", f)
		}
	}
}

func TestVerifyHelloIsClean(t *testing.T) {
	if fs := Verify(helloDir, helloPack(t)); len(fs) != 0 {
		t.Fatalf("findings: %v", fs)
	}
}

// copyHello copies the fixture into a temp dir so tests can damage it.
func copyHello(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	for _, n := range []string{ManifestPath, "index.js"} {
		b, err := os.ReadFile(filepath.Join(helloDir, n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, n), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func TestVerifyCatchesDamage(t *testing.T) {
	cases := []struct {
		name string
		do   func(t *testing.T, dir string, pm *manifest.Manifest)
		code string
	}{
		{"wrong ui_digest", func(t *testing.T, _ string, pm *manifest.Manifest) {
			pm.UIDigest = "sha256:" + strings.Repeat("0", 64)
		}, "ui_digest_mismatch"},
		{"index edited after digest", func(t *testing.T, dir string, _ *manifest.Manifest) {
			raw, _ := os.ReadFile(filepath.Join(dir, ManifestPath))
			_ = os.WriteFile(filepath.Join(dir, ManifestPath), append(raw, '\n'), 0o644)
		}, "ui_digest_mismatch"},
		{"flipped byte", func(t *testing.T, dir string, _ *manifest.Manifest) {
			b, _ := os.ReadFile(filepath.Join(dir, "index.js"))
			b[0] ^= 0x01
			_ = os.WriteFile(filepath.Join(dir, "index.js"), b, 0o644)
		}, "ui_file_hash_mismatch"},
		{"size drift", func(t *testing.T, dir string, _ *manifest.Manifest) {
			f, _ := os.OpenFile(filepath.Join(dir, "index.js"), os.O_APPEND|os.O_WRONLY, 0o644)
			_, _ = f.WriteString("//")
			_ = f.Close()
		}, "ui_file_size_mismatch"},
		{"missing file", func(t *testing.T, dir string, _ *manifest.Manifest) { _ = os.Remove(filepath.Join(dir, "index.js")) }, "ui_file_missing"},
		{"unlisted file", func(t *testing.T, dir string, _ *manifest.Manifest) {
			_ = os.WriteFile(filepath.Join(dir, "stray.js"), []byte("x"), 0o644)
		}, "ui_file_unlisted"},
		{"missing index", func(t *testing.T, dir string, _ *manifest.Manifest) { _ = os.Remove(filepath.Join(dir, ManifestPath)) }, "ui_manifest_missing"},
		{"slot declared but not provided", func(t *testing.T, _ string, pm *manifest.Manifest) {
			pm.Slots = append(pm.Slots, "settingsPanel")
		}, "ui_slot_missing"},
		{"slot provided but not declared", func(t *testing.T, _ string, pm *manifest.Manifest) {
			pm.Slots = nil
		}, "ui_slot_not_declared"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, pm := copyHello(t), helloPack(t)
			c.do(t, dir, pm)
			if fs := Verify(dir, pm); !hasCode(fs, c.code) {
				t.Fatalf("want %s, got %v", c.code, codes(fs))
			}
		})
	}
}

func TestVerifyNeverReadsOutsideTheDirectory(t *testing.T) {
	dir := copyHello(t)
	secret := filepath.Join(filepath.Dir(dir), "secret.js")
	if err := os.WriteFile(secret, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := mutate(t, func(m map[string]any) {
		files(m)["../secret.js"] = map[string]any{"sha256": strings.Repeat("a", 64), "size": 1, "content_type": "text/javascript"}
	})
	if err := os.WriteFile(filepath.Join(dir, ManifestPath), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	fs := Verify(dir, nil)
	if !hasCode(fs, "ui_path_invalid") {
		t.Fatalf("want ui_path_invalid, got %v", codes(fs))
	}
	if hasCode(fs, "ui_file_hash_mismatch") || hasCode(fs, "ui_file_size_mismatch") {
		t.Fatalf("an invalid path was read from disk: %v", codes(fs))
	}
}

func TestShimsCoverTheExternals(t *testing.T) {
	for _, m := range []string{"react", "react/jsx-runtime", "react-dom", "@stagehand/console-ui"} {
		u, ok := Shims[m]
		if !ok || !strings.HasPrefix(u, "/runtime/v1/") || !strings.HasSuffix(u, ".js") {
			t.Errorf("Shims[%q] = %q", m, u)
		}
	}
	if len(ConsoleUIPrimitives) != 20 {
		t.Errorf("console-ui exports %d primitives; the contract lists 20", len(ConsoleUIPrimitives))
	}
}

func TestLabelProblem(t *testing.T) {
	for _, ok := range []string{"Hello", "Node detail", strings.Repeat("x", 32)} {
		if p := LabelProblem(ok); p != "" {
			t.Errorf("LabelProblem(%q) = %q", ok, p)
		}
	}
	for _, bad := range []string{"", strings.Repeat("x", 33), " lead", "trail ", "a<b", "a\x00b"} {
		if LabelProblem(bad) == "" {
			t.Errorf("LabelProblem(%q) accepted", bad)
		}
	}
}
