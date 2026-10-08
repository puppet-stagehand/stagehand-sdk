//go:build harness

// Real Bolt 4.0.0 behaviour, recorded as version-pinned fixtures (FND-05, D-11).
//
// Every scenario runs `bolt ... --project /opt/stagehand-bolt --format json` as
// the non-root deploy user inside the runner. Task parameters travel on stdin
// with `--params -`, never on the Bolt command line. Every target is localhost
// (Bolt's local transport) or a name under the reserved .invalid domain (RFC
// 6761), so no scenario can reach a real host (T-13-33).
//
// The fixtures keep what Bolt actually did. Where an observation disagrees with
// the research baseline the fixture keeps the observed value and the plan
// summary names the contradiction.
package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	// boltProject is the Bolt project plan 13-06 builds into the runner image.
	boltProject = "/opt/stagehand-bolt"

	// boltDebugLog is where Bolt writes its debug log: the project directory.
	boltDebugLog = boltProject + "/bolt-debug.log"

	// approverCanaryPrefix is the offline validator's approver-token marker, so a
	// leak of this canary into a fixture would also be caught by
	// TestFixturesCarryNoSecrets. It stands in for the approver tokens that
	// facets will hand to Bolt in Phase 14 (Pitfall 10).
	approverCanaryPrefix = "stagehand-approver-token-"

	// unreachableTarget and unknownTarget are the only non-local names any
	// scenario uses. The .invalid top-level domain never resolves (RFC 6761).
	unreachableTarget = "ssh://unreachable.invalid"
	unknownTarget     = "nothere.invalid"
)

// approverCanary is an approver-token-shaped fake secret, generated once per
// test binary like the main canary. It travels in the environment of the exec.
var approverCanary = newApproverCanary()

func newApproverCanary() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("harness: cannot generate approver canary: " + err.Error())
	}
	return approverCanaryPrefix + hex.EncodeToString(b)
}

// reElapsed matches Bolt's wall-clock figure so a re-recorded fixture does not
// change on every run.
var reElapsed = regexp.MustCompile(`"elapsed_time":\s*[0-9.eE+-]+`)

func scrubBoltStdout(s string) string {
	return reElapsed.ReplaceAllString(s, `"elapsed_time": "<DUR>"`)
}

// execWithStdin runs argv in the runner as user, feeding stdin to the command
// and adding extraEnv (KEY=value) next to the standard canary. It mirrors
// composeExecService, which has no stdin, and stays inside this file so the
// shared driver is untouched.
func execWithStdin(ctx context.Context, t *testing.T, user, stdin string, extraEnv []string, argv ...string) execResult {
	t.Helper()
	project := harnessProject(t)

	args := []string{
		"compose", "-f", "compose.yaml", "-p", project,
		"exec", "-T", "-u", user,
		"-e", "STAGEHAND_CANARY_SECRET=" + canary,
	}
	for _, e := range extraEnv {
		args = append(args, "-e", e)
	}
	args = append(args, runnerService)
	args = append(args, argv...)

	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = strings.NewReader(stdin)
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

// boltRunOpts are the knobs one scenario can set.
type boltRunOpts struct {
	params   string   // JSON for --params -; empty means no --params
	extraEnv []string // KEY=value pairs added to the exec environment
}

// boltArgs is the Bolt command line for a scenario. Parameters are never on it:
// --params - makes Bolt read them from stdin.
func boltArgs(opts boltRunOpts, args ...string) []string {
	argv := append([]string{"bolt"}, args...)
	if opts.params != "" {
		argv = append(argv, "--params", "-")
	}
	return append(argv, "--project", boltProject, "--format", "json")
}

// boltRun runs bolt <args> --project /opt/stagehand-bolt --format json as the
// deploy user and returns the argv it used with the captured result. The debug
// log is removed first so each scenario's log is its own.
func boltRun(t *testing.T, opts boltRunOpts, args ...string) ([]string, execResult) {
	t.Helper()
	mustSh(t, deployUser, `rm -f "$1"`, boltDebugLog)
	argv := boltArgs(opts, args...)
	res := execWithStdin(context.Background(), t, deployUser, opts.params, opts.extraEnv, argv...)
	return argv, res
}

// parseBolt decodes Bolt's stdout, and only stdout: stderr carries warnings and
// progress that are not part of the JSON contract (Pitfall 8). An empty stdout
// gives a nil map so the caller can record that fact.
func parseBolt(t *testing.T, stdout string) map[string]any {
	t.Helper()
	if strings.TrimSpace(stdout) == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatalf("bolt stdout is not a JSON object: %v", err)
	}
	return m
}

