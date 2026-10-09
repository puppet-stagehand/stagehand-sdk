//go:build harness

// The Puppet Server environment-cache flush, recorded against a real Puppet
// Server 9 (FND-05, D-11).
//
// This is the call Phase 18 will make after a deploy: a DELETE on
// /puppet-admin-api/v1/environment-cache, sent through the puppetlabs-http_request
// Bolt task on the runner, authenticated with the runner's client certificate.
// Every scenario runs `bolt task run http_request --targets localhost` as the
// non-root deploy user, with the task parameters on stdin (`--params -`).
//
// The certificates live in the shared certs volume (read-only in the runner).
// Fixtures record certificate paths only, never key material (T-13-34). The
// fixture directory and tool_version come from PUPPETSERVER_VERSION in
// images.lock, so a server upgrade cannot silently reuse old evidence.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// serverService is the compose service name of the Puppet Server 9 container.
	serverService = "puppetserver"

	// flushBaseURL is where the runner reaches the server: the compose hostname
	// "puppet" on the harness network. It is not the registry host.
	flushBaseURL = "https://puppet:8140/"

	// flushPath is the environment-cache admin endpoint, without a query string.
	flushPath = "puppet-admin-api/v1/environment-cache"

	certsDir = "/certs"

	// certsReadyTimeout bounds the wait for the server entrypoint to finish
	// issuing the runner and other certificates. The server reports healthy
	// before the certificates exist, so the tests wait for the files.
	certsReadyTimeout = 3 * time.Minute

	// expectedBoltVersion and expectedHTTPRequestVersion are what the plan
	// records; the test fails if the runner reports anything else.
	expectedBoltVersion        = "4.0.0"
	expectedHTTPRequestVersion = "0.3.1"
)

// requireServer fails (never skips) when the Puppet Server 9 image is not part of
// this run, then waits for the certificates the server entrypoint issues.
func requireServer(t *testing.T) {
	t.Helper()
	if os.Getenv("HARNESS_SERVER") != "1" {
		t.Fatalf("Puppet Server 9 image not available: complete plan 13-07's human build and push (see docs/harness.md)")
	}
	requireRunner(t)
	waitForCerts(t)
}

// waitForCerts polls the runner until the CA certificate and both client
// certificates and keys exist in the certs volume.
func waitForCerts(t *testing.T) {
	t.Helper()
	files := []string{"ca.pem", "runner.test.pem", "runner.test.key", "other.test.pem", "other.test.key"}
	deadline := time.Now().Add(certsReadyTimeout)
	for {
		missing := []string{}
		for _, f := range files {
			res := composeExec(context.Background(), t, deployUser, "test", "-s", certsDir+"/"+f)
			if res.ExitCode != 0 {
				missing = append(missing, f)
			}
		}
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server did not publish these files in %s within %s: %v", certsDir, certsReadyTimeout, missing)
		}
		time.Sleep(2 * time.Second)
	}
}

// serverVersion is the PUPPETSERVER_VERSION the image pipeline recorded in
// images.lock. It names the fixture directory and is the fixture's tool_version.
func serverVersion(t *testing.T) string {
	t.Helper()
	lock, err := readKeyValues("images.lock")
	if err != nil {
		t.Fatalf("cannot read images.lock: %v", err)
	}
	v := lock["PUPPETSERVER_VERSION"]
	if v == "" || !strings.HasPrefix(v, "9") {
		t.Fatalf("images.lock has PUPPETSERVER_VERSION=%q: want a Puppet Server 9 version (plan 13-07 Task 3)", v)
	}
	return v
}

// flush runs the http_request task through Bolt with the base flush parameters
// (DELETE, the server's base URL, the CA certificate) merged with params. A
// parameter set to nil removes the base value, so a scenario can leave one out.
// It returns the Bolt argv, the full parameter map that went to stdin, and the
// result.
func flush(t *testing.T, params map[string]any) ([]string, map[string]any, execResult) {
	t.Helper()
	all := map[string]any{
		"base_url": flushBaseURL,
		"method":   "delete",
		"cacert":   certsDir + "/ca.pem",
	}
	for k, v := range params {
		if v == nil {
			delete(all, k)
			continue
		}
		all[k] = v
	}
	raw, err := json.Marshal(all)
	if err != nil {
		t.Fatalf("cannot encode the task parameters: %v", err)
	}
	argv, res := boltRun(t, boltRunOpts{params: string(raw)}, "task", "run", "http_request", "--targets", "localhost")
	return argv, all, res
}

