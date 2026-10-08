//go:build harness

// Real r10k and g10k behaviour, recorded as version-pinned fixtures (FND-05,
// D-11). Every scenario resets its own tool directory and the git fixture,
// runs the tool as the non-root deploy user unless a scenario says otherwise,
// and records or compares one fixture under harness/fixtures/<tool>-<version>/.
//
// The fixtures keep what the tools actually did. When an observation disagrees
// with the research spike (13-RESEARCH.md, S1-S14) the fixture keeps the
// observed value and the plan summary names the contradiction; no expectation
// is edited to match the research.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

const (
	treeHashScript = "/opt/harness-scripts/tree-hash.sh"
	fixtureScript  = "/opt/harness-scripts/make-git-fixture.sh"
	controlRemote  = "file:///srv/git/control.git"
	moduleRemote   = "file:///srv/git/module-fixture.git"

	// victimsDir is the only place a hostile scenario may let a tool write or
	// purge outside an environment. It sits on the /srv/work named volume, which
	// "down -v" removes, and the runner has no writable host bind mount (T-13-30).
	victimsDir = "/srv/work/victims"
)

// deployTool describes one deploy tool and where a scenario keeps its files.
type deployTool struct {
	name    string // r10k or g10k
	version string // read from the real binary
	root    string // /srv/work/<name> (or a scenario-specific sibling)
}

func (d deployTool) cfg() string   { return d.root + "/" + d.name + ".yaml" }
func (d deployTool) cache() string { return d.root + "/cache" }
func (d deployTool) envs() string  { return d.root + "/environments" }
func (d deployTool) envDir(env string) string {
	return d.envs() + "/" + env
}
func (d deployTool) markerName() string { return "." + d.name + "-deploy.json" }
func (d deployTool) markerPath(env string) string {
	return d.envDir(env) + "/" + d.markerName()
}

// deployArgv is the deploy command line. An empty env deploys every environment
// the source offers. cfgPath is a parameter so the missing-config scenario can
// point at a file that does not exist.
func (d deployTool) deployArgv(cfgPath, env string) []string {
	if d.name == "r10k" {
		argv := []string{"r10k", "deploy", "environment"}
		if env != "" {
			argv = append(argv, env)
		}
		return append(argv, "--modules", "--config", cfgPath, "-v", "info")
	}
	argv := []string{"g10k", "-config", cfgPath, "-info"}
	if env != "" {
		argv = append(argv, "-branch", env)
	}
	return argv
}

// newDeployTool builds the descriptor for a tool, reading its version from the
// real binary so a fixture records what ran, not a constant typed into the test.
func newDeployTool(t *testing.T, name string) deployTool {
	t.Helper()
	d := deployTool{name: name, root: "/srv/work/" + name}
	switch name {
	case "r10k":
		d.version = toolVersion(t, "r10k", "version")
	case "g10k":
		// "g10k  0.10.0  Build time: ..." - the version is the second field.
		res := composeExec(context.Background(), t, deployUser, "g10k", "-version")
		if res.ExitCode != 0 {
			t.Fatalf("g10k -version failed: exit %d: %s", res.ExitCode, res.Stderr)
		}
		fields := strings.Fields(strings.SplitN(strings.TrimSpace(res.Stdout+res.Stderr), "\n", 2)[0])
		if len(fields) < 2 {
			t.Fatalf("g10k -version printed no version")
		}
		d.version = fields[1]
	default:
		t.Fatalf("unknown deploy tool %q", name)
	}
	return d
}

var cachedPlatform string

// recordedPlatform is platform(t), asked once per test binary.
func recordedPlatform(t *testing.T) string {
	t.Helper()
	if cachedPlatform == "" {
		cachedPlatform = platform(t)
	}
	return cachedPlatform
}

// sh runs a fixed script in the runner with the given positional arguments. The
// variable parts travel as arguments, never spliced into the script text.
func sh(t *testing.T, user, script string, args ...string) execResult {
	t.Helper()
	argv := append([]string{"sh", "-c", script, "sh"}, args...)
	res := composeExec(context.Background(), t, user, argv...)
	assertNoCanary(t, res.Stdout, res.Stderr)
	return res
}