// summariseItems reduces the per-target results to the fields Phase 14 designs
// against: target, action, object, status, the task error kind and the exit code
// the task reported, and the keys of the result value.
func summariseItems(m map[string]any) []any {
	raw, _ := m["items"].([]any)
	out := make([]any, 0, len(raw))
	for _, it := range raw {
		item, _ := it.(map[string]any)
		s := map[string]any{
			"target": item["target"],
			"action": item["action"],
			"object": item["object"],
			"status": item["status"],
		}
		if v, ok := item["value"].(map[string]any); ok {
			s["value_keys"] = anySlice(sortedKeys(v))
			if e, ok := v["_error"].(map[string]any); ok {
				s["error_kind"] = e["kind"]
				if d, ok := e["details"].(map[string]any); ok {
					if code, ok := d["exit_code"]; ok {
						s["exit_code"] = code
					}
				}
			}
		}
		out = append(out, s)
	}
	// Bolt runs targets in parallel and the order of items is not fixed, so sort
	// by target to keep a recording comparable.
	sort.SliceStable(out, func(i, j int) bool {
		return asString(out[i].(map[string]any)["target"]) < asString(out[j].(map[string]any)["target"])
	})
	return out
}

// topLevelError finds an error that is not attached to a target. Bolt 4 puts it
// under a top-level "_error" key in --format json; a bare kind/msg object is
// also recognised so the shape is recorded either way.
func topLevelError(m map[string]any) map[string]any {
	if e, ok := m["_error"].(map[string]any); ok {
		return map[string]any{"kind": e["kind"], "msg": normalise(asString(e["msg"]))}
	}
	if k, ok := m["kind"].(string); ok {
		return map[string]any{"kind": k, "msg": normalise(asString(m["msg"]))}
	}
	return nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// boltBase builds the Extra map every scenario shares: the JSON shape, the
// per-target items, any top-level error, and whether the debug log was written.
func boltBase(t *testing.T, res execResult) map[string]any {
	t.Helper()
	m := parseBolt(t, res.Stdout)
	extra := map[string]any{
		"stdout_is_json": m != nil,
		"debug_log_written": strings.TrimSpace(
			mustSh(t, deployUser, `if [ -s "$1" ]; then echo yes; else echo no; fi`, boltDebugLog)) == "yes",
	}
	if m == nil {
		return extra
	}
	extra["stdout_top_level_keys"] = anySlice(sortedKeys(m))
	extra["items"] = summariseItems(m)
	if tc, ok := m["target_count"]; ok {
		extra["target_count"] = tc
	}
	if e := topLevelError(m); e != nil {
		extra["top_level_error"] = e
	}
	return extra
}

// assertNoBoltCanaries fails the test, naming only where, if either canary is in
// the captured output or the project's debug log. It returns the places found so
// a caller that records the answer can still assert it is empty.
func canaryPlaces(t *testing.T, res execResult) []string {
	t.Helper()
	log := composeExec(context.Background(), t, deployUser, "sh", "-c",
		`[ -f "$1" ] && cat "$1" || true`, "sh", boltDebugLog)
	places := []string{}
	for _, c := range []struct{ label, text string }{
		{"stdout", res.Stdout}, {"stderr", res.Stderr}, {"bolt-debug.log", log.Stdout},
	} {
		if strings.Contains(c.text, canary) {
			places = append(places, "main canary in "+c.label)
		}
		if strings.Contains(c.text, approverCanary) {
			places = append(places, "approver-token canary in "+c.label)
		}
	}
	return places
}

// recordBolt records or compares one Bolt fixture. recordOrCompare covers
// exit_code, argv, run_as and expect_lines; in compare mode this also requires
// every key in Extra to match the recorded value exactly, so a change in Bolt's
// per-target status, error kind or exit code fails the run and names the field.
func recordBolt(t *testing.T, fx Fixture) {
	t.Helper()
	fx.Tool = "bolt"
	fx.ToolVersion = toolVersion(t, "bolt", "--version")
	fx.RecordedOnPlatform = recordedPlatform(t)
	fx.RunAs = deployUser
	fx.Stdout = scrubBoltStdout(fx.Stdout)

	recordOrCompare(t, fx)
	if os.Getenv("HARNESS_RECORD") == "1" {
		return
	}
	path := fixturePath(fx.Tool, fx.ToolVersion, fx.Scenario)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	var want Fixture
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("fixture %s is not valid JSON: %v", path, err)
	}
	// Round-trip the observed Extra through JSON so numbers and slices compare
	// the way they were stored.
	raw, err := json.Marshal(fx.Extra)
	if err != nil {
		t.Fatalf("cannot encode observed extra: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("cannot decode observed extra: %v", err)
	}
	for _, k := range sortedKeys(want.Extra) {
		if !reflect.DeepEqual(want.Extra[k], got[k]) {
			t.Errorf("fixture %s: field extra.%s: recorded %v, observed %v", path, k, want.Extra[k], got[k])
		}
	}
	var extraKeys []string
	for k := range got {
		if _, ok := want.Extra[k]; !ok {
			extraKeys = append(extraKeys, k)
		}
	}
	sort.Strings(extraKeys)
	if len(extraKeys) > 0 {
		t.Errorf("fixture %s: extra has fields the recording does not: %v", path, extraKeys)
	}
}

// TestHarnessBoltTaskOutcomes runs a real Bolt 4.0.0 task against localhost twice:
// once succeeding (echo) and once failing with exit 3 (exit3), and records the
// exit code and per-target JSON for each.
func TestHarnessBoltTaskOutcomes(t *testing.T) {
	requireRunner(t)

	t.Run("task-success", func(t *testing.T) {
		opts := boltRunOpts{params: `{"message": "hello"}`}
		argv, res := boltRun(t, opts, "task", "run", "harness_fixtures::echo", "--targets", "localhost")
		if places := canaryPlaces(t, res); len(places) > 0 {
			t.Fatalf("a canary leaked: %v", places)
		}
		extra := boltBase(t, res)
		recordBolt(t, Fixture{
			Scenario:    "task-success",
			Argv:        argv,
			ExitCode:    res.ExitCode,
			Stdout:      res.Stdout,
			Stderr:      res.Stderr,
			ExpectLines: []string{`"status":"success"`},
			Extra:       extra,
			Notes: "Real Bolt 4.0.0 running harness_fixtures::echo against localhost (local transport). " +
				"Parameters were sent on stdin with --params -. elapsed_time is scrubbed.",
		})
	})

	t.Run("task-failure-exit3", func(t *testing.T) {
		argv, res := boltRun(t, boltRunOpts{}, "task", "run", "harness_fixtures::exit3", "--targets", "localhost")
		if places := canaryPlaces(t, res); len(places) > 0 {
			t.Fatalf("a canary leaked: %v", places)
		}
		extra := boltBase(t, res)
		recordBolt(t, Fixture{
			Scenario:    "task-failure-exit3",
			Argv:        argv,
			ExitCode:    res.ExitCode,
			Stdout:      res.Stdout,
			Stderr:      res.Stderr,
			ExpectLines: []string{`"status":"failure"`, `"kind":"puppetlabs.tasks/task-error"`},
			Extra:       extra,
			Notes: "Real Bolt 4.0.0 running harness_fixtures::exit3 (the task exits 3) against localhost. " +
				"Bolt's own exit code and the task's exit code are different numbers: see extra.items.",
		})
	})
}

// firstItemValue returns the value of the first per-target result, or nil.
func firstItemValue(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	items, _ := m["items"].([]any)
	if len(items) == 0 {
		return nil
	}
	item, _ := items[0].(map[string]any)
	v, _ := item["value"].(map[string]any)
	return v
}

// statuses returns the status of every per-target result.
func statuses(m map[string]any) []string {
	var out []string
	items, _ := m["items"].([]any)
	for _, it := range items {
		item, _ := it.(map[string]any)
		out = append(out, asString(item["status"]))
	}
	return out
}

// unreachableArgs are the options that keep a connection attempt to a .invalid
// name short and free of interactive prompts.
var unreachableArgs = []string{"--connect-timeout", "2", "--no-host-key-check"}

var cachedUnreachableExit = -1

// unreachableExit is the Bolt exit code when every target is unreachable, asked
// once per test binary, so the mixed-targets scenario can say whether its exit
// code differs from total failure.
func unreachableExit(t *testing.T) int {
	t.Helper()
	if cachedUnreachableExit < 0 {
		args := append([]string{"task", "run", "harness_fixtures::echo", "--targets", unreachableTarget}, unreachableArgs...)
		_, res := boltRun(t, boltRunOpts{params: `{"message": "hello"}`}, args...)
		cachedUnreachableExit = res.ExitCode
	}
	return cachedUnreachableExit
}

// edgeCase is one Bolt invocation and what to record about it.
type edgeCase struct {
	scenario string
	opts     boltRunOpts
	args     []string
	expect   []string
	notes    string
	// annotate adds scenario-specific evidence to Extra.
	annotate func(t *testing.T, m map[string]any, res execResult, extra map[string]any)
}

// TestHarnessBoltEdgeCases records Bolt 4.0.0's behaviour for an unreachable
// target, a mix of reachable and unreachable targets, the --noop gate, an
// unknown task, an unknown target string, and a sensitive parameter plus an
// approver-token-shaped value in the environment. Every target is localhost or
// a .invalid name.
func TestHarnessBoltEdgeCases(t *testing.T) {
	requireRunner(t)
	echo := boltRunOpts{params: `{"message": "hello"}`}

	cases := []edgeCase{
		{
			scenario: "unreachable-target",
			opts:     echo,
			args:     append([]string{"task", "run", "harness_fixtures::echo", "--targets", unreachableTarget}, unreachableArgs...),
			expect:   []string{`"status":"failure"`},
			notes: "Real Bolt 4.0.0 against ssh://unreachable.invalid (a name that can never resolve) with a 2 second connect timeout. " +
				"No real host can answer.",
		},
		{
			scenario: "mixed-targets-partial",
			opts:     echo,
			args:     append([]string{"task", "run", "harness_fixtures::echo", "--targets", "localhost," + unreachableTarget}, unreachableArgs...),
			expect:   []string{`"status":"success"`, `"status":"failure"`},
			notes: "One reachable target (localhost) and one unreachable (.invalid) in a single run. " +
				"partial_only_in_items is true when Bolt's exit code is the same as when every target fails, " +
				"so only the per-target status can tell a partial failure from a total one.",
			annotate: func(t *testing.T, m map[string]any, res execResult, extra map[string]any) {
				sts := statuses(m)
				hasOK, hasFail := false, false
				for _, s := range sts {
					hasOK = hasOK || s == "success"
					hasFail = hasFail || s == "failure"
				}
				extra["has_success_and_failure"] = hasOK && hasFail
				extra["partial_only_in_items"] = hasOK && hasFail && res.ExitCode == unreachableExit(t)
			},
		},
		{
			scenario: "noop-unsupported",
			args:     []string{"task", "run", "harness_fixtures::no_noop", "--targets", "localhost", "--noop"},
			expect:   nil,
			notes: "--noop against a task that does not declare supports_noop. task_ran is true if the task's own output " +
				"appears in Bolt's stdout, which would mean the gate did not stop it.",
			annotate: func(t *testing.T, m map[string]any, res execResult, extra map[string]any) {
				extra["task_ran"] = strings.Contains(res.Stdout, `"ran":true`) || strings.Contains(res.Stdout, `"ran": true`)
			},
		},
		{
			scenario: "noop-supported",
			args:     []string{"task", "run", "harness_fixtures::noop_capable", "--targets", "localhost", "--noop"},
			expect:   []string{`"status":"success"`},
			notes:    "--noop against a task that declares supports_noop. task_saw_noop is the PT__noop value the task received.",
			annotate: func(t *testing.T, m map[string]any, res execResult, extra map[string]any) {
				if v := firstItemValue(m); v != nil {
					extra["task_saw_noop"] = v["noop"]
				}
			},
		},
		{
			scenario: "unknown-task",
			args:     []string{"task", "run", "harness_fixtures::does_not_exist", "--targets", "localhost"},
			notes:    "A task name that does not exist on the module path. Records whether any per-target items exist.",
		},
		{
			scenario: "unknown-target",
			opts:     echo,
			args:     append([]string{"task", "run", "harness_fixtures::echo", "--targets", unknownTarget}, unreachableArgs...),
			notes: "A bare string that is not in any inventory. treated_as_hostname is true when Bolt made a per-target " +
				"result whose target is that string, meaning Bolt accepts arbitrary strings as hosts.",
			annotate: func(t *testing.T, m map[string]any, res execResult, extra map[string]any) {
				found := false
				items, _ := m["items"].([]any)
				for _, it := range items {
					if item, ok := it.(map[string]any); ok && strings.Contains(asString(item["target"]), unknownTarget) {
						found = true
					}
				}
				extra["treated_as_hostname"] = found
			},
		},
		{
			scenario: "sensitive-typed-param-rejected",
			opts:     boltRunOpts{params: `{"token": "` + canary + `"}`},
			args:     []string{"task", "run", "harness_fixtures::secret_param_typed", "--targets", "localhost"},
			expect:   []string{`bolt/pal-error`},
			notes: "A task whose token parameter is typed Sensitive[String], given a plain JSON string on stdin. Bolt refuses " +
				"before running the task, so a CLI or JSON caller cannot supply a Sensitive-typed value. The task metadata must " +
				"declare the parameter as String with sensitive: true instead (see sensitive-param-canary). The error text is " +
				"checked for the canary like every other output.",
		},
		{
			scenario: "sensitive-param-canary",
			opts: boltRunOpts{
				params:   `{"token": "` + canary + `"}`,
				extraEnv: []string{"STAGEHAND_APPROVER_TOKEN_CANARY=" + approverCanary},
			},
			args:   []string{"task", "run", "harness_fixtures::secret_param", "--targets", "localhost"},
			expect: []string{`"token_length"`},
			notes: "A Sensitive[String] parameter carrying a canary on stdin, plus an approver-token-shaped canary in the exec " +
				"environment. canary_found_in lists where either canary appeared (stdout, stderr or bolt-debug.log); an empty " +
				"list means neither leaked. Neither value is stored here.",
			annotate: func(t *testing.T, m map[string]any, res execResult, extra map[string]any) {
				if v := firstItemValue(m); v != nil {
					extra["task_saw_token_length_matches"] = v["token_length"] == float64(len(canary))
				}
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.scenario, func(t *testing.T) {
			argv, res := boltRun(t, c.opts, c.args...)
			places := canaryPlaces(t, res)
			m := parseBolt(t, res.Stdout)
			extra := boltBase(t, res)
			if c.annotate != nil && m != nil {
				c.annotate(t, m, res, extra)
			}
			extra["canary_found_in"] = anySlice(places)
			if len(places) > 0 {
				// A leak is a finding for Phase 14's BOLT-07, not something to
				// normalise away: the fixture is not written with the value, only
				// the place, and the run fails so the leak is named.
				t.Errorf("a canary leaked in scenario %s: %v", c.scenario, places)
			}
			recordBolt(t, Fixture{
				Scenario:    c.scenario,
				Argv:        argv,
				ExitCode:    res.ExitCode,
				Stdout:      res.Stdout,
				Stderr:      res.Stderr,
				ExpectLines: c.expect,
				Extra:       extra,
				Notes:       c.notes,
			})
		})
	}
}