// versions checks the runner has the Bolt and http_request versions this plan
// records, and returns them.
func flushToolVersions(t *testing.T) (bolt, httpRequest string) {
	t.Helper()
	bolt = toolVersion(t, "bolt", "--version")
	if bolt != expectedBoltVersion {
		t.Fatalf("the runner has Bolt %s: this plan records %s", bolt, expectedBoltVersion)
	}
	res := composeExec(context.Background(), t, deployUser, "sh", "-c",
		`sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$1/metadata.json" | head -n 1`,
		"sh", boltProject+"/.modules/http_request")
	httpRequest = strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || httpRequest != expectedHTTPRequestVersion {
		t.Fatalf("the runner has puppetlabs-http_request %q (exit %d): this plan records %s",
			httpRequest, res.ExitCode, expectedHTTPRequestVersion)
	}
	return bolt, httpRequest
}

// httpResult is what the first Bolt item says about the HTTP call.
type httpResult struct {
	itemStatus string
	statusCode any // float64 from JSON, or nil when the task did not get a response
	body       any
	errorKind  string
	errorMsg   string
}

func readHTTPResult(m map[string]any) httpResult {
	var r httpResult
	if m == nil {
		return r
	}
	items, _ := m["items"].([]any)
	if len(items) == 0 {
		return r
	}
	item, _ := items[0].(map[string]any)
	r.itemStatus = asString(item["status"])
	v, _ := item["value"].(map[string]any)
	if v == nil {
		return r
	}
	r.statusCode = v["status_code"]
	if b, ok := v["body"].(string); ok {
		r.body = normalise(b)
	} else {
		r.body = v["body"]
	}
	if e, ok := v["_error"].(map[string]any); ok {
		r.errorKind = asString(e["kind"])
		r.errorMsg = normalise(asString(e["msg"]))
	}
	return r
}

// flushCase is one flush call and the notes recorded with it.
type flushCase struct {
	scenario string
	params   map[string]any
	expect   []string
	notes    string
}

// runFlush performs the call, checks no canary or key material escaped, and
// records or compares the fixture.
func runFlush(t *testing.T, c flushCase) {
	t.Helper()
	argv, params, res := flush(t, c.params)

	if places := canaryPlaces(t, res); len(places) > 0 {
		t.Fatalf("a canary leaked in scenario %s: %v", c.scenario, places)
	}
	assertNoCanary(t, res.Stdout, res.Stderr)
	if strings.Contains(res.Stdout+res.Stderr, "PRIVATE KEY") {
		t.Fatalf("scenario %s: Bolt output contains private key material", c.scenario)
	}

	m := parseBolt(t, res.Stdout)
	extra := boltBase(t, res)
	hr := readHTTPResult(m)
	boltVersion, httpRequestVersion := flushToolVersions(t)

	pathPart, _ := params["path"].(string)
	request := map[string]any{
		"method": params["method"],
		"url":    fmt.Sprint(params["base_url"]) + pathPart,
	}
	for _, k := range []string{"cacert", "cert", "key"} {
		if v, ok := params[k]; ok {
			request[k] = v
		}
	}
	extra["request"] = request
	extra["item_status"] = hr.itemStatus
	extra["status_code"] = hr.statusCode
	extra["body"] = hr.body
	if hr.errorKind != "" {
		extra["error_kind"] = hr.errorKind
		extra["error_msg"] = hr.errorMsg
	}
	extra["bolt_version"] = boltVersion
	extra["http_request_version"] = httpRequestVersion
	extra["canary_found_in"] = anySlice([]string{})

	recordFlush(t, Fixture{
		Scenario:    c.scenario,
		Argv:        argv,
		RunAs:       deployUser,
		ExitCode:    res.ExitCode,
		Stdout:      scrubBoltStdout(res.Stdout),
		Stderr:      res.Stderr,
		ExpectLines: c.expect,
		Extra:       extra,
		Notes:       c.notes,
	})
}

// recordFlush stamps the Puppet Server identity on a fixture and records or
// compares it. In compare mode it also requires every recorded Extra key to
// match exactly and refuses Extra keys the recording does not have.
func recordFlush(t *testing.T, fx Fixture) {
	t.Helper()
	fx.Tool = "puppetserver"
	fx.ToolVersion = serverVersion(t)
	fx.RecordedOnPlatform = recordedPlatform(t)
	if strings.Contains(fx.Notes+fx.Stdout+fx.Stderr, "PRIVATE KEY") {
		t.Fatalf("scenario %s: a fixture would carry private key material", fx.Scenario)
	}

	recordOrCompare(t, fx)
	if os.Getenv("HARNESS_RECORD") == "1" {
		return
	}
	compareExtra(t, fixturePath(fx.Tool, fx.ToolVersion, fx.Scenario), fx.Extra)
}

