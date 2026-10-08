//go:build harness

// The container-driving half of the real-tool harness. Every test here is named
// TestHarness* and carries the harness build tag, so the default go test ./...
// never starts a container. Run it through ./harness/run.sh, which builds and
// starts the containers, exports HARNESS_PROJECT, and tears everything down.
//
// HARNESS_RECORD=1 rewrites the fixtures from the real tools; without it the
// tests compare against the committed fixtures.
package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// The driver's limits. Every one is a named constant with the threat it
// enforces, following the host/local/git_exec.go convention.
const (
	// execTimeout bounds one command run inside a container, so a hung tool
	// (for example a git fetch against a dead remote) cannot hold the whole
	// harness run open.
	execTimeout = 3 * time.Minute

	// composeUpTimeout bounds the compose bookkeeping calls made by the driver
	// (service lookups), which move no data and so have no business taking long.
	composeUpTimeout = 2 * time.Minute

	// runnerService and deployUser name the compose service and the non-root
	// account every tool runs as. Root would hide permission faults (T-13-21).
	runnerService = "runner"
	deployUser    = "deploy"

	// canaryPrefix marks the secret-leak canary (T-13-22).
	canaryPrefix = "STAGEHAND-CANARY-"
)

// canary is generated once per test binary: a fake secret passed into every
// harness exec and asserted absent from everything captured or committed.
var canary = newCanary()

func newCanary() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("harness: cannot generate canary: " + err.Error())
	}
	return canaryPrefix + hex.EncodeToString(b)
}

// execResult is what one container command produced.
type execResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// combined is stdout then stderr, the text expect_lines are matched against.
func (r execResult) combined() string { return r.Stdout + r.Stderr }

// harnessProject returns the compose project name run.sh exported, or fails the
// test with the one instruction that fixes it.
func harnessProject(t *testing.T) string {
	t.Helper()
	p := os.Getenv("HARNESS_PROJECT")
	if p == "" {
		t.Fatalf("HARNESS_PROJECT is not set: run the harness through ./harness/run.sh")
	}
	return p
}

// composeExec runs argv in the runner container as the given user.
func composeExec(ctx context.Context, t *testing.T, user string, argv ...string) execResult {
	t.Helper()
	return composeExecService(ctx, t, runnerService, user, argv...)
}

// composeExecService runs argv in one compose service as the given user, via
// os/exec with an argument vector (never a joined shell string), under a
// timeout, with the canary in the environment.
func composeExecService(ctx context.Context, t *testing.T, service, user string, argv ...string) execResult {
	t.Helper()
	project := harnessProject(t)

	args := []string{
		"compose", "-f", "compose.yaml", "-p", project,
		"exec", "-T", "-u", user,
		"-e", "STAGEHAND_CANARY_SECRET=" + canary,
		service,
	}
	args = append(args, argv...)

	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := execResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return res
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res
	}
	if ctx.Err() != nil {
		t.Fatalf("docker compose exec timed out after %s running %q", execTimeout, argv[0])
	}
	t.Fatalf("docker compose exec could not run %q: %v", argv[0], err)
	return res
}

// requireRunner fails the test unless the runner service is up in this
// checkout's compose project.
func requireRunner(t *testing.T) {
	t.Helper()
	project := harnessProject(t)
	ctx, cancel := context.WithTimeout(context.Background(), composeUpTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "compose", "-f", "compose.yaml", "-p", project,
		"ps", "--status", "running", "-q", runnerService).Output()
	if err != nil {
		t.Fatalf("cannot list the runner service: %v", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Fatalf("the runner service is not running: run the harness through ./harness/run.sh")
	}
}

// assertNoCanary fails the test if the canary appears in any of the texts. It
// reports the position, never the canary itself.
func assertNoCanary(t *testing.T, texts ...string) {
	t.Helper()
	for i, s := range texts {
		if strings.Contains(s, canary) {
			t.Fatalf("secret-leak canary found in captured text #%d: a secret escaped into tool output", i)
		}
	}
}

var (
	reSHA      = regexp.MustCompile(`\b[0-9a-f]{40}\b`)
	reTS       = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z| ?[+-]\d{2}:?\d{2})?`)
	reDuration = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|µs|ms|s)\b|\b\d+(?:\.\d+)? seconds?\b`)
)

// normalise makes tool output comparable across runs: 40-hex commit SHAs become
// <SHA>, ISO and r10k timestamps become <TS>, durations become <DUR>.
func normalise(s string) string {
	s = reSHA.ReplaceAllString(s, "<SHA>")
	s = reTS.ReplaceAllString(s, "<TS>")
	s = reDuration.ReplaceAllString(s, "<DUR>")
	return s
}