// mustSh is sh that fails the test on a non-zero exit.
func mustSh(t *testing.T, user, script string, args ...string) string {
	t.Helper()
	res := sh(t, user, script, args...)
	if res.ExitCode != 0 {
		t.Fatalf("runner script failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout)
}

// resetWorld removes everything a scenario may have left (as root, so a
// root-owned or read-only leftover cannot block the reset), rebuilds the git
// fixture as deploy, and returns the control repo's production HEAD.
func resetWorld(t *testing.T, dirs ...string) string {
	t.Helper()
	for _, d := range dirs {
		if !strings.HasPrefix(d, "/srv/work/") {
			t.Fatalf("resetWorld refuses to remove %q: scratch space is /srv/work only", d)
		}
	}
	args := append([]string{"/srv/git/control.git.away", victimsDir, "/srv/work/push"}, dirs...)
	mustSh(t, "root", `chmod -R u+w "$@" 2>/dev/null; rm -rf "$@"`, args...)
	res := composeExec(context.Background(), t, deployUser, "bash", fixtureScript)
	assertNoCanary(t, res.Stdout, res.Stderr)
	if res.ExitCode != 0 {
		t.Fatalf("make-git-fixture.sh failed: exit %d: %s", res.ExitCode, res.Stderr)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	head := strings.TrimSpace(lines[len(lines)-1])
	if !reSHA.MatchString(head) {
		t.Fatalf("make-git-fixture.sh did not end with a commit SHA: %q", head)
	}
	return head
}

// writeConfig writes the tool's configuration for the scenario root.
func writeConfig(t *testing.T, d deployTool) {
	t.Helper()
	if d.name == "r10k" {
		writeR10kConfig(t, d)
		return
	}
	writeG10kConfig(t, d)
}

// writeG10kConfig writes /srv/work/g10k/g10k.yaml: cachedir, one source named
// control pointing at the local file:// control repo, and the basedir.
func writeG10kConfig(t *testing.T, d deployTool) {
	t.Helper()
	cfg := "---\ncachedir: " + d.cache() + "\nsources:\n  control:\n" +
		"    remote: " + controlRemote + "\n    basedir: " + d.envs() + "\n"
	writeInRunner(t, d.cfg(), cfg)
}

// writeR10kConfig writes /srv/work/r10k/r10k.yaml with the same shape.
func writeR10kConfig(t *testing.T, d deployTool) {
	t.Helper()
	cfg := "---\ncachedir: " + d.cache() + "\nsources:\n  control:\n" +
		"    remote: " + controlRemote + "\n    basedir: " + d.envs() + "\n"
	writeInRunner(t, d.cfg(), cfg)
}

// pushControlCommit commits one file change to the production branch of the
// bare control repo, with fixed author and dates, and returns the new HEAD.
func pushControlCommit(t *testing.T, path, content string) string {
	t.Helper()
	return pushControlCommitOn(t, "production", "production", path, content)
}

// pushControlCommitOn commits a file change to branch, creating it from base
// when it does not exist yet, and returns the new HEAD.
func pushControlCommitOn(t *testing.T, branch, base, path, content string) string {
	t.Helper()
	const script = `set -eu
export GIT_AUTHOR_NAME="Stagehand Harness" GIT_AUTHOR_EMAIL="harness@example.invalid"
export GIT_COMMITTER_NAME="Stagehand Harness" GIT_COMMITTER_EMAIL="harness@example.invalid"
export GIT_AUTHOR_DATE="2026-01-02T00:00:00+0000" GIT_COMMITTER_DATE="2026-01-02T00:00:00+0000"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
rm -rf /srv/work/push
git clone -q --branch "$2" file:///srv/git/control.git /srv/work/push
cd /srv/work/push
git checkout -q -B "$1"
printf '%s' "$4" > "$3"
git add -A
git commit -q -m "harness change to $3"
git push -q origin "$1"
git rev-parse HEAD
cd /
rm -rf /srv/work/push`
	head := mustSh(t, deployUser, script, branch, base, path, content)
	if !reSHA.MatchString(head) {
		t.Fatalf("pushControlCommit did not return a commit SHA: %q", head)
	}
	return head
}

// runTool runs one deploy-tool command line and checks nothing escaped.
func runTool(t *testing.T, user string, argv []string) execResult {
	t.Helper()
	res := composeExec(context.Background(), t, user, argv...)
	assertNoCanary(t, res.Stdout, res.Stderr)
	return res
}

// snapshot is a content view of one environment directory at one moment.
type snapshot struct {
	inclHash  string            // tree-hash.sh, markers included
	exclHash  string            // tree-hash.sh --exclude-markers
	files     map[string]string // path -> sha256, .git excluded
	markerRaw string            // the marker file as written; "" when absent
	marker    map[string]any
}

// takeSnapshot hashes dir and reads the tool's marker. A missing directory gives
// empty hashes, not a failure: several scenarios assert that nothing was made.
func takeSnapshot(t *testing.T, d deployTool, env string) snapshot {
	t.Helper()
	dir := d.envDir(env)
	s := snapshot{files: map[string]string{}}
	if sh(t, deployUser, `test -d "$1"`, dir).ExitCode != 0 {
		return s
	}
	s.inclHash = mustSh(t, deployUser, treeHashScript+` "$1"`, dir)
	s.exclHash = mustSh(t, deployUser, treeHashScript+` "$1" --exclude-markers`, dir)
	listing := mustSh(t, deployUser, `cd "$1" && find . -type f ! -path '*/.git/*' -exec sha256sum {} +`, dir)
	for _, line := range strings.Split(listing, "\n") {
		if hash, path, ok := strings.Cut(line, "  "); ok {
			s.files[path] = hash
		}
	}
	if res := sh(t, deployUser, `cat "$1"`, d.markerPath(env)); res.ExitCode == 0 {
		s.markerRaw = res.Stdout
		if err := json.Unmarshal([]byte(res.Stdout), &s.marker); err != nil {
			t.Fatalf("%s is not valid JSON: %v", d.markerName(), err)
		}
	}
	return s
}

// changedFiles lists the files whose content differs between two snapshots,
// including files added or removed and the marker itself.
func changedFiles(before, after snapshot) []string {
	seen := map[string]bool{}
	var out []string
	for p, h := range after.files {
		if before.files[p] != h {
			seen[p] = true
		}
	}
	for p := range before.files {
		if _, ok := after.files[p]; !ok {
			seen[p] = true
		}
	}
	for p := range seen {
		out = append(out, strings.TrimPrefix(p, "./"))
	}
	sort.Strings(out)
	return out
}

// changedMarkerFields lists the marker keys whose raw value differs.
func changedMarkerFields(before, after map[string]any) []string {
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	var out []string
	for k := range keys {
		if fmt.Sprint(before[k]) != fmt.Sprint(after[k]) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// markerString returns marker[key] as a string, or "" when absent.
func markerString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// markerBool returns marker[key] as a bool, or false when absent.
func markerBool(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

// jsonCanon renders a value as canonical JSON (sorted map keys) after a round
// trip, so recorded and observed values compare regardless of Go number types.
func jsonCanon(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("cannot encode value: %v", err)
	}
	var back any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("cannot decode value: %v", err)
	}
	out, err := json.Marshal(back)
	if err != nil {
		t.Fatalf("cannot encode value: %v", err)
	}
	return string(out)
}

// record writes or compares fx through the shared recordOrCompare, and in
// compare mode also holds the scenario's own evidence to the recording: every
// extra key, files_changed and the markerless tree hashes must equal what was
// recorded. The marker-inclusive hashes carry timestamps and so differ on every
// run; they are stored as evidence and the booleans derived from them in extra
// are what is compared.
func record(t *testing.T, fx Fixture) {
	t.Helper()
	fx.RecordedOnPlatform = recordedPlatform(t)
	assertNoCanary(t, fx.Stdout, fx.Stderr)
	recordOrCompare(t, fx)
	if os.Getenv("HARNESS_RECORD") == "1" {
		return
	}
	path := fixturePath(fx.Tool, fx.ToolVersion, fx.Scenario)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read recorded fixture %s: %v", path, err)
	}
	var want Fixture
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("fixture %s is not valid JSON: %v", path, err)
	}
	if a, b := jsonCanon(t, want.Extra), jsonCanon(t, fx.Extra); a != b {
		for k := range want.Extra {
			if jsonCanon(t, want.Extra[k]) != jsonCanon(t, fx.Extra[k]) {
				t.Errorf("fixture %s: field extra.%s: recorded %s, observed %s",
					path, k, jsonCanon(t, want.Extra[k]), jsonCanon(t, fx.Extra[k]))
			}
		}
		for k := range fx.Extra {
			if _, ok := want.Extra[k]; !ok {
				t.Errorf("fixture %s: field extra.%s: observed %s, not recorded", path, k, jsonCanon(t, fx.Extra[k]))
			}
		}
	}
	if a, b := strings.Join(want.FilesChanged, ","), strings.Join(fx.FilesChanged, ","); a != b {
		t.Errorf("fixture %s: field files_changed: recorded [%s], observed [%s]", path, a, b)
	}
	if want.TreeHashExclMarkersBefore != fx.TreeHashExclMarkersBefore {
		t.Errorf("fixture %s: field tree_hash_excl_markers_before: recorded %q, observed %q",
			path, want.TreeHashExclMarkersBefore, fx.TreeHashExclMarkersBefore)
	}
	if want.TreeHashExclMarkersAfter != fx.TreeHashExclMarkersAfter {
		t.Errorf("fixture %s: field tree_hash_excl_markers_after: recorded %q, observed %q",
			path, want.TreeHashExclMarkersAfter, fx.TreeHashExclMarkersAfter)
	}
	if want.RecordedOnPlatform != fx.RecordedOnPlatform {
		t.Logf("fixture %s was recorded on %s and is compared on %s", path, want.RecordedOnPlatform, fx.RecordedOnPlatform)
	}
}

// baseFixture fills the fields every deploy-tool fixture shares.
func baseFixture(d deployTool, scenario, user string, argv []string, res execResult) Fixture {
	return Fixture{
		Tool:        d.name,
		ToolVersion: d.version,
		Scenario:    scenario,
		Argv:        argv,
		RunAs:       user,
		ExitCode:    res.ExitCode,
		Stdout:      res.Stdout,
		Stderr:      res.Stderr,
	}
}

// markerKeysOf returns the sorted key set of a marker, nil when absent.
func markerKeysOf(m map[string]any) []string {
	if m == nil {
		return nil
	}
	return sortedKeys(m)
}

// ---- Task 1: is g10k -dryrun read-only? ----

// TestHarnessG10kDryRunIsNotReadOnly answers D-11's question by content hash:
// deploy production with g10k, run -dryrun against an in-sync tree, push a
// control commit and run -dryrun against an out-of-sync tree, then run it again.
// Each step records the live environment's tree hash and marker before and
// after, with and without the deploy marker.
func TestHarnessG10kDryRunIsNotReadOnly(t *testing.T) {
	requireRunner(t)
	d := newDeployTool(t, "g10k")
	head := resetWorld(t, d.root)
	writeConfig(t, d)

	// 1. A real deploy of production, as deploy.
	deployArgv := d.deployArgv(d.cfg(), "production")
	before := takeSnapshot(t, d, "production")
	res := runTool(t, deployUser, deployArgv)
	if res.ExitCode != 0 {
		t.Fatalf("g10k deploy exited %d: %s", res.ExitCode, res.Stderr)
	}
	deployed := takeSnapshot(t, d, "production")
	if deployed.marker == nil {
		t.Fatalf("no %s after a successful deploy", d.markerName())
	}
	if markerString(deployed.marker, "signature") != head {
		t.Fatalf("marker signature does not equal the control repo HEAD: field signature")
	}
	fx := baseFixture(d, "deploy-success", deployUser, deployArgv, res)
	fx.ExpectLines = []string{"Need to sync", "Synced"}
	fx.MarkerKeys = markerKeysOf(deployed.marker)
	fx.MarkerBefore = nil
	fx.MarkerAfter = deployed.marker
	fx.TreeHashBefore, fx.TreeHashAfter = before.inclHash, deployed.inclHash
	fx.TreeHashExclMarkersBefore, fx.TreeHashExclMarkersAfter = before.exclHash, deployed.exclHash
	fx.FilesChanged = changedFiles(before, deployed)
	fx.Extra = map[string]any{
		"deploy_success":           markerBool(deployed.marker, "deploy_success"),
		"signature_is_remote_head": markerString(deployed.marker, "signature") == head,
	}
	fx.Notes = "Real g10k deploy of production from a local file:// control repo, run as deploy; the baseline the dry-run fixtures are measured against."
	record(t, fx)

	// dryRun runs -dryrun and builds the evidence fixture for one step.
	dryRun := func(scenario, note string, remoteHead string, remoteConf string) snapshot {
		t.Helper()
		argv := append(append([]string{}, deployArgv...), "-dryrun")
		pre := takeSnapshot(t, d, "production")
		out := runTool(t, deployUser, argv)
		post := takeSnapshot(t, d, "production")

		dfx := baseFixture(d, scenario, deployUser, argv, out)
		dfx.MarkerKeys = markerKeysOf(post.marker)
		dfx.MarkerBefore, dfx.MarkerAfter = pre.marker, post.marker
		dfx.TreeHashBefore, dfx.TreeHashAfter = pre.inclHash, post.inclHash
		dfx.TreeHashExclMarkersBefore, dfx.TreeHashExclMarkersAfter = pre.exclHash, post.exclHash
		dfx.FilesChanged = changedFiles(pre, post)
		envConf := mustSh(t, deployUser, `cat "$1"`, d.envDir("production")+"/environment.conf")
		dfx.Extra = map[string]any{
			// tree_changed: the working files differ (markers excluded).
			"tree_changed": pre.exclHash != post.exclHash,
			// tree_changed_incl_markers: anything differs, marker rewrite included.
			"tree_changed_incl_markers":              pre.inclHash != post.inclHash,
			"marker_changed":                         pre.markerRaw != post.markerRaw,
			"marker_changed_fields":                  changedMarkerFields(pre.marker, post.marker),
			"marker_deploy_success":                  markerBool(post.marker, "deploy_success"),
			"marker_signature_is_remote_head_before": markerString(pre.marker, "signature") == remoteHead,
			"marker_signature_is_remote_head_after":  markerString(post.marker, "signature") == remoteHead,
			"live_environment_conf_is_remote":        envConf == strings.TrimSpace(remoteConf),
		}
		dfx.ExpectLines = []string{"Synced"}
		dfx.Notes = note
		record(t, dfx)
		return post
	}

	baseConf := mustSh(t, deployUser, `cat "$1"`, d.envDir("production")+"/environment.conf")

	// 2. -dryrun against a tree that is already in sync.
	dryRun("dryrun-in-sync",
		"g10k -dryrun against an environment already at the remote HEAD. tree_changed covers the working files, tree_changed_incl_markers adds the deploy marker.",
		head, baseConf)

	// 3. A new control commit, then -dryrun: the out-of-sync case.
	newConf := "modulepath = site:modules:$basemodulepath\n"
	newHead := pushControlCommit(t, "environment.conf", newConf)
	dryRun("dryrun-out-of-sync",
		"g10k -dryrun after a new commit was pushed to the control repo (environment.conf changed). Compare tree_changed, marker_changed and the marker signature with the remote HEAD to see whether the dry run mutated the live environment.",
		newHead, newConf)

	// 4. -dryrun again: does the first dry run change what the second one sees?
	dryRun("dryrun-second-run",
		"g10k -dryrun run a second time, immediately after dryrun-out-of-sync, with no new commit.",
		newHead, newConf)
}

// ---- Task 2: failure modes, drift, hostile moduledir, root ownership ----

const (
	badRefPuppetfile = "mod 'm',\n  :git => '" + moduleRemote + "',\n  :ref => 'no-such-branch'\n"
	goodPuppetfile   = "mod 'm',\n  :git => '" + moduleRemote + "',\n  :ref => 'main'\n"

	// syntaxBrokenPuppetfile is not valid Ruby, so it fails when it is read,
	// before any module is touched.
	syntaxBrokenPuppetfile = "mod 'm',\n  :git => \n  this is not ruby (((\n"
)

// freshDeploy resets the scenario, writes the tool config and deploys
// production once as deploy. It fails the test if that first deploy fails: the
// scenario under test is always the second step.
func freshDeploy(t *testing.T, d deployTool, extraDirs ...string) (head string, snap snapshot) {
	t.Helper()
	head = resetWorld(t, append([]string{d.root}, extraDirs...)...)
	writeConfig(t, d)
	res := runTool(t, deployUser, d.deployArgv(d.cfg(), "production"))
	if res.ExitCode != 0 {
		t.Fatalf("%s: the setup deploy exited %d: %s", d.name, res.ExitCode, res.Stderr+res.Stdout)
	}
	return head, takeSnapshot(t, d, "production")
}

// exists reports whether path exists inside the runner.
func exists(t *testing.T, path string) bool {
	t.Helper()
	return sh(t, deployUser, `test -e "$1"`, path).ExitCode == 0
}

// environmentIntact reports whether the files a successful deploy leaves are all
// still present: control repo files and the installed module.
func environmentIntact(t *testing.T, d deployTool, env string) bool {
	t.Helper()
	dir := d.envDir(env)
	for _, p := range []string{"Puppetfile", "environment.conf", "manifests/site.pp", "modules/m/metadata.json"} {
		if !exists(t, dir+"/"+p) {
			return false
		}
	}
	return true
}

// listDir returns the sorted entry names of a directory, nil when it is absent.
func listDir(t *testing.T, path string) []string {
	t.Helper()
	res := sh(t, deployUser, `ls -A "$1" | LC_ALL=C sort`, path)
	if res.ExitCode != 0 {
		return nil
	}
	out := strings.Fields(res.Stdout)
	if out == nil {
		return []string{}
	}
	return out
}

// stepFixture builds the fixture for a scenario step observed between two
// snapshots of one environment.
func stepFixture(d deployTool, scenario, user string, argv []string, res execResult, pre, post snapshot, extra map[string]any, expect []string, notes string) Fixture {
	fx := baseFixture(d, scenario, user, argv, res)
	marker := post.marker
	if marker == nil {
		marker = pre.marker
	}
	fx.MarkerKeys = markerKeysOf(marker)
	fx.MarkerBefore, fx.MarkerAfter = pre.marker, post.marker
	fx.TreeHashBefore, fx.TreeHashAfter = pre.inclHash, post.inclHash
	fx.TreeHashExclMarkersBefore, fx.TreeHashExclMarkersAfter = pre.exclHash, post.exclHash
	fx.FilesChanged = changedFiles(pre, post)
	fx.ExpectLines = expect
	fx.Extra = extra
	fx.Notes = notes
	return fx
}

// markerSummary is the common marker evidence added to a scenario's extra.
func markerSummary(extra map[string]any, pre, post snapshot) map[string]any {
	extra["marker_changed"] = pre.markerRaw != post.markerRaw
	extra["marker_changed_fields"] = changedMarkerFields(pre.marker, post.marker)
	extra["marker_deploy_success"] = markerBool(post.marker, "deploy_success")
	extra["tree_changed"] = pre.exclHash != post.exclHash
	return extra
}

// partialRun is what one deploy-all-with-a-failing-environment run left behind.
type partialRun struct {
	argv             []string
	res              execResult
	pre, post        snapshot // production
	failing          snapshot
	failingDir       bool
	entries          []string
	productionIntact bool
}

// deployAllPartial deploys production, pushes a branch whose Puppetfile is the
// given content, then deploys every environment.
func deployAllPartial(t *testing.T, d deployTool, branch, puppetfile string) partialRun {
	t.Helper()
	_, pre := freshDeploy(t, d)
	pushControlCommitOn(t, branch, "production", "Puppetfile", puppetfile)
	argv := d.deployArgv(d.cfg(), "")
	res := runTool(t, deployUser, argv)
	return partialRun{
		argv:             argv,
		res:              res,
		pre:              pre,
		post:             takeSnapshot(t, d, "production"),
		failing:          takeSnapshot(t, d, branch),
		failingDir:       exists(t, d.envDir(branch)),
		entries:          listDir(t, d.envs()),
		productionIntact: environmentIntact(t, d, "production"),
	}
}

// outputExpectations are the stable fragments each fixture's output must still
// contain in compare mode, keyed by tool/scenario. They are chosen from the
// recorded output; a tool upgrade that rewords an error fails here, by name.
var outputExpectations = map[string][]string{
	"r10k/unreachable-remote":         {"does not appear to be a git repository"},
	"g10k/unreachable-remote":         {"Could not resolve git repository in source"},
	"r10k/missing-config":             {"Couldn't load config file"},
	"g10k/missing-config":             {"error parsing the config file"},
	"r10k/readonly-cachedir":          {"Permission denied"},
	"g10k/readonly-cachedir":          {"is not writable"},
	"r10k/bad-module-ref":             {"Could not resolve desired ref 'no-such-branch'"},
	"g10k/bad-module-ref":             {"Failed to resolve git module 'm'"},
	"r10k/unknown-environment":        {"cannot be found in any source"},
	"g10k/unknown-environment":        {"Synced"},
	"r10k/deploy-all-partial":         {"Failed to evaluate"},
	"g10k/deploy-all-partial":         {"Trailing comma or invalid setting"},
	"r10k/hostile-moduledir-absolute": {"Removing unmanaged path"},
	"r10k/hostile-moduledir-relative": {"Removing unmanaged path"},
	"r10k/hostile-moduledir-dot":      {"Deploying module to"},
	"g10k/hostile-moduledir-relative": {"Removing unmanaged path"},
	"g10k/hostile-moduledir-absolute": {"Need to sync"},
	"r10k/tracked-dir-moduledir":      {"Removing unmanaged path"},
	"r10k/run-as-root-ownership":      {"Deploying module to"},
}

type failureScenario struct {
	name string
	run  func(t *testing.T, d deployTool) Fixture
}

var failureScenarios = []failureScenario{
	{"unreachable-remote", func(t *testing.T, d deployTool) Fixture {
		_, pre := freshDeploy(t, d)
		mustSh(t, deployUser, `mv /srv/git/control.git /srv/git/control.git.away`)
		defer sh(t, deployUser, `if [ -d /srv/git/control.git.away ] && [ ! -e /srv/git/control.git ]; then mv /srv/git/control.git.away /srv/git/control.git; fi`)
		argv := d.deployArgv(d.cfg(), "production")
		res := runTool(t, deployUser, argv)
		post := takeSnapshot(t, d, "production")
		extra := markerSummary(map[string]any{"environment_intact": environmentIntact(t, d, "production")}, pre, post)
		return stepFixture(d, "unreachable-remote", deployUser, argv, res, pre, post, extra, nil,
			"The control repo was renamed away after a first successful deploy, then the same deploy was run again.")
	}},
	{"missing-config", func(t *testing.T, d deployTool) Fixture {
		resetWorld(t, d.root)
		argv := d.deployArgv(d.root+"/does-not-exist.yaml", "production")
		res := runTool(t, deployUser, argv)
		extra := map[string]any{
			"environment_dir_created": exists(t, d.envDir("production")),
			"cachedir_created":        exists(t, d.cache()),
		}
		return stepFixture(d, "missing-config", deployUser, argv, res, snapshot{}, snapshot{}, extra, nil,
			"The config path does not exist; nothing else was set up.")
	}},
	{"readonly-cachedir", func(t *testing.T, d deployTool) Fixture {
		_, pre := freshDeploy(t, d)
		mustSh(t, deployUser, `chmod -R a-w "$1"`, d.cache())
		defer sh(t, deployUser, `chmod -R u+w "$1"`, d.cache())
		argv := d.deployArgv(d.cfg(), "production")
		res := runTool(t, deployUser, argv)
		post := takeSnapshot(t, d, "production")
		extra := markerSummary(map[string]any{
			"cachedir_writable_by_deploy": sh(t, deployUser, `test -w "$1"`, d.cache()).ExitCode == 0,
			"chmod_scope":                 "recursive a-w as deploy, never root",
			"environment_intact":          environmentIntact(t, d, "production"),
		}, pre, post)
		return stepFixture(d, "readonly-cachedir", deployUser, argv, res, pre, post, extra, nil,
			"After a first successful deploy the whole cache directory was made read-only for the deploy user (root would ignore it, Pitfall 7).")
	}},
	{"bad-module-ref", func(t *testing.T, d deployTool) Fixture {
		_, pre := freshDeploy(t, d)
		newHead := pushControlCommit(t, "Puppetfile", badRefPuppetfile)
		argv := d.deployArgv(d.cfg(), "production")
		res := runTool(t, deployUser, argv)
		post := takeSnapshot(t, d, "production")
		puppetfile := mustSh(t, deployUser, `cat "$1"`, d.envDir("production")+"/Puppetfile")
		extra := markerSummary(map[string]any{
			"checkout_moved":                  strings.Contains(puppetfile, "no-such-branch"),
			"marker_signature_is_new_commit":  markerString(post.marker, "signature") == newHead,
			"marker_signature_is_prev_commit": markerString(post.marker, "signature") == markerString(pre.marker, "signature"),
			"module_dir_kept":                 exists(t, d.envDir("production")+"/modules/m/metadata.json"),
		}, pre, post)
		return stepFixture(d, "bad-module-ref", deployUser, argv, res, pre, post, extra, nil,
			"A new control commit changed the Puppetfile so module m names a branch that does not exist.")
	}},
	{"unknown-environment", func(t *testing.T, d deployTool) Fixture {
		_, pre := freshDeploy(t, d)
		argv := d.deployArgv(d.cfg(), "nosuchenv")
		res := runTool(t, deployUser, argv)
		post := takeSnapshot(t, d, "production")
		extra := markerSummary(map[string]any{
			"environment_dir_created":       exists(t, d.envDir("nosuchenv")),
			"environments_entries":          listDir(t, d.envs()),
			"production_environment_intact": environmentIntact(t, d, "production"),
		}, pre, post)
		return stepFixture(d, "unknown-environment", deployUser, argv, res, pre, post, extra, nil,
			"Production was deployed first; the second deploy names an environment that matches no branch of the control repo. marker_* describe production.")
	}},
	{"deploy-all-partial", func(t *testing.T, d deployTool) Fixture {
		// Primary run: the failing environment ("broken") sorts before production.
		p := deployAllPartial(t, d, "broken", syntaxBrokenPuppetfile)
		extra := markerSummary(map[string]any{
			"broken_environment_dir_present":   p.failingDir,
			"broken_marker_present":            p.failing.marker != nil,
			"broken_marker_deploy_success":     markerBool(p.failing.marker, "deploy_success"),
			"environments_entries":             p.entries,
			"production_environment_intact":    p.productionIntact,
			"production_marker_deploy_success": markerBool(p.post.marker, "deploy_success"),
		}, p.pre, p.post)
		fx := stepFixture(d, "deploy-all-partial", deployUser, p.argv, p.res, p.pre, p.post, extra, nil,
			"Production deployed once, then a second branch named broken was pushed whose Puppetfile is not valid Ruby, and every environment was deployed. marker_* describe production. "+
				"The variant_* extras repeat the run with the failing environment sorting after production, and with a Puppetfile that parses but names a missing ref.")
		fx.ExitCode = p.res.ExitCode

		// Variant: the failing environment sorts after production.
		after := deployAllPartial(t, d, "zbroken", syntaxBrokenPuppetfile)
		extra["variant_failing_env_sorts_after_production"] = map[string]any{
			"exit_code":                        after.res.ExitCode,
			"failing_marker_deploy_success":    markerBool(after.failing.marker, "deploy_success"),
			"failing_marker_present":           after.failing.marker != nil,
			"production_environment_intact":    after.productionIntact,
			"production_marker_deploy_success": markerBool(after.post.marker, "deploy_success"),
			"production_marker_changed":        after.pre.markerRaw != after.post.markerRaw,
		}
		// Variant: a Puppetfile that parses but names a ref that does not exist.
		badRef := deployAllPartial(t, d, "broken", badRefPuppetfile)
		extra["variant_bad_module_ref"] = map[string]any{
			"exit_code":                        badRef.res.ExitCode,
			"failing_marker_deploy_success":    markerBool(badRef.failing.marker, "deploy_success"),
			"failing_marker_present":           badRef.failing.marker != nil,
			"production_environment_intact":    badRef.productionIntact,
			"production_marker_deploy_success": markerBool(badRef.post.marker, "deploy_success"),
			"production_marker_changed":        badRef.pre.markerRaw != badRef.post.markerRaw,
		}
		return fx
	}},
}

// TestHarnessDeployToolFailureModes records exit codes and what is left behind
// for six induced failures, under both tools. Exit codes are evidence, not the
// verdict: Phase 15 judges a deploy from markers and verification.
func TestHarnessDeployToolFailureModes(t *testing.T) {
	requireRunner(t)
	for _, name := range []string{"r10k", "g10k"} {
		d := newDeployTool(t, name)
		for _, sc := range failureScenarios {
			t.Run(name+"/"+sc.name, func(t *testing.T) {
				fx := sc.run(t, d)
				fx.ExpectLines = outputExpectations[name+"/"+sc.name]
				record(t, fx)
			})
		}
	}
}

// TestHarnessR10kDisplayDrift records r10k's drift report: `deploy display
// --fetch --format json` after a deploy (insync) and after a new commit reached
// the control repo (outdated). Exit codes are recorded, not assumed.
func TestHarnessR10kDisplayDrift(t *testing.T) {
	requireRunner(t)
	d := newDeployTool(t, "r10k")
	_, deployed := freshDeploy(t, d)

	display := func(scenario, note string) {
		t.Helper()
		argv := []string{"r10k", "deploy", "display", "--modules", "--detail", "--fetch", "--format", "json",
			"--config", d.cfg(), "production"}
		pre := takeSnapshot(t, d, "production")
		res := runTool(t, deployUser, argv)
		post := takeSnapshot(t, d, "production")

		var report struct {
			Sources []struct {
				Name         string           `json:"name"`
				Environments []map[string]any `json:"environments"`
			} `json:"sources"`
		}
		if err := json.Unmarshal([]byte(res.Stdout), &report); err != nil {
			t.Fatalf("r10k deploy display did not print JSON: %v", err)
		}
		status := map[string]any{}
		envKeys := map[string]any{}
		for _, src := range report.Sources {
			for _, env := range src.Environments {
				name, _ := env["name"].(string)
				status[name] = env["status"]
				envKeys[name] = sortedKeys(env)
			}
		}
		extra := markerSummary(map[string]any{
			"status":              status,
			"environment_keys":    envKeys,
			"display_wrote_files": pre.inclHash != post.inclHash,
		}, pre, post)
		fx := stepFixture(d, scenario, deployUser, argv, res, pre, post, extra, []string{`"status"`}, note)
		record(t, fx)
	}

	_ = deployed
	display("display-insync", "r10k deploy display --fetch right after a deploy: the environment is at the remote HEAD.")

	pushControlCommit(t, "environment.conf", "modulepath = site:modules:$basemodulepath\n")
	display("display-outdated", "r10k deploy display --fetch after a new commit reached the control repo and was not deployed.")
}

// hostileCase is one moduledir value written straight into the git fixture,
// bypassing the SDK validator on purpose: this is the evidence behind FND-02.
type hostileCase struct {
	tool      string
	scenario  string
	moduledir string
	victim    string // directory name under victimsDir, "" for the dot case
}

var hostileCases = []hostileCase{
	{"r10k", "hostile-moduledir-absolute", victimsDir + "/abs-victim", "abs-victim"},
	{"r10k", "hostile-moduledir-relative", "../../../victims/rel-victim", "rel-victim"},
	{"r10k", "hostile-moduledir-dot", ".", ""},
	{"g10k", "hostile-moduledir-relative", "../../../victims/rel-victim", "rel-victim"},
	{"g10k", "hostile-moduledir-absolute", victimsDir + "/abs-victim", "abs-victim"},
}

// TestHarnessHostileModuledir deploys a control repo whose Puppetfile carries a
// moduledir the SDK validator would refuse, with a victim directory (and for the
// dot case the environment root itself) holding an unmanaged file, and records
// whether a module landed outside the environment and whether the file was purged.
// Victims live only under /srv/work (T-13-30).
func TestHarnessHostileModuledir(t *testing.T) {
	requireRunner(t)
	for _, hc := range hostileCases {
		t.Run(hc.tool+"/"+hc.scenario, func(t *testing.T) {
			d := newDeployTool(t, hc.tool)
			resetWorld(t, d.root)
			writeConfig(t, d)
			env := d.envDir("production")
			puppetfile := "moduledir '" + hc.moduledir + "'\n\n" + goodPuppetfile

			var pre snapshot
			if hc.victim != "" {
				mustSh(t, deployUser, `mkdir -p "$1" && printf 'keep\n' > "$1/keep.txt"`, victimsDir+"/"+hc.victim)
				pushControlCommit(t, "Puppetfile", puppetfile)
			} else {
				// The dot case: the victim is the environment root, so the
				// environment exists first and holds an unmanaged file.
				if res := runTool(t, deployUser, d.deployArgv(d.cfg(), "production")); res.ExitCode != 0 {
					t.Fatalf("setup deploy exited %d", res.ExitCode)
				}
				mustSh(t, deployUser, `printf 'keep\n' > "$1/keep.txt"`, env)
				pre = takeSnapshot(t, d, "production")
				pushControlCommit(t, "Puppetfile", puppetfile)
			}

			argv := d.deployArgv(d.cfg(), "production")
			res := runTool(t, deployUser, argv)
			post := takeSnapshot(t, d, "production")

			extra := map[string]any{"moduledir": hc.moduledir}
			if hc.victim != "" {
				victim := victimsDir + "/" + hc.victim
				extra["module_installed_outside_env"] = exists(t, victim+"/m")
				extra["unmanaged_file_purged"] = !exists(t, victim+"/keep.txt")
				extra["victim_entries"] = listDir(t, victim)
				if strings.HasPrefix(hc.moduledir, "/") {
					extra["module_installed_at_env_joined_path"] = exists(t, env+hc.moduledir+"/m")
				}
			} else {
				extra["module_installed_outside_env"] = false
				extra["module_installed_in_env_root"] = exists(t, env+"/m")
				extra["unmanaged_file_purged"] = !exists(t, env+"/keep.txt")
				extra["tracked_files_survived"] = exists(t, env+"/environment.conf") && exists(t, env+"/manifests/site.pp") && exists(t, env+"/Puppetfile")
			}
			extra["environment_entries"] = listDir(t, env)
			extra["deploy_success_marker"] = markerBool(post.marker, "deploy_success")

			note := "moduledir written straight into the control repo Puppetfile, bypassing the SDK validator on purpose (FND-02 evidence). " +
				"Victims live under " + victimsDir + " only."
			fx := stepFixture(d, hc.scenario, deployUser, argv, res, pre, post, extra, nil, note)
			fx.FilesChanged = nil
			fx.ExpectLines = outputExpectations[hc.tool+"/"+hc.scenario]
			record(t, fx)
		})
	}

	t.Run("r10k/tracked-dir-moduledir", func(t *testing.T) {
		d := newDeployTool(t, "r10k")
		_, pre := freshDeploy(t, d)
		env := d.envDir("production")
		// An untracked file inside a directory the control repo also tracks.
		mustSh(t, deployUser, `printf 'node default {}\n' > "$1/manifests/local.pp"`, env)
		pre = takeSnapshot(t, d, "production")
		pushControlCommit(t, "Puppetfile", "moduledir 'manifests'\n\n"+goodPuppetfile)

		argv := d.deployArgv(d.cfg(), "production")
		res := runTool(t, deployUser, argv)
		post := takeSnapshot(t, d, "production")
		extra := markerSummary(map[string]any{
			"moduledir":                     "manifests",
			"site_pp_survived":              exists(t, env+"/manifests/site.pp"),
			"local_pp_survived":             exists(t, env+"/manifests/local.pp"),
			"module_installed_in_manifests": exists(t, env+"/manifests/m"),
			"manifests_entries":             listDir(t, env+"/manifests"),
		}, pre, post)
		fx := stepFixture(d, "tracked-dir-moduledir", deployUser, argv, res, pre, post, extra, nil,
			"moduledir 'manifests' names a directory the control repo tracks (site.pp). An untracked local.pp was placed in it before the deploy (research A10).")
		fx.ExpectLines = outputExpectations["r10k/tracked-dir-moduledir"]
		record(t, fx)
	})
}

// allowRootGitSafe lets root use the deploy-owned fixture repositories. git
// refuses a repository owned by another user unless safe.directory allows it,
// and r10k does not pass its environment on to git, so the setting goes into
// root's global git config inside the container. The returned function removes
// it again.
func allowRootGitSafe(t *testing.T) func() {
	t.Helper()
	mustSh(t, "root", `git config --global --add safe.directory '*'`)
	return func() { sh(t, "root", `git config --global --unset-all safe.directory`) }
}

// TestHarnessR10kRootOwnership records what r10k does when it is run as root
// instead of the deploy user. Three runs: root with no git setup (git refuses
// the deploy-owned repository), root with safe.directory into a fresh tree, and
// root over a tree the deploy user created earlier. The main fields describe the
// second; the others are in extra.
func TestHarnessR10kRootOwnership(t *testing.T) {
	requireRunner(t)
	base := newDeployTool(t, "r10k")
	root := base
	root.root = "/srv/work/r10k-root"

	uid := func(path string) string {
		t.Helper()
		res := sh(t, deployUser, `stat -c %u "$1"`, path)
		if res.ExitCode != 0 {
			return "absent"
		}
		return strings.TrimSpace(res.Stdout)
	}

	// 1. Root with no git setup, into a fresh tree.
	resetWorld(t, base.root, root.root)
	writeConfig(t, root)
	plain := runTool(t, "root", root.deployArgv(root.cfg(), "production"))
	extra := map[string]any{
		"plain_root_exit_code":                 plain.ExitCode,
		"plain_root_git_refused_dubious_owner": strings.Contains(plain.Stdout+plain.Stderr, "dubious ownership"),
		"plain_root_cache_dir_owner_uid":       uid(root.cache()),
		"plain_root_environment_dir_created":   exists(t, root.envDir("production")),
		"plain_root_marker_written":            exists(t, root.markerPath("production")),
	}

	// 2. Root with safe.directory, into a fresh tree.
	resetWorld(t, base.root, root.root)
	writeConfig(t, root)
	undo := allowRootGitSafe(t)
	defer undo()
	argv := root.deployArgv(root.cfg(), "production")
	res := runTool(t, "root", argv)
	post := takeSnapshot(t, root, "production")
	env := root.envDir("production")
	extra["deploy_user_uid"] = "1000"
	extra["base_dir_owner_uid"] = uid(root.root)
	extra["cache_dir_owner_uid"] = uid(root.cache())
	extra["environment_dir_owner_uid"] = uid(env)
	extra["marker_owner_uid"] = uid(root.markerPath("production"))
	extra["modules_dir_owner_uid"] = uid(env + "/modules")
	extra["puppetfile_owner_uid"] = uid(env + "/Puppetfile")
	extra["marker_deploy_success"] = markerBool(post.marker, "deploy_success")

	// 3. A tree the deploy user created, then a new commit deployed as root.
	resetWorld(t, base.root, root.root)
	writeConfig(t, base)
	if first := runTool(t, deployUser, base.deployArgv(base.cfg(), "production")); first.ExitCode != 0 {
		t.Fatalf("setup deploy as deploy exited %d", first.ExitCode)
	}
	pushControlCommit(t, "environment.conf", "modulepath = site:modules:$basemodulepath\n")
	again := runTool(t, "root", base.deployArgv(base.cfg(), "production"))
	benv := base.envDir("production")
	extra["existing_tree_exit_code"] = again.ExitCode
	extra["existing_tree_environment_dir_owner_uid"] = uid(benv)
	extra["existing_tree_environment_conf_owner_uid"] = uid(benv + "/environment.conf")
	extra["existing_tree_marker_owner_uid"] = uid(base.markerPath("production"))
	extra["existing_tree_modules_dir_owner_uid"] = uid(benv + "/modules")

	fx := stepFixture(root, "run-as-root-ownership", "root", argv, res, snapshot{}, post, extra, nil,
		"r10k run as root with git safe.directory allowed in the global git config of root, into a fresh tree under a deploy-owned base directory. "+
			"plain_root_* describe the same run without safe.directory; existing_tree_* describe a root run over a tree the deploy user created. "+
			"Uids are numbers: 0 is root, 1000 is deploy.")
	fx.ExpectLines = outputExpectations["r10k/run-as-root-ownership"]
	record(t, fx)
}