// compareExtra checks the observed Extra against the recorded fixture, naming
// the field that differs.
func compareExtra(t *testing.T, path string, observed map[string]any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	var want Fixture
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("fixture %s is not valid JSON: %v", path, err)
	}
	raw, err := json.Marshal(observed)
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

// runnerCert are the parameters that make the call as runner.test, the one
// certificate the harness auth rule allows to flush the cache.
func runnerCert(path string) map[string]any {
	return map[string]any{
		"path": path,
		"cert": certsDir + "/runner.test.pem",
		"key":  certsDir + "/runner.test.key",
	}
}

// TestHarnessFlushWithRunnerCert is the call Phase 18 will make: flush one
// environment's cache with the runner's certificate, through Bolt http_request.
func TestHarnessFlushWithRunnerCert(t *testing.T) {
	requireServer(t)

	runFlush(t, flushCase{
		scenario: "flush-with-cert",
		params:   runnerCert(flushPath + "?environment=production"),
		expect:   []string{`"status":"success"`, `"status_code":204`},
		notes: "Real Puppet Server 9 admin API. DELETE /puppet-admin-api/v1/environment-cache?environment=production sent by " +
			"bolt task run http_request on the runner with the runner.test client certificate (read from the shared certs volume; " +
			"only the paths are recorded). The harness auth rule allows runner.test only. Parameters travelled on stdin. " +
			"extra.status_code is the HTTP status the server answered; extra.item_status is Bolt's own per-target status.",
	})
}

// TestHarnessFlushFailureShapes records every way the flush call can go wrong
// that Phase 18 has to type: no certificate, a valid certificate that is not on
// the allow list, a certificate path that does not exist, an environment name
// the server does not know, and no environment parameter at all.
func TestHarnessFlushFailureShapes(t *testing.T) {
	requireServer(t)

	cases := []flushCase{
		{
			scenario: "flush-no-cert",
			params: map[string]any{
				"path": flushPath + "?environment=production",
			},
			notes: "The same call with no client certificate and no key (only the CA certificate to trust the server). " +
				"The research baseline on Puppet Server 8.7.0 was a Bolt success carrying HTTP 403; read extra.item_status, " +
				"extra.status_code and exit_code for what Puppet Server 9 does.",
		},
		{
			scenario: "flush-other-cert",
			params: map[string]any{
				"path": flushPath + "?environment=production",
				"cert": certsDir + "/other.test.pem",
				"key":  certsDir + "/other.test.key",
			},
			notes: "A valid client certificate (other.test, signed by the same CA) that the harness auth rule does not list. " +
				"This is the operator allow-list precondition made visible: authenticated is not the same as allowed.",
		},
		{
			scenario: "flush-bad-cert-path",
			params: map[string]any{
				"path": flushPath + "?environment=production",
				"cert": certsDir + "/missing.pem",
				"key":  certsDir + "/runner.test.key",
			},
			notes: "The certificate path does not exist inside the runner. The task cannot build the request, so the question is " +
				"whether Bolt reports a failure (and with which error kind) rather than a status code.",
		},
		{
			scenario: "flush-unknown-environment",
			params:   runnerCert(flushPath + "?environment=does_not_exist"),
			notes: "The runner certificate flushing an environment name the server does not have. The research baseline was that " +
				"the server does not validate the name (204), so the SDK has to validate environment names itself.",
		},
		{
			scenario: "flush-no-environment-param",
			params:   runnerCert(flushPath),
			notes: "The runner certificate and no environment query parameter. NOTE: this flushes the cache of EVERY environment on " +
				"the server. Phase 18 must always send an environment name.",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.scenario, func(t *testing.T) { runFlush(t, c) })
	}

	t.Run("package-default-auth-rule", recordPackageDefaultAuthRule)
}

const (
	packageDefaultAuth = "/etc/stagehand-harness/auth.conf.package-default"
	liveAuth           = "/etc/puppetlabs/puppetserver/conf.d/auth.conf"
	serverVersionFile  = "/etc/stagehand-harness/puppetserver-version"
)

var reAllow = regexp.MustCompile(`^\s*allow(?:-unauthenticated)?\s*:\s*(.+?)\s*,?\s*$`)

