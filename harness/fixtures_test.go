// Package harness holds the real-tool evidence harness: containers that run
// r10k, g10k and Bolt for real, and the recorded fixtures those runs produce.
//
// This file is untagged, so the default go test ./... compiles it and (once the
// validator lands) checks the committed fixtures offline with no Docker. The
// container-driving tests live in harness_test.go behind the harness build tag
// and run only through harness/run.sh. See docs/harness.md for the overview.
package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture is one recorded real-tool observation, stored as
// harness/fixtures/<tool>-<tool_version>/<scenario>.json. Both the tagged
// driver and the untagged validator use this type.
//
// Fixtures are evidence, so every field is plain data: nothing here may hold a
// credential, a private key, an approver token or the operator's registry host.
type Fixture struct {
	Tool               string   `json:"tool"`
	ToolVersion        string   `json:"tool_version"`
	Scenario           string   `json:"scenario"`
	RecordedOnPlatform string   `json:"recorded_on_platform"`
	Argv               []string `json:"argv"`
	RunAs              string   `json:"run_as"`
	ExitCode           int      `json:"exit_code"`
	Stdout             string   `json:"stdout"`
	Stderr             string   `json:"stderr"`

	// ExpectLines are substrings that must appear in the normalised output in
	// compare mode. The comparison is a subset check so harmless tool chatter
	// does not flake it; the full normalised output is still stored above.
	ExpectLines []string `json:"expect_lines,omitempty"`

	// MarkerKeys is the sorted key set of the deploy marker the tool wrote.
	MarkerKeys []string `json:"marker_keys,omitempty"`

	// MarkerBefore and MarkerAfter hold the parsed marker, with timestamps and
	// commit SHAs normalised.
	MarkerBefore map[string]any `json:"marker_before,omitempty"`
	MarkerAfter  map[string]any `json:"marker_after,omitempty"`

	// Tree hashes prove read-only behaviour: the same live tree hashed before
	// and after, with and without the deploy markers.
	TreeHashBefore            string `json:"tree_hash_before,omitempty"`
	TreeHashAfter             string `json:"tree_hash_after,omitempty"`
	TreeHashExclMarkersBefore string `json:"tree_hash_excl_markers_before,omitempty"`
	TreeHashExclMarkersAfter  string `json:"tree_hash_excl_markers_after,omitempty"`

	FilesChanged []string `json:"files_changed,omitempty"`

	// Extra carries scenario-specific evidence such as an HTTP status code or
	// file ownership.
	Extra map[string]any `json:"extra,omitempty"`

	Notes string `json:"notes,omitempty"`
}

// ---- offline validator (RED stage: the helpers below are deliberate no-ops) ----

// readKeyValues reads a KEY=VALUE file, ignoring blank lines and # comments.
func readKeyValues(path string) (map[string]string, error) { return nil, nil }

// checkFixturesValid returns one problem string per defect found under root.
func checkFixturesValid(root string) []string { return nil }

// checkFixtureDirsPinned returns one problem string per fixture directory whose
// version is not the pinned version of its tool.
func checkFixtureDirsPinned(root, versionsEnv, imagesLock string) []string { return nil }

// checkFixturesCarryNoSecrets returns one problem string per secret-shaped
// string found under root or in imagesLock.
func checkFixturesCarryNoSecrets(root, imagesLock, harborHost string) []string { return nil }

// ---- test helpers ----

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func goodFixture() Fixture {
	return Fixture{
		Tool:               "r10k",
		ToolVersion:        "5.0.3",
		Scenario:           "deploy-success",
		RecordedOnPlatform: "linux/amd64",
		Argv:               []string{"r10k", "deploy", "environment", "production"},
		RunAs:              "deploy",
		ExitCode:           0,
	}
}

func writeFixture(t *testing.T, root, dir string, fx Fixture) string {
	t.Helper()
	data, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, dir, fx.Scenario+".json")
	mustWrite(t, path, string(data)+"\n")
	return path
}

// editFixtureJSON rewrites a fixture file through a generic map.
func editFixtureJSON(t *testing.T, path string, edit func(m map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, string(out)+"\n")
}

// wantProblem fails unless some problem mentions every wanted fragment.
func wantProblem(t *testing.T, problems []string, fragments ...string) {
	t.Helper()
	for _, p := range problems {
		ok := true
		for _, f := range fragments {
			if !strings.Contains(p, f) {
				ok = false
				break
			}
		}
		if ok {
			return
		}
	}
	t.Fatalf("expected a problem mentioning %q, got %q", fragments, problems)
}