// normaliseValue applies normalise to every string inside a parsed JSON value.
func normaliseValue(v any) any {
	switch x := v.(type) {
	case string:
		return normalise(x)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = normaliseValue(x[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normaliseValue(e)
		}
		return out
	default:
		return v
	}
}

// sortedKeys returns the keys of m in order.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// fixturePath is where a fixture lives, relative to this package directory.
func fixturePath(tool, version, scenario string) string {
	return filepath.Join("fixtures", tool+"-"+version, scenario+".json")
}

// encodeFixture renders a fixture as indented JSON with sorted keys and a
// trailing newline. The round trip through a map is what sorts the keys.
func encodeFixture(fx Fixture) ([]byte, error) {
	raw, err := json.Marshal(fx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	// An Encoder with HTML escaping off keeps <SHA> and <TS> readable; map keys
	// are emitted in sorted order, and Encode adds the trailing newline.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// recordOrCompare is the single place fixtures are written or checked. With
// HARNESS_RECORD=1 it writes the fixture; otherwise it loads the committed one
// and compares exit_code and marker_keys exactly, requires every expect_lines
// entry to appear in the normalised output, and names the field that differs.
func recordOrCompare(t *testing.T, fx Fixture) {
	t.Helper()
	fx.Stdout = normalise(fx.Stdout)
	fx.Stderr = normalise(fx.Stderr)
	if fx.MarkerBefore != nil {
		fx.MarkerBefore = normaliseValue(fx.MarkerBefore).(map[string]any)
	}
	if fx.MarkerAfter != nil {
		fx.MarkerAfter = normaliseValue(fx.MarkerAfter).(map[string]any)
	}
	path := fixturePath(fx.Tool, fx.ToolVersion, fx.Scenario)

	if os.Getenv("HARNESS_RECORD") == "1" {
		data, err := encodeFixture(fx)
		if err != nil {
			t.Fatalf("cannot encode fixture %s: %v", path, err)
		}
		assertNoCanary(t, string(data))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("cannot create fixture directory: %v", err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("cannot write fixture %s: %v", path, err)
		}
		t.Logf("recorded %s", path)
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no recorded fixture at %s (%v): record it with HARNESS_RECORD=1 ./harness/run.sh", path, err)
	}
	var want Fixture
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("fixture %s is not valid JSON: %v", path, err)
	}
	if want.ExitCode != fx.ExitCode {
		t.Errorf("fixture %s: field exit_code: recorded %d, observed %d", path, want.ExitCode, fx.ExitCode)
	}
	if strings.Join(want.MarkerKeys, ",") != strings.Join(fx.MarkerKeys, ",") {
		t.Errorf("fixture %s: field marker_keys: recorded %v, observed %v", path, want.MarkerKeys, fx.MarkerKeys)
	}
	if want.RunAs != fx.RunAs {
		t.Errorf("fixture %s: field run_as: recorded %q, observed %q", path, want.RunAs, fx.RunAs)
	}
	if strings.Join(want.Argv, "\x00") != strings.Join(fx.Argv, "\x00") {
		t.Errorf("fixture %s: field argv: recorded %q, observed %q", path, want.Argv, fx.Argv)
	}
	out := fx.Stdout + fx.Stderr
	for _, line := range want.ExpectLines {
		if !strings.Contains(out, line) {
			t.Errorf("fixture %s: field expect_lines: %q not found in the normalised output", path, line)
		}
	}
}

// platform returns the architecture the container actually ran on, from uname.
func platform(t *testing.T) string {
	t.Helper()
	res := composeExec(context.Background(), t, deployUser, "uname", "-m")
	if res.ExitCode != 0 {
		t.Fatalf("uname -m failed in the runner: exit %d", res.ExitCode)
	}
	switch m := strings.TrimSpace(res.Stdout); m {
	case "aarch64", "arm64":
		return "linux/arm64"
	case "x86_64", "amd64":
		return "linux/amd64"
	default:
		t.Fatalf("unrecognised runner architecture %q", m)
		return ""
	}
}

// writeInRunner writes content to path inside the runner as the deploy user.
// The content travels as an argument to a fixed script, never spliced into it.
func writeInRunner(t *testing.T, path, content string) {
	t.Helper()
	res := composeExec(context.Background(), t, deployUser, "sh", "-c",
		`mkdir -p "$(dirname "$1")" && printf '%s' "$2" > "$1"`, "write", path, content)
	if res.ExitCode != 0 {
		t.Fatalf("cannot write %s in the runner: exit %d: %s", path, res.ExitCode, res.Stderr)
	}
}

// toolVersion runs `<tool> <args...>` and returns the last field of the first
// line (for example "r10k 5.0.3" gives "5.0.3"), so a fixture records the
// version the real binary reported, not a constant typed into the test.
func toolVersion(t *testing.T, argv ...string) string {
	t.Helper()
	res := composeExec(context.Background(), t, deployUser, argv...)
	if res.ExitCode != 0 {
		t.Fatalf("%v failed: exit %d: %s", argv, res.ExitCode, res.Stderr)
	}
	fields := strings.Fields(strings.SplitN(strings.TrimSpace(res.Stdout), "\n", 2)[0])
	if len(fields) == 0 {
		t.Fatalf("%v printed no version", argv)
	}
	return fields[len(fields)-1]
}

// TestHarnessCanaryIsInjected proves the canary reaches the runner's
// environment, so the "canary absent" assertions elsewhere are meaningful. It
// prints only a yes/no, never the value.
func TestHarnessCanaryIsInjected(t *testing.T) {
	requireRunner(t)
	res := composeExec(context.Background(), t, deployUser, "sh", "-c",
		`case "$STAGEHAND_CANARY_SECRET" in STAGEHAND-CANARY-*) echo injected ;; *) echo missing ;; esac`)
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "injected" {
		t.Fatalf("the canary was not injected into the runner environment (exit %d)", res.ExitCode)
	}
}

// TestHarnessR10kDeploySuccess runs a real r10k deploy of a local file:// git
// control repo, as the non-root deploy user, and records or compares it.
func TestHarnessR10kDeploySuccess(t *testing.T) {
	requireRunner(t)
	ctx := context.Background()

	version := toolVersion(t, "r10k", "version")

	fixture := composeExec(ctx, t, deployUser, "bash", "/opt/harness-scripts/make-git-fixture.sh")
	if fixture.ExitCode != 0 {
		t.Fatalf("make-git-fixture.sh failed: exit %d: %s", fixture.ExitCode, fixture.Stderr)
	}
	lines := strings.Split(strings.TrimSpace(fixture.Stdout), "\n")
	head := strings.TrimSpace(lines[len(lines)-1])
	if !reSHA.MatchString(head) {
		t.Fatalf("make-git-fixture.sh did not end with a commit SHA: %q", head)
	}

	if res := composeExec(ctx, t, deployUser, "rm", "-rf", "/srv/work/r10k"); res.ExitCode != 0 {
		t.Fatalf("cannot reset /srv/work/r10k: exit %d", res.ExitCode)
	}
	cfg := "---\ncachedir: /srv/work/r10k/cache\nsources:\n  control:\n" +
		"    remote: file:///srv/git/control.git\n    basedir: /srv/work/r10k/environments\n"
	writeInRunner(t, "/srv/work/r10k/r10k.yaml", cfg)

	argv := []string{"r10k", "deploy", "environment", "production", "--modules",
		"--config", "/srv/work/r10k/r10k.yaml", "-v", "info"}
	res := composeExec(ctx, t, deployUser, argv...)
	if res.ExitCode != 0 {
		t.Fatalf("r10k deploy exited %d: %s", res.ExitCode, res.Stderr)
	}

	markerRes := composeExec(ctx, t, deployUser, "cat", "/srv/work/r10k/environments/production/.r10k-deploy.json")
	if markerRes.ExitCode != 0 {
		t.Fatalf("no .r10k-deploy.json after a successful deploy: exit %d", markerRes.ExitCode)
	}
	var marker map[string]any
	if err := json.Unmarshal([]byte(markerRes.Stdout), &marker); err != nil {
		t.Fatalf(".r10k-deploy.json is not valid JSON: %v", err)
	}

	assertNoCanary(t, res.Stdout, res.Stderr, markerRes.Stdout, markerRes.Stderr, fixture.Stdout, fixture.Stderr)

	if sig, _ := marker["signature"].(string); sig != head {
		t.Fatalf("marker signature does not equal the control repo HEAD: field signature")
	}
	if ok, _ := marker["deploy_success"].(bool); !ok {
		t.Fatalf("marker deploy_success is not true after a successful deploy")
	}

	recordOrCompare(t, Fixture{
		Tool:               "r10k",
		ToolVersion:        version,
		Scenario:           "deploy-success",
		RecordedOnPlatform: platform(t),
		Argv:               argv,
		RunAs:              deployUser,
		ExitCode:           res.ExitCode,
		Stdout:             res.Stdout,
		Stderr:             res.Stderr,
		ExpectLines:        []string{"Deploying environment", "Environment production is now at <SHA>", "Deploying module to"},
		MarkerKeys:         sortedKeys(marker),
		MarkerAfter:        marker,
		Notes: fmt.Sprintf("Real r10k deploy of a local file:// control repo (branch production, one Git module) run as %s. "+
			"Code Manager coexistence was not run (D-10).", deployUser),
	})
}