// grepCount runs `grep -c needle path` in the server container as root and
// returns the count. grep exits 1 when nothing matched, which is a zero count
// rather than an error; any other failure ends the test.
func grepCount(t *testing.T, needle, path string) (int, execResult, []string) {
	t.Helper()
	argv := []string{"grep", "-c", needle, path}
	res := composeExecService(context.Background(), t, serverService, "root", argv...)
	if res.ExitCode != 0 && res.ExitCode != 1 {
		t.Fatalf("grep in the server container failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	n, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if err != nil {
		t.Fatalf("grep -c printed %q, not a count", res.Stdout)
	}
	return n, res, argv
}

// recordPackageDefaultAuthRule records whether Puppet Server 9's own auth.conf,
// as the package ships it, already lets any certificate delete the environment
// cache. The image keeps that file untouched next to the harness copy, so this
// is evidence about the package and not about the harness rule.
func recordPackageDefaultAuthRule(t *testing.T) {
	ctx := context.Background()

	count, res, argv := grepCount(t, "environment-cache", packageDefaultAuth)
	adminCount, _, _ := grepCount(t, "puppet-admin-api", packageDefaultAuth)
	liveCount, _, _ := grepCount(t, "environment-cache", liveAuth)
	harnessRule, _, _ := grepCount(t, "stagehand-harness env-cache flush", liveAuth)
	packageHarnessRule, _, _ := grepCount(t, "stagehand-harness env-cache flush", packageDefaultAuth)

	extra := map[string]any{
		"package_default_has_env_cache_rule":       count > 0,
		"package_default_env_cache_rule_count":     count,
		"package_default_has_admin_api_rule":       adminCount > 0,
		"live_auth_conf_env_cache_rule_count":      liveCount,
		"live_auth_conf_has_harness_rule":          harnessRule > 0,
		"package_default_has_harness_rule":         packageHarnessRule > 0,
		"package_default_auth_conf_path":           packageDefaultAuth,
		"live_auth_conf_path":                      liveAuth,
		"harness_rule_is_the_only_flush_allowance": count == 0 && liveCount > 0 && harnessRule > 0,
	}
	if count > 0 {
		// The rule exists in the package: record the allow lines near it.
		ctxRes := composeExecService(ctx, t, serverService, "root", "grep", "-A", "12", "environment-cache", packageDefaultAuth)
		allows := []any{}
		for _, line := range strings.Split(ctxRes.Stdout, "\n") {
			if m := reAllow.FindStringSubmatch(line); m != nil {
				allows = append(allows, m[1])
			}
		}
		extra["package_default_env_cache_allow"] = allows
	}

	versionRes := composeExecService(ctx, t, serverService, "root", "cat", serverVersionFile)
	if versionRes.ExitCode != 0 {
		t.Fatalf("cannot read %s in the server container (exit %d)", serverVersionFile, versionRes.ExitCode)
	}
	versionLines := []any{}
	for _, l := range strings.Split(strings.TrimSpace(versionRes.Stdout), "\n") {
		versionLines = append(versionLines, strings.TrimSpace(l))
	}
	want := serverVersion(t)
	found := false
	for _, l := range versionLines {
		if strings.Contains(l.(string), want) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the running server image reports %v, not the %s pinned in images.lock", versionLines, want)
	}
	extra["server_version_file"] = versionLines

	archRes := composeExecService(ctx, t, serverService, "root", "uname", "-m")
	serverPlatform := "linux/amd64"
	if m := strings.TrimSpace(archRes.Stdout); m == "aarch64" || m == "arm64" {
		serverPlatform = "linux/arm64"
	}

	if strings.Contains(res.Stdout+res.Stderr, "PRIVATE KEY") {
		t.Fatalf("the grep output carries private key material")
	}

	fx := Fixture{
		Tool:               "puppetserver",
		ToolVersion:        want,
		Scenario:           "package-default-auth-rule",
		RecordedOnPlatform: serverPlatform,
		Argv:               append([]string{"docker", "compose", "exec", "-u", "root", serverService}, argv...),
		RunAs:              "root@" + serverService,
		ExitCode:           res.ExitCode,
		Stdout:             res.Stdout,
		Stderr:             res.Stderr,
		Extra:              extra,
		Notes: "Read from the running Puppet Server 9 container, not from the runner. " + packageDefaultAuth +
			" is the auth.conf exactly as the Puppet Server package ships it; the image build copies it aside before inserting the " +
			"harness rule. exit_code is grep's: 1 means no match. package_default_has_env_cache_rule false means Puppet Server 9 " +
			"grants nobody the environment-cache DELETE by default, so a real deployment needs an operator-added allow rule " +
			"(Phase 18 documents this precondition).",
	}
	recordOrCompare(t, fx)
	if os.Getenv("HARNESS_RECORD") != "1" {
		compareExtra(t, fixturePath(fx.Tool, fx.ToolVersion, fx.Scenario), fx.Extra)
	}
}