const testVersionsEnv = "# pins\nR10K_VERSION=5.0.3\nG10K_VERSION=0.10.0\n\nBOLT_VERSION=4.0.0\n"

// ---- the three offline checks ----

func TestFixturesAreValid(t *testing.T) {
	t.Run("committed fixtures are valid", func(t *testing.T) {
		if problems := checkFixturesValid("fixtures"); len(problems) != 0 {
			t.Fatalf("committed fixtures are invalid: %q", problems)
		}
		if _, err := os.Stat(filepath.Join("fixtures", "r10k-5.0.3", "deploy-success.json")); err != nil {
			t.Fatalf("the first recorded fixture is missing: %v", err)
		}
	})

	t.Run("a good tree passes", func(t *testing.T) {
		root := t.TempDir()
		writeFixture(t, root, "r10k-5.0.3", goodFixture())
		if problems := checkFixturesValid(root); len(problems) != 0 {
			t.Fatalf("a good tree was flagged: %q", problems)
		}
	})

	for _, field := range []string{"tool", "tool_version", "scenario", "recorded_on_platform", "argv", "run_as"} {
		t.Run("missing "+field+" is refused", func(t *testing.T) {
			root := t.TempDir()
			path := writeFixture(t, root, "r10k-5.0.3", goodFixture())
			editFixtureJSON(t, path, func(m map[string]any) { delete(m, field) })
			wantProblem(t, checkFixturesValid(root), field)
		})
	}

	t.Run("empty argv is refused", func(t *testing.T) {
		root := t.TempDir()
		path := writeFixture(t, root, "r10k-5.0.3", goodFixture())
		editFixtureJSON(t, path, func(m map[string]any) { m["argv"] = []any{} })
		wantProblem(t, checkFixturesValid(root), "argv")
	})

	t.Run("directory name must equal tool-version", func(t *testing.T) {
		root := t.TempDir()
		writeFixture(t, root, "r10k-9.9.9", goodFixture())
		wantProblem(t, checkFixturesValid(root), "directory", "r10k-5.0.3")
	})

	t.Run("file name must equal scenario", func(t *testing.T) {
		root := t.TempDir()
		path := writeFixture(t, root, "r10k-5.0.3", goodFixture())
		if err := os.Rename(path, filepath.Join(filepath.Dir(path), "other.json")); err != nil {
			t.Fatal(err)
		}
		wantProblem(t, checkFixturesValid(root), "file name", "deploy-success.json")
	})

	t.Run("invalid JSON is refused", func(t *testing.T) {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "r10k-5.0.3", "broken.json"), "{not json")
		wantProblem(t, checkFixturesValid(root), "broken.json", "parse")
	})

	t.Run("an unknown field is refused", func(t *testing.T) {
		root := t.TempDir()
		path := writeFixture(t, root, "r10k-5.0.3", goodFixture())
		editFixtureJSON(t, path, func(m map[string]any) { m["surprise"] = "x" })
		wantProblem(t, checkFixturesValid(root), "surprise")
	})
}

func TestFixtureDirsArePinned(t *testing.T) {
	t.Run("committed fixtures match the pins", func(t *testing.T) {
		if problems := checkFixtureDirsPinned("fixtures", "versions.env", "images.lock"); len(problems) != 0 {
			t.Fatalf("committed fixtures are not pinned to versions.env: %q", problems)
		}
	})

	t.Run("a matching tree passes and root docs are allowed", func(t *testing.T) {
		root := t.TempDir()
		writeFixture(t, root, "r10k-5.0.3", goodFixture())
		mustWrite(t, filepath.Join(root, "README.md"), "notes\n")
		mustWrite(t, filepath.Join(root, "code-manager.UNVERIFIED.md"), "not run\n")
		env := filepath.Join(t.TempDir(), "versions.env")
		mustWrite(t, env, testVersionsEnv)
		if problems := checkFixtureDirsPinned(root, env, filepath.Join(t.TempDir(), "absent.lock")); len(problems) != 0 {
			t.Fatalf("a matching tree was flagged: %q", problems)
		}
	})

	t.Run("a wrong version is refused", func(t *testing.T) {
		root := t.TempDir()
		fx := goodFixture()
		fx.ToolVersion = "9.9.9"
		writeFixture(t, root, "r10k-9.9.9", fx)
		env := filepath.Join(t.TempDir(), "versions.env")
		mustWrite(t, env, testVersionsEnv)
		wantProblem(t, checkFixtureDirsPinned(root, env, ""), "R10K_VERSION", "9.9.9")
	})

	t.Run("an unknown tool is refused", func(t *testing.T) {
		root := t.TempDir()
		fx := goodFixture()
		fx.Tool = "mystery"
		fx.ToolVersion = "1.0.0"
		writeFixture(t, root, "mystery-1.0.0", fx)
		env := filepath.Join(t.TempDir(), "versions.env")
		mustWrite(t, env, testVersionsEnv)
		wantProblem(t, checkFixtureDirsPinned(root, env, ""), "mystery")
	})

	t.Run("a stray file at the fixtures root is refused", func(t *testing.T) {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "notes.txt"), "stray\n")
		env := filepath.Join(t.TempDir(), "versions.env")
		mustWrite(t, env, testVersionsEnv)
		wantProblem(t, checkFixtureDirsPinned(root, env, ""), "notes.txt")
	})

	t.Run("a puppetserver fixture needs images.lock", func(t *testing.T) {
		root := t.TempDir()
		fx := goodFixture()
		fx.Tool = "puppetserver"
		fx.ToolVersion = "8.7.0"
		writeFixture(t, root, "puppetserver-8.7.0", fx)
		env := filepath.Join(t.TempDir(), "versions.env")
		mustWrite(t, env, testVersionsEnv)

		wantProblem(t, checkFixtureDirsPinned(root, env, filepath.Join(t.TempDir(), "absent.lock")), "images.lock")

		lock := filepath.Join(t.TempDir(), "images.lock")
		mustWrite(t, lock, "SOMETHING_ELSE=1\n")
		wantProblem(t, checkFixtureDirsPinned(root, env, lock), "PUPPETSERVER_VERSION")

		mustWrite(t, lock, "# lock\nPUPPETSERVER_VERSION=8.7.0\n")
		if problems := checkFixtureDirsPinned(root, env, lock); len(problems) != 0 {
			t.Fatalf("a pinned puppetserver fixture was flagged: %q", problems)
		}
	})
}

func TestFixturesCarryNoSecrets(t *testing.T) {
	t.Run("committed fixtures are clean", func(t *testing.T) {
		host := os.Getenv("STAGEHAND_HARBOR_HOST")
		if problems := checkFixturesCarryNoSecrets("fixtures", "images.lock", host); len(problems) != 0 {
			t.Fatalf("committed fixtures carry secret-shaped content: %q", problems)
		}
	})

	t.Run("a clean tree passes, even with no host set", func(t *testing.T) {
		root := t.TempDir()
		writeFixture(t, root, "r10k-5.0.3", goodFixture())
		if problems := checkFixturesCarryNoSecrets(root, "", ""); len(problems) != 0 {
			t.Fatalf("a clean tree was flagged: %q", problems)
		}
	})

	for name, planted := range map[string]string{
		"canary":         "STAGEHAND-CANARY-0123456789abcdef",
		"private key":    "-----BEGIN PRIVATE KEY-----",
		"approver token": "stagehand-approver-token: abc",
	} {
		t.Run("a planted "+name+" is refused", func(t *testing.T) {
			root := t.TempDir()
			fx := goodFixture()
			fx.Stdout = "output " + planted
			writeFixture(t, root, "r10k-5.0.3", fx)
			wantProblem(t, checkFixturesCarryNoSecrets(root, "", ""), "deploy-success.json")
		})
	}

	t.Run("the registry host is refused in fixtures and images.lock", func(t *testing.T) {
		const host = "registry.example.invalid"
		root := t.TempDir()
		fx := goodFixture()
		fx.Notes = "pulled from " + host
		writeFixture(t, root, "r10k-5.0.3", fx)
		wantProblem(t, checkFixturesCarryNoSecrets(root, "", host), "deploy-success.json")

		clean := t.TempDir()
		writeFixture(t, clean, "r10k-5.0.3", goodFixture())
		lock := filepath.Join(t.TempDir(), "images.lock")
		mustWrite(t, lock, "IMAGE="+host+"/p/img\n")
		wantProblem(t, checkFixturesCarryNoSecrets(clean, lock, host), "images.lock")
	})
}
