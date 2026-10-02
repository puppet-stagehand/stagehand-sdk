package controlrepoauthoring_test

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	controlrepoauthoring "github.com/puppet-stagehand/stagehand-sdk/examples/control-repo-authoring"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
	"github.com/puppet-stagehand/stagehand-sdk/manifest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// fixtureAPIKey is the sealed LLM provider key every test host is configured
// with. Tests assert it reaches the LLM client and never a prompt.
const fixtureAPIKey = "sk-proof-11-key"

// Git credential fixture values. The token is an obvious fixture string, never
// anything that reads as a real credential.
const (
	gitCredentialName  = "control-repo-login"
	gitCredentialUser  = "svc-stagehand"
	fixtureGitToken    = "tok-proof-11"
	controlRepoURL     = "https://git.example.test/org/control-repo.git"
	canaryCommit       = "c0ffee0000000000000000000000000000000001"
	qaTwoCommit        = "c0ffee0000000000000000000000000000000002"
	featureSpikeCommit = "c0ffee0000000000000000000000000000000003"
)

// world is one test host plus the fixtures it was built over, so a test can
// read what the fixtures observed and override what they answer.
type world struct {
	h        *host.Host
	forge    *forgeFixture
	llm      *llmFixture
	git      *gitFixture
	provider string
}

// newHost loads manifest.json from this example's own directory, refuses any
// parse or validation finding, and builds one *host.Host scoped to exactly the
// permissions that manifest declares. The host's grant therefore comes from
// the manifest and nowhere else. The host runs against a registry fixture and
// an LLM fixture and an in-memory git remote, so nothing here touches the
// network or a git binary, and its LLM provider is configured the way an
// operator would configure it.
func newHost(t *testing.T) *world {
	t.Helper()
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	m, findings := manifest.Parse(raw)
	if len(findings) > 0 {
		t.Fatalf("manifest.json failed to parse: %v", findings)
	}
	if findings := manifest.Validate(m); len(findings) > 0 {
		t.Fatalf("manifest.json failed validation: %v", findings)
	}
	forge := &forgeFixture{}
	llm := &llmFixture{rankingReply: rankingReplyFor(rankedEntry{"puppetlabs/ntp", "puppet-forge", "Keeps clocks in sync across the fleet."})}
	git := newGitFixture()
	h := local.New(m.Permissions, m.ID, local.WithForgeClient(forge), local.WithLLMClient(llm), local.WithGitClient(git))
	return &world{h: h, forge: forge, llm: llm, git: git, provider: configureLLMProvider(t, h)}
}

// configureLLMProvider is operator setup, performed the way docs/forge-recommend.md
// asks a pack to do it for itself: seal the provider's config as one Secrets
// value, then write a name/label/ref-only index document into the llm-providers
// collection. It returns the provider name Recommend is called with.
//
// It lives in this test file, and nowhere in the production file, on purpose:
// the index write needs a Documents selector, which the proposing persona is
// structurally barred from carrying (see TestControlRepoAuthoring_ProposerCannotSelfApprove).
//
// The collection literal and the JSON tags below are unexported in host/local,
// so this helper is the single place they are repeated. A wrong collection
// gives a not-found provider error; a wrong tag gives a no-model-configured
// error. The secret ref is copied verbatim from the store call, never rebuilt.
func configureLLMProvider(t *testing.T, h *host.Host) string {
	t.Helper()
	const name = "primary"
	sealed, err := json.Marshal(map[string]string{
		"kind":             "anthropic",
		"base_url":         "",
		"model":            "claude-sonnet-4-5",
		"api_key":          fixtureAPIKey,
		"max_tokens_field": "max_tokens",
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := h.Secrets.Store(context.Background(), &hostv1.StoreSecretRequest{Name: "llm-provider-" + name, Plaintext: sealed})
	if err != nil {
		t.Fatalf("configureLLMProvider: Secrets.Store: %v", err)
	}
	body, err := structpb.NewStruct(map[string]any{"name": name, "label": "Primary", "secret_ref": ref.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Documents.Put(context.Background(), &hostv1.PutDocumentRequest{
		Collection: "llm-providers", DocId: name, Body: &hostv1.Json{Value: body}, IfVersion: 0,
	}); err != nil {
		t.Fatalf("configureLLMProvider: Documents.Put: %v", err)
	}
	return name
}

// forgeFixture is a three-module registry. Every value is deliberately unlike
// the real registry's, so no assertion can pass by accident.
type forgeFixture struct{}

var forgeCatalog = []*hostv1.ForgeSearchResult{
	{
		Name: "puppetlabs/ntp", Version: "13.2.1", Endorsement: "supported", QualityScore: 4.6,
		Summary: "Installs, configures and manages the NTP service for time synchronisation",
		Tags:    []string{"ntp", "time"},
	},
	{
		Name: "puppetlabs/stdlib", Version: "9.6.0", Endorsement: "supported", QualityScore: 4.8,
		Summary: "Standard library of resources for Puppet modules",
		Tags:    []string{"stdlib", "functions"},
	},
	{
		Name: "example/timekeeper", Version: "0.4.0", Deprecated: true, SupersededBy: "puppetlabs/ntp",
		Summary: "Legacy clock helper, use ntp instead",
		Tags:    []string{"legacy"},
	},
}

// Search matches an entry when any whitespace-separated term of the query is
// contained in the entry's name or summary. It stamps Source with the source
// argument it was handed: the host joins ranked entries to candidates on
// lower-cased name plus source, so an empty source would make every ranked
// entry an unknown-module drop. It never returns the not-found sentinel.
func (*forgeFixture) Search(_ context.Context, _ local.ForgeEndpoint, source, query string, _ *hostv1.Page) ([]*hostv1.ForgeSearchResult, *hostv1.PageInfo, error) {
	terms := strings.Fields(strings.ToLower(query))
	out := []*hostv1.ForgeSearchResult{}
	for _, entry := range forgeCatalog {
		hay := strings.ToLower(entry.Name + " " + entry.Summary)
		for _, term := range terms {
			if strings.Contains(hay, term) {
				c := proto.Clone(entry).(*hostv1.ForgeSearchResult)
				c.Source = source
				out = append(out, c)
				break
			}
		}
	}
	return out, &hostv1.PageInfo{}, nil
}

func (*forgeFixture) ListReleases(_ context.Context, _ local.ForgeEndpoint, name string) ([]string, error) {
	switch name {
	case "puppetlabs/ntp":
		return []string{"12.0.1", "13.0.0", "13.2.1"}, nil
	case "puppetlabs/stdlib":
		return []string{"9.4.1", "9.6.0"}, nil
	}
	return nil, local.ErrForgeNotFound
}

func (*forgeFixture) GetRelease(_ context.Context, _ local.ForgeEndpoint, name, version string) (*local.ForgeRelease, error) {
	switch {
	case name == "puppetlabs/ntp" && version == "13.2.1":
		return &local.ForgeRelease{Name: name, Version: version, Dependencies: []local.ForgeDependency{
			{Name: "puppetlabs/stdlib", VersionRequirement: ">= 9.0.0 < 10.0.0"},
		}}, nil
	case name == "puppetlabs/stdlib" && version == "9.6.0":
		return &local.ForgeRelease{Name: name, Version: version, Dependencies: []local.ForgeDependency{}}, nil
	}
	return nil, local.ErrForgeNotFound
}

// sealGitCredential is operator setup: it seals an https-token credential as one
// Secrets value and returns its NAME, which is the only thing an import request
// carries. The host reveals the sealed secret itself and builds the remote
// credential. The three JSON keys are the host's own convention for a sealed git
// credential; they are unexported in host/local, so this helper is the one place
// they are repeated. Like configureLLMProvider it lives in this test file so the
// proposing persona never needs a Secrets selector.
func sealGitCredential(t *testing.T, h *host.Host) string {
	t.Helper()
	sealed, err := json.Marshal(map[string]string{
		"kind":     "https_token",
		"username": gitCredentialUser,
		"token":    fixtureGitToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Secrets.Store(context.Background(), &hostv1.StoreSecretRequest{Name: gitCredentialName, Plaintext: sealed}); err != nil {
		t.Fatalf("sealGitCredential: Secrets.Store: %v", err)
	}
	return gitCredentialName
}

// gitFixture is an in-memory control-repo remote implementing local.GitClient.
// It is deliberately re-implemented here rather than borrowed from host/local:
// that package's fixture lives in a test file and cannot be imported, and the
// real system-git client is not exercised by this example (Phase 10 covers it,
// and the host's URL allowlist refuses a filesystem remote anyway).
//
// Every call records the remote it was handed, under a mutex because the suite
// runs under -race. Both the inspect and the propose paths fetch, so recording
// only the last call would hide a credential dropped on the first.
type gitFixture struct {
	mu      sync.Mutex
	files   map[string]map[string]string // branch -> repo-relative path -> bytes
	commits map[string]string            // branch -> fixed 40-hex SHA
	remotes []local.GitRemote
	opened  []string
}

// newGitFixture builds the three-branch remote. Every value differs from a
// Puppet default, so no assertion can pass by matching one. feature-spike is in
// the branch listing with a SHA and no content: its name cannot be an
// environment name, so the host classifies it from the listing alone and never
// opens it. Do not add other branch names.
func newGitFixture() *gitFixture {
	const hiera = "version: 5\ndefaults:\n  datadir: hieradata\nhierarchy:\n  - name: common\n    path: common.yaml\n"
	return &gitFixture{
		files: map[string]map[string]string{
			"canary": {
				"Puppetfile":            "mod 'puppetlabs-stdlib', '9.4.1'\n",
				"hiera.yaml":            hiera,
				"hieradata/common.yaml": "profile::ntp::servers: ntp-canary.example.test\n",
				"environment.conf":      "config_version = scripts/canary_config_version.sh\nenvironment_timeout = 10m\n",
			},
			"qa_two": {
				"Puppetfile":            "mod 'puppetlabs/apache', '12.3.0'\n",
				"hiera.yaml":            hiera,
				"hieradata/common.yaml": "profile::apache::docroot: /srv/qa_two\n",
				"environment.conf":      "config_version = scripts/qa_two_config_version.sh\n",
			},
		},
		commits: map[string]string{
			"canary":        canaryCommit,
			"qa_two":        qaTwoCommit,
			"feature-spike": featureSpikeCommit,
		},
	}
}

func (f *gitFixture) ListBranches(_ context.Context, r local.GitRemote) ([]local.GitBranchRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remotes = append(f.remotes, r)
	out := make([]local.GitBranchRef, 0, len(f.commits))
	for name, sha := range f.commits {
		out = append(out, local.GitBranchRef{Name: name, Commit: sha})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *gitFixture) Open(_ context.Context, r local.GitRemote, branches []string) (local.GitRepo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remotes = append(f.remotes, r)
	f.opened = append(f.opened, branches...)
	for _, b := range branches {
		if _, ok := f.files[b]; !ok {
			return nil, status.Errorf(codes.NotFound, "gitFixture: no content for branch %q", b)
		}
	}
	return &gitFixtureRepo{f: f, branches: append([]string(nil), branches...)}, nil
}

// recordedRemotes returns a copy of every remote the fixture was handed.
func (f *gitFixture) recordedRemotes() []local.GitRemote {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]local.GitRemote(nil), f.remotes...)
}

// openedBranches returns every branch name Open was ever asked for.
func (f *gitFixture) openedBranches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.opened...)
}

// gitFixtureRepo is the read-only view Open returns, carrying only the branches
// that were requested.
type gitFixtureRepo struct {
	f        *gitFixture
	branches []string
}

func (r *gitFixtureRepo) has(branch string) bool {
	for _, b := range r.branches {
		if b == branch {
			return true
		}
	}
	return false
}

func (r *gitFixtureRepo) Commit(branch string) (string, bool) {
	if !r.has(branch) {
		return "", false
	}
	sha, ok := r.f.commits[branch]
	return sha, ok
}

// ListFiles matches a path against a pathspec when the path equals it, or
// starts with it plus a separator, so both an exact file and a directory prefix
// are accepted.
func (r *gitFixtureRepo) ListFiles(branch string, pathspecs ...string) ([]local.GitFileEntry, error) {
	if !r.has(branch) {
		return nil, status.Errorf(codes.NotFound, "gitFixture: branch %q was not opened", branch)
	}
	files := r.f.files[branch]
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []local.GitFileEntry
	for _, p := range paths {
		if len(pathspecs) > 0 {
			match := false
			for _, ps := range pathspecs {
				if p == ps || strings.HasPrefix(p, strings.TrimSuffix(ps, "/")+"/") {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		out = append(out, local.GitFileEntry{Path: p, Mode: "100644", Size: int64(len(files[p]))})
	}
	return out, nil
}

func (r *gitFixtureRepo) ReadFile(branch, path string) ([]byte, error) {
	if !r.has(branch) {
		return nil, status.Errorf(codes.NotFound, "gitFixture: branch %q was not opened", branch)
	}
	body, ok := r.f.files[branch][path]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "gitFixture: no file %q on branch %q", path, branch)
	}
	return []byte(body), nil
}

func (*gitFixtureRepo) Close() error { return nil }

// llmCall is one observed LLM request, with the key the client was handed.
type llmCall struct{ APIKey, System, User string }

// llmFixture is a scripted LLM. It records every call under a mutex because
// the suite runs under -race, and answers by the shape of the schema it is
// asked for, never by call order.
type llmFixture struct {
	mu           sync.Mutex
	calls        []llmCall
	rankingReply string
}

// Complete returns the extraction reply when the schema asks for queries, and
// rankingReply otherwise. The key is read from the APIKey field directly: a
// fmt verb on the provider redacts it.
func (f *llmFixture) Complete(_ context.Context, p local.LLMProvider, req local.LLMRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, llmCall{APIKey: p.APIKey, System: req.System, User: req.User})
	if props, ok := req.Schema["properties"].(map[string]any); ok {
		if _, asksQueries := props["queries"]; asksQueries {
			b, _ := json.Marshal(map[string]any{"queries": []string{"ntp time synchronisation"}})
			return string(b), nil
		}
	}
	return f.rankingReply, nil
}

func (f *llmFixture) observed() []llmCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]llmCall(nil), f.calls...)
}

type rankedEntry struct{ Name, Source, Reasoning string }

// rankingReplyFor builds the model's ranking answer: names and reasoning only.
func rankingReplyFor(entries ...rankedEntry) string {
	out := make([]map[string]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]string{"name": e.Name, "source": e.Source, "reasoning": e.Reasoning})
	}
	b, _ := json.Marshal(map[string]any{"suggestions": out})
	return string(b)
}

// approverToken mints a CodeKind.ApproveScope-scoped token and returns only
// its secret — standing in for an operator obtaining a token out of band.
// Token minting lives in this test file and nowhere else: it is the entire
// structural claim of this example that no function in
// control_repo_authoring.go can reach it.
func approverToken(t *testing.T, h *host.Host, label string) string {
	t.Helper()
	tok, err := h.Auth.IssueToken(context.Background(), &hostv1.IssueTokenRequest{
		Scope:      controlrepoauthoring.CodeKind.ApproveScope,
		Label:      label,
		TtlSeconds: 300,
	})
	if err != nil {
		t.Fatalf("approverToken: IssueToken: %v", err)
	}
	return tok.Secret
}

// ptr returns a pointer to v, for the optional proto3 fields of
// EnvironmentSettings.
func ptr[T any](v T) *T { return &v }

// TestControlRepoAuthoring_EndToEnd walks one blank host through the whole
// slice: create environments, turn a sentence into a ranked real module, check
// it against the registry and its dependency tree, write it to a Puppetfile,
// author a Hiera level and key, then author settings through the approval
// gate, and read every byte back. Run with -v to read it as a transcript.
func TestControlRepoAuthoring_EndToEnd(t *testing.T) {
	ctx := context.Background()
	w := newHost(t)
	h := w.h
	proposer := controlrepoauthoring.NewProposer(h)
	approver := controlrepoauthoring.NewApprover(h)

	// Step 1: two blank environments.
	for _, name := range []string{"authored", "canary"} {
		env, err := proposer.CreateEnvironment(ctx, name)
		if err != nil {
			t.Fatalf("CreateEnvironment(%q): %v", name, err)
		}
		if env.Name != name {
			t.Fatalf("CreateEnvironment: expected name %q, got %q", name, env.Name)
		}
		t.Logf("step 1: created blank environment %q", env.Name)
	}

	// Step 2: a sentence becomes a ranked, real module. The model only orders
	// and describes; every module fact is a copy of a registry search result.
	rec, err := proposer.RecommendModules(ctx, "keep the clocks on my servers in sync", w.provider)
	if err != nil {
		t.Fatalf("RecommendModules: %v", err)
	}
	if len(rec.Suggestions) < 1 {
		t.Fatalf("RecommendModules: expected at least one suggestion, got %+v", rec)
	}
	top := rec.Suggestions[0]
	if top.Module.Name != "puppetlabs/ntp" || top.Module.Source != "puppet-forge" || top.Rank != 1 || top.Reasoning == "" {
		t.Fatalf("RecommendModules: expected rank 1 puppetlabs/ntp from puppet-forge with reasoning, got %+v", top)
	}
	if len(rec.Queries) != 1 || rec.Queries[0] != "ntp time synchronisation" {
		t.Fatalf("RecommendModules: expected the single extracted query to be echoed, got %v", rec.Queries)
	}
	calls := w.llm.observed()
	if len(calls) != 2 {
		t.Fatalf("RecommendModules: expected exactly 2 LLM calls, got %d", len(calls))
	}
	for i, c := range calls {
		if c.APIKey != fixtureAPIKey {
			t.Errorf("LLM call %d: expected the sealed provider key to reach the client", i+1)
		}
		if strings.Contains(c.System, fixtureAPIKey) || strings.Contains(c.User, fixtureAPIKey) {
			t.Errorf("LLM call %d: the provider key leaked into a prompt", i+1)
		}
	}
	t.Logf("step 2: recommended %s (rank %d, %s) from query %q; key reached the client, not the prompts", top.Module.Name, top.Rank, top.Module.Source, rec.Queries[0])

	// Step 3: confirm the suggestion against the registry before trusting it.
	found, err := proposer.SearchModules(ctx, top.Module.Name, top.Module.Source)
	if err != nil {
		t.Fatalf("SearchModules(%q): %v", top.Module.Name, err)
	}
	var exact *hostv1.ForgeSearchResult
	for _, r := range found.Results {
		if r.Name == top.Module.Name {
			exact = r
		}
	}
	if exact == nil || exact.Deprecated {
		t.Fatalf("SearchModules: expected a live exact-name hit for %q, got %+v", top.Module.Name, found.Results)
	}
	// A broader search shows the deprecated decoy next to the real module, and
	// the two are told apart by the registry's own fields.
	broad, err := proposer.SearchModules(ctx, "ntp", top.Module.Source)
	if err != nil {
		t.Fatalf("SearchModules(ntp): %v", err)
	}
	var live, decoy *hostv1.ForgeSearchResult
	for _, r := range broad.Results {
		switch r.Name {
		case "puppetlabs/ntp":
			live = r
		case "example/timekeeper":
			decoy = r
		}
	}
	if live == nil || decoy == nil {
		t.Fatalf("SearchModules(ntp): expected both the live module and the decoy, got %+v", broad.Results)
	}
	if live.SupersededBy != "" || live.Deprecated || !decoy.Deprecated || decoy.SupersededBy != "puppetlabs/ntp" {
		t.Fatalf("SearchModules(ntp): expected only the decoy to be deprecated and superseded, got live=%+v decoy=%+v", live, decoy)
	}
	t.Logf("step 3: registry confirms %s %s; decoy %s is deprecated, superseded by %s", exact.Name, exact.Version, decoy.Name, decoy.SupersededBy)

	// Step 4: resolve its dependency tree against the environment's current
	// Puppetfile. The tree is advice; nothing is written yet.
	tree, err := proposer.ResolveModule(ctx, top.Module.Name, "13.2.1", "authored")
	if err != nil {
		t.Fatalf("ResolveModule: %v", err)
	}
	root := tree.Root
	if root.Name != "puppetlabs/ntp" || root.Version != "13.2.1" || len(root.Dependencies) != 1 {
		t.Fatalf("ResolveModule: expected puppetlabs/ntp 13.2.1 with one dependency, got %+v", root)
	}
	dep := root.Dependencies[0]
	if dep.Name != "puppetlabs/stdlib" || dep.Version != "9.6.0" || dep.Unresolved || dep.AlreadyInPuppetfile {
		t.Fatalf("ResolveModule: expected an unresolved-false, not-yet-present puppetlabs/stdlib 9.6.0, got %+v", dep)
	}
	if !tree.AdvisoryOnly || tree.AdvisoryMessage == "" {
		t.Fatalf("ResolveModule: expected an advisory-only tree with a message, got advisory_only=%v message=%q", tree.AdvisoryOnly, tree.AdvisoryMessage)
	}
	t.Logf("step 4: resolved %s %s -> %s %s (advisory only: %s)", root.Name, root.Version, dep.Name, dep.Version, tree.AdvisoryMessage)

	// Step 5: write the root, then accept each dependency the tree selected and
	// the Puppetfile does not yet hold. Resolve never writes; this is the
	// explicit per-module accept. Names are used exactly as the resolver
	// returned them.
	var accepted []*hostv1.DependencyNode
	var walk func(n *hostv1.DependencyNode)
	walk = func(n *hostv1.DependencyNode) {
		for _, d := range n.Dependencies {
			if d.Unresolved {
				t.Fatalf("ResolveModule: dependency %s is unresolved; refusing to write it", d.Name)
			}
			if !d.AlreadyInPuppetfile {
				accepted = append(accepted, d)
			}
			walk(d)
		}
	}
	accepted = append(accepted, root)
	walk(root)
	for _, n := range accepted {
		mod, err := proposer.AddModule(ctx, "authored", &hostv1.PuppetfileModule{
			Name:   n.Name,
			Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: n.Version}},
		})
		if err != nil {
			t.Fatalf("AddModule(%s): %v", n.Name, err)
		}
		if mod.Name != n.Name || mod.GetForge().GetVersion() != n.Version {
			t.Fatalf("AddModule: expected %s at %s, got %+v", n.Name, n.Version, mod)
		}
		t.Logf("step 5: accepted module %s at %s into authored", mod.Name, mod.GetForge().GetVersion())
	}

	// Step 6: the Hiera half. A first level needs the insert flag; its datadir
	// is one no Puppet default would produce, and the data path agrees with it.
	put, err := proposer.AuthorHieraLevel(ctx, "authored", &hostv1.HieraLevel{Name: "common", Path: "common.yaml", Datadir: "hieradata"})
	if err != nil {
		t.Fatalf("AuthorHieraLevel: %v", err)
	}
	if len(put.Warnings) != 0 {
		t.Fatalf("AuthorHieraLevel: expected no lint warnings, got %+v", put.Warnings)
	}
	assertCommonLevel(t, "AuthorHieraLevel", put.Hierarchy)
	df, err := proposer.AuthorHieraDataKey(ctx, "authored", "common.yaml", "profile::ntp::servers", "ntp1.example.test")
	if err != nil {
		t.Fatalf("AuthorHieraDataKey: %v", err)
	}
	assertScalarKey(t, "AuthorHieraDataKey", df, "profile::ntp::servers", "ntp1.example.test")
	reread, err := proposer.DataFile(ctx, "authored", "common.yaml")
	if err != nil {
		t.Fatalf("DataFile: %v", err)
	}
	assertScalarKey(t, "DataFile", reread, "profile::ntp::servers", "ntp1.example.test")
	hier, err := proposer.Hierarchy(ctx, "authored")
	if err != nil {
		t.Fatalf("Hierarchy: %v", err)
	}
	assertCommonLevel(t, "Hierarchy", hier)
	t.Logf("step 6: authored level %q (datadir %s) and key profile::ntp::servers = ntp1.example.test", hier.Levels[0].Name, hier.Levels[0].Datadir)

	// Step 7: propose the environment settings through the gate.
	const proposalID = "settings-authored-1"
	proposal, err := proposer.ProposeSettings(ctx, proposalID, &hostv1.EnvironmentSettings{
		Environment:        "authored",
		ConfigVersion:      ptr("scripts/config_version.sh"),
		EnvironmentTimeout: ptr("5m"),
	})
	if err != nil {
		t.Fatalf("ProposeSettings: %v", err)
	}
	if proposal.Status != approval.StatusPending {
		t.Fatalf("ProposeSettings: expected status %q, got %q", approval.StatusPending, proposal.Status)
	}
	t.Logf("step 7: proposed settings for authored as %q (status %s)", proposalID, proposal.Status)

	// Step 8: a second persona, holding a token the proposer never saw, approves.
	secret := approverToken(t, h, "operator-ada")
	approved, err := approver.Approve(ctx, proposalID, secret)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Status != approval.StatusApproved {
		t.Fatalf("Approve: expected status %q, got %q", approval.StatusApproved, approved.Status)
	}
	if approved.DecidedBy != "operator-ada" {
		t.Fatalf("Approve: expected DecidedBy %q, got %q", "operator-ada", approved.DecidedBy)
	}
	t.Logf("step 8: %s approved %q", approved.DecidedBy, proposalID)

	// Step 9: apply immediately after Approve, with no intervening read.
	applied, err := proposer.ApplySettings(ctx, proposalID)
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	assertAuthoredSettings(t, "ApplySettings", applied)
	t.Logf("step 9: applied settings: config_version=%s environment_timeout=%s", applied.GetConfigVersion(), applied.GetEnvironmentTimeout())

	// Step 10: read everything back.
	got, err := proposer.Settings(ctx, "authored")
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	assertAuthoredSettings(t, "Settings", got)

	text, err := proposer.RenderPuppetfile(ctx, "authored")
	if err != nil {
		t.Fatalf("RenderPuppetfile: %v", err)
	}
	if want := "mod 'puppetlabs/ntp', '13.2.1'\nmod 'puppetlabs/stdlib', '9.6.0'\n"; text != want {
		t.Fatalf("RenderPuppetfile: expected %q, got %q", want, text)
	}
	mods, err := proposer.ListModules(ctx, "authored")
	if err != nil {
		t.Fatalf("ListModules: %v", err)
	}
	if len(mods) != 2 || mods[0].Name != "puppetlabs/ntp" || mods[1].Name != "puppetlabs/stdlib" {
		t.Fatalf("ListModules: expected [puppetlabs/ntp puppetlabs/stdlib] in insertion order, got %+v", mods)
	}

	envs, err := proposer.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(envs) != 2 || envs[0].Name != "authored" || envs[1].Name != "canary" {
		t.Fatalf("ListEnvironments: expected [authored canary], got %+v", envs)
	}
	t.Logf("step 10: read back settings, Puppetfile %q and environments [%s %s]", text, envs[0].Name, envs[1].Name)

	// Step 11: read an existing control repo before anything is written. The
	// credential was sealed by operator setup; the request names it and nothing
	// else. Every environment this run creates was created above, so the
	// will-overwrite flags in this report are not stale: an environment that
	// appeared after the report would refuse the whole import at apply time.
	credential := sealGitCredential(t, h)
	snap, err := proposer.InspectImport(ctx, controlRepoURL, credential)
	if err != nil {
		t.Fatalf("InspectImport: %v", err)
	}
	if len(snap.Branches) != 3 {
		t.Fatalf("InspectImport: expected 3 branch entries, got %+v", snap.Branches)
	}
	report := map[string]*hostv1.ImportBranchSnapshot{}
	for _, b := range snap.Branches {
		report[b.Branch] = b
	}
	// canary is an environment the author step already created, still blank: a
	// blank environment is still an environment, so it is a will-overwrite.
	if b := report["canary"]; b == nil || !b.Importable || !b.WillOverwrite || b.Commit != canaryCommit {
		t.Fatalf("InspectImport: expected canary importable, will-overwrite, at %s, got %+v", canaryCommit, b)
	}
	if b := report["qa_two"]; b == nil || !b.Importable || b.WillOverwrite || b.Commit != qaTwoCommit {
		t.Fatalf("InspectImport: expected qa_two importable, not a will-overwrite, at %s, got %+v", qaTwoCommit, b)
	}
	spike := report["feature-spike"]
	if spike == nil || spike.Importable || len(spike.Findings) < 1 || !strings.Contains(spike.Findings[0].Message, "[a-z0-9_]") {
		t.Fatalf("InspectImport: expected feature-spike not importable with a finding that explains the branch-name rule, got %+v", spike)
	}
	remotes := w.git.recordedRemotes()
	if len(remotes) < 1 {
		t.Fatalf("InspectImport: expected the fixture to record at least one remote")
	}
	for i, r := range remotes {
		if r.Credential == nil || r.Credential.Kind != local.GitCredentialHTTPSToken || r.Credential.Token != fixtureGitToken {
			t.Errorf("remote %d: expected the sealed https-token credential on every call", i+1)
		}
		if strings.Contains(r.URL, fixtureGitToken) {
			t.Errorf("remote %d: the token travelled in the URL", i+1)
		}
	}
	for _, b := range w.git.openedBranches() {
		if b == "feature-spike" {
			t.Errorf("the fixture was asked to open feature-spike, which cannot be an environment")
		}
	}
	t.Logf("step 11: inspected %s read-only: canary will overwrite an existing environment, qa_two is new, feature-spike is refused by name and never opened; credential %q reached %d fixture calls and no URL", controlRepoURL, credential, len(remotes))

	// Step 12: propose the import. Both selected branches are pinned to the SHAs
	// the report showed; a partially pinned selection would be refused.
	const importID = "import-adopt-1"
	proposed, err := proposer.ProposeImport(ctx, importID, controlRepoURL, credential, []string{"canary", "qa_two"},
		map[string]string{"canary": report["canary"].Commit, "qa_two": report["qa_two"].Commit})
	if err != nil {
		t.Fatalf("ProposeImport: %v", err)
	}
	if proposed.ProposalId != importID || len(proposed.Snapshot.Branches) != 2 ||
		proposed.Snapshot.Branches[0].Branch != "canary" || proposed.Snapshot.Branches[1].Branch != "qa_two" {
		t.Fatalf("ProposeImport: expected proposal %q frozen over canary and qa_two, got %+v", importID, proposed)
	}
	t.Logf("step 12: proposed import %q over branches canary and qa_two, pinned to the commits the report showed", importID)

	// Step 13: a pending import writes nothing. This refusal carries no
	// structured error detail, so the assertion is on the gRPC code and on the
	// absence of written state, never on a message string.
	if _, err := proposer.ApplyImport(ctx, importID); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ApplyImport before approval: expected FailedPrecondition, got %v", err)
	}
	pendingEnvs, err := proposer.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(pendingEnvs) != 2 || pendingEnvs[0].Name != "authored" || pendingEnvs[1].Name != "canary" {
		t.Fatalf("ApplyImport before approval: expected environments to stay [authored canary], got %+v", pendingEnvs)
	}
	if pendingMods, err := proposer.ListModules(ctx, "canary"); err != nil || len(pendingMods) != 0 {
		t.Fatalf("ApplyImport before approval: expected canary to hold no module, got %+v (err %v)", pendingMods, err)
	}
	t.Logf("step 13: apply refused while pending (FailedPrecondition); environments still [authored canary], canary still has no module")

	// Step 14: a person approves, and the apply follows on the same path.
	importSecret := approverToken(t, h, "operator-grace")
	decided, err := approver.Approve(ctx, importID, importSecret)
	if err != nil {
		t.Fatalf("Approve(import): %v", err)
	}
	if decided.Status != approval.StatusApproved {
		t.Fatalf("Approve(import): expected status %q, got %q", approval.StatusApproved, decided.Status)
	}
	imported, err := proposer.ApplyImport(ctx, importID)
	if err != nil {
		t.Fatalf("ApplyImport: %v", err)
	}
	if len(imported.Environments) != 2 {
		t.Fatalf("ApplyImport: expected two environments, got %+v", imported.Environments)
	}
	t.Logf("step 14: %s approved %q and the apply wrote %d environments", decided.DecidedBy, importID, len(imported.Environments))

	// Step 15: the merge happens at the set-of-environments level. An environment
	// with no matching branch is outside the import's blast radius.
	authoredText, err := proposer.RenderPuppetfile(ctx, "authored")
	if err != nil {
		t.Fatalf("RenderPuppetfile(authored): %v", err)
	}
	if authoredText != text {
		t.Fatalf("authored Puppetfile changed by the import: expected %q, got %q", text, authoredText)
	}
	authoredMods, err := proposer.ListModules(ctx, "authored")
	if err != nil || len(authoredMods) != 2 {
		t.Fatalf("authored modules changed by the import: %+v (err %v)", authoredMods, err)
	}
	authoredHier, err := proposer.Hierarchy(ctx, "authored")
	if err != nil {
		t.Fatalf("Hierarchy(authored): %v", err)
	}
	assertCommonLevel(t, "authored hierarchy after import", authoredHier)
	authoredData, err := proposer.DataFile(ctx, "authored", "common.yaml")
	if err != nil {
		t.Fatalf("DataFile(authored): %v", err)
	}
	assertScalarKey(t, "authored data after import", authoredData, "profile::ntp::servers", "ntp1.example.test")
	authoredSettings, err := proposer.Settings(ctx, "authored")
	if err != nil {
		t.Fatalf("Settings(authored): %v", err)
	}
	assertAuthoredSettings(t, "authored settings after import", authoredSettings)

	// canary was blank, and an existing environment is replaced as a whole: the
	// apply deletes every document the environment owns before writing the
	// snapshot. That is why the author step put its content in a different
	// environment, and why canary now holds the imported content and only that.
	// The Puppetfile module keeps the hyphenated name exactly as the fixture
	// Puppetfile spells it, and the data path is relative to the declared
	// datadir, so hieradata/common.yaml reads back at the bare common.yaml.
	canaryMods, err := proposer.ListModules(ctx, "canary")
	if err != nil {
		t.Fatalf("ListModules(canary): %v", err)
	}
	if len(canaryMods) != 1 || canaryMods[0].Name != "puppetlabs-stdlib" || canaryMods[0].GetForge().GetVersion() != "9.4.1" {
		t.Fatalf("canary modules: expected exactly puppetlabs-stdlib 9.4.1, got %+v", canaryMods)
	}
	canaryHier, err := proposer.Hierarchy(ctx, "canary")
	if err != nil {
		t.Fatalf("Hierarchy(canary): %v", err)
	}
	assertImportedLevel(t, "canary hierarchy", canaryHier)
	canaryData, err := proposer.DataFile(ctx, "canary", "common.yaml")
	if err != nil {
		t.Fatalf("DataFile(canary): %v", err)
	}
	assertScalarKey(t, "canary data", canaryData, "profile::ntp::servers", "ntp-canary.example.test")
	canarySettings, err := proposer.Settings(ctx, "canary")
	if err != nil {
		t.Fatalf("Settings(canary): %v", err)
	}
	if canarySettings.ConfigVersion == nil || *canarySettings.ConfigVersion != "scripts/canary_config_version.sh" ||
		canarySettings.EnvironmentTimeout == nil || *canarySettings.EnvironmentTimeout != "10m" {
		t.Fatalf("canary settings: expected the imported config_version and 10m, got %+v", canarySettings)
	}
	if canarySettings.ConfigVersion != nil && *canarySettings.ConfigVersion == "scripts/config_version.sh" {
		t.Fatalf("canary settings: carry the authored environment's value")
	}

	// qa_two is new: it exists only because the import created it.
	qaMods, err := proposer.ListModules(ctx, "qa_two")
	if err != nil {
		t.Fatalf("ListModules(qa_two): %v", err)
	}
	if len(qaMods) != 1 || qaMods[0].Name != "puppetlabs/apache" || qaMods[0].GetForge().GetVersion() != "12.3.0" {
		t.Fatalf("qa_two modules: expected exactly puppetlabs/apache 12.3.0, got %+v", qaMods)
	}
	qaHier, err := proposer.Hierarchy(ctx, "qa_two")
	if err != nil {
		t.Fatalf("Hierarchy(qa_two): %v", err)
	}
	assertImportedLevel(t, "qa_two hierarchy", qaHier)
	qaData, err := proposer.DataFile(ctx, "qa_two", "common.yaml")
	if err != nil {
		t.Fatalf("DataFile(qa_two): %v", err)
	}
	assertScalarKey(t, "qa_two data", qaData, "profile::apache::docroot", "/srv/qa_two")
	qaSettings, err := proposer.Settings(ctx, "qa_two")
	if err != nil {
		t.Fatalf("Settings(qa_two): %v", err)
	}
	if qaSettings.ConfigVersion == nil || *qaSettings.ConfigVersion != "scripts/qa_two_config_version.sh" || qaSettings.EnvironmentTimeout != nil {
		t.Fatalf("qa_two settings: expected only the imported config_version, got %+v", qaSettings)
	}

	finalEnvs, err := proposer.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments(final): %v", err)
	}
	gotNames := make([]string, 0, len(finalEnvs))
	for _, e := range finalEnvs {
		gotNames = append(gotNames, e.Name)
	}
	if want := []string{"authored", "canary", "qa_two"}; !reflect.DeepEqual(gotNames, want) {
		t.Fatalf("ListEnvironments(final): expected %v in ascending order, got %v", want, gotNames)
	}
	t.Logf("step 15: authored untouched; canary replaced wholesale by its branch; qa_two created; environments %v", gotNames)

	// Step 16: bump a module that already exists. This is the second gated path,
	// per item rather than per branch: the ladder is refused, proposed, still
	// refused while pending, approved, apply-pending on a further write, applied.
	//
	// The module name is the hyphenated spelling the imported Puppetfile produced.
	// The facet decides "already exists" by exact name string, so the slug form
	// used in the authored environment (puppetlabs/stdlib) and the hyphenated form
	// the import produced (puppetlabs-stdlib) are different modules to this gate:
	// the slug form would be an ungated append, and the refusal below would fail
	// for the wrong reason.
	bumped := &hostv1.PuppetfileModule{
		Name:   canaryMods[0].Name,
		Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: "9.6.0"}},
	}
	if bumped.Name != "puppetlabs-stdlib" {
		t.Fatalf("overwrite target: expected the imported spelling puppetlabs-stdlib, got %q", bumped.Name)
	}

	// 16a: refused. A write that would replace existing content needs approval.
	_, err = proposer.AddModule(ctx, "canary", bumped)
	if !local.IsCodeOverwriteRequiresApproval(err) || status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("AddModule over an existing module: expected a requires-approval refusal with FailedPrecondition, got %v", err)
	}
	if cur, err := proposer.ListModules(ctx, "canary"); err != nil || len(cur) != 1 || cur[0].GetForge().GetVersion() != "9.4.1" {
		t.Fatalf("refused AddModule: expected canary to still carry 9.4.1, got %+v (err %v)", cur, err)
	}
	t.Logf("step 16a: bumping %s to 9.6.0 is refused as requiring approval; canary still carries 9.4.1", bumped.Name)

	// 16b: propose the bump.
	const bumpID = "bump-stdlib-1"
	bumpProposal, err := proposer.ProposeModuleOverwrite(ctx, bumpID, "canary", bumped)
	if err != nil {
		t.Fatalf("ProposeModuleOverwrite: %v", err)
	}
	if bumpProposal.Status != approval.StatusPending {
		t.Fatalf("ProposeModuleOverwrite: expected status %q, got %q", approval.StatusPending, bumpProposal.Status)
	}
	t.Logf("step 16b: proposed the bump as %q (status %s)", bumpID, bumpProposal.Status)

	// 16c: a pending proposal is not an approval. The refusal is still
	// requires-approval, and it is not the apply-pending refusal.
	_, err = proposer.AddModule(ctx, "canary", bumped)
	if !local.IsCodeOverwriteRequiresApproval(err) || local.IsCodeOverwriteApplyPending(err) {
		t.Fatalf("AddModule with a pending proposal: expected requires-approval and not apply-pending, got %v", err)
	}
	t.Logf("step 16c: a pending proposal does not unlock the write; still refused as requiring approval")

	// 16d: approve. Verifying a token does not consume it, so the token minted for
	// the import is still valid inside its lifetime and serves this cycle too.
	bumpDecided, err := approver.Approve(ctx, bumpID, importSecret)
	if err != nil {
		t.Fatalf("Approve(bump): %v", err)
	}
	if bumpDecided.Status != approval.StatusApproved || bumpDecided.DecidedBy != "operator-grace" {
		t.Fatalf("Approve(bump): expected approved by operator-grace, got status %q by %q", bumpDecided.Status, bumpDecided.DecidedBy)
	}
	t.Logf("step 16d: %s approved %q", bumpDecided.DecidedBy, bumpID)

	// 16e: the refusal changes shape. A write never applies an approval; only the
	// apply RPC does.
	_, err = proposer.AddModule(ctx, "canary", bumped)
	if !local.IsCodeOverwriteApplyPending(err) {
		t.Fatalf("AddModule after approval: expected an apply-pending refusal, got %v", err)
	}
	if cur, err := proposer.ListModules(ctx, "canary"); err != nil || len(cur) != 1 || cur[0].GetForge().GetVersion() != "9.4.1" {
		t.Fatalf("apply-pending AddModule: expected canary to still carry 9.4.1, got %+v (err %v)", cur, err)
	}
	t.Logf("step 16e: the write now reports apply-pending; the content has still not changed")

	// 16f: apply. The replacement is one module, not two.
	appliedMod, err := proposer.ApplyModuleOverwrite(ctx, bumpID)
	if err != nil {
		t.Fatalf("ApplyModuleOverwrite: %v", err)
	}
	if appliedMod.Name != bumped.Name || appliedMod.GetForge().GetVersion() != "9.6.0" {
		t.Fatalf("ApplyModuleOverwrite: expected %s at 9.6.0, got %+v", bumped.Name, appliedMod)
	}
	finalMods, err := proposer.ListModules(ctx, "canary")
	if err != nil {
		t.Fatalf("ListModules(canary) after apply: %v", err)
	}
	if len(finalMods) != 1 || finalMods[0].Name != "puppetlabs-stdlib" || finalMods[0].GetForge().GetVersion() != "9.6.0" {
		t.Fatalf("canary modules after apply: expected exactly puppetlabs-stdlib 9.6.0, got %+v", finalMods)
	}
	t.Logf("step 16f: applied; canary holds exactly one module, %s at %s", finalMods[0].Name, finalMods[0].GetForge().GetVersion())
}

// TestControlRepoAuthoring_PendingImportDoesNotApply is the first half of the
// negative ladder: an import nobody has approved changes nothing at all. It is
// independent of the end-to-end walk: a fresh host with no environment, an
// import proposed over both importable branches, and an apply that is refused.
//
// The refusal carries no structured error detail, so the assertions are on the
// gRPC code and on the absence of written state, never on a message string.
func TestControlRepoAuthoring_PendingImportDoesNotApply(t *testing.T) {
	ctx := context.Background()
	w := newHost(t)
	proposer := controlrepoauthoring.NewProposer(w.h)
	credential := sealGitCredential(t, w.h)

	const importID = "import-pending-1"
	if _, err := proposer.ProposeImport(ctx, importID, controlRepoURL, credential, []string{"canary", "qa_two"}, nil); err != nil {
		t.Fatalf("ProposeImport: %v", err)
	}
	// Reading the proposal back is the test's own act (the proposer cannot read
	// one): it shows the import is genuinely waiting for a decision.
	pending, err := approval.Get(ctx, w.h, controlrepoauthoring.CodeKind, importID)
	if err != nil {
		t.Fatalf("approval.Get: %v", err)
	}
	if pending.Status != approval.StatusPending {
		t.Fatalf("expected the import proposal to be %q, got %q", approval.StatusPending, pending.Status)
	}

	if _, err := proposer.ApplyImport(ctx, importID); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ApplyImport on a pending proposal: expected codes.FailedPrecondition, got %v (%v)", status.Code(err), err)
	}
	envs, err := proposer.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(envs) != 0 {
		t.Fatalf("expected no environment after a refused apply, got %+v", envs)
	}
	t.Logf("pending import %q refused with FailedPrecondition; no environment was created", importID)
}

// forgeModule builds a Forge-sourced Puppetfile module at version.
func forgeModule(name, version string) *hostv1.PuppetfileModule {
	return &hostv1.PuppetfileModule{
		Name:   name,
		Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: version}},
	}
}

// assertModuleVersion fails unless env holds exactly one module, named name, at
// version.
func assertModuleVersion(t *testing.T, p *controlrepoauthoring.ProposerBackend, env, name, version, label string) {
	t.Helper()
	mods, err := p.ListModules(context.Background(), env)
	if err != nil {
		t.Fatalf("%s: ListModules: %v", label, err)
	}
	if len(mods) != 1 || mods[0].Name != name || mods[0].GetForge().GetVersion() != version {
		t.Fatalf("%s: expected exactly %s at %s, got %+v", label, name, version, mods)
	}
}

// TestControlRepoAuthoring_RejectedOverwriteDoesNotApply is the rejected half of
// the negative ladder: a rejected overwrite never applies, a rejection must say
// why, and a decision is terminal. It is independent of the end-to-end walk: a
// fresh host, one environment, one module so that the next write is an
// overwrite.
//
// The apply refusal carries no structured error detail, so it is asserted by
// gRPC code only, never by message.
func TestControlRepoAuthoring_RejectedOverwriteDoesNotApply(t *testing.T) {
	ctx := context.Background()
	w := newHost(t)
	proposer := controlrepoauthoring.NewProposer(w.h)
	approver := controlrepoauthoring.NewApprover(w.h)

	const (
		env  = "rejecting"
		name = "puppetlabs/stdlib"
	)
	if _, err := proposer.CreateEnvironment(ctx, env); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if _, err := proposer.AddModule(ctx, env, forgeModule(name, "9.4.1")); err != nil {
		t.Fatalf("AddModule: %v", err)
	}

	const proposalID = "bump-rejected-1"
	pending, err := proposer.ProposeModuleOverwrite(ctx, proposalID, env, forgeModule(name, "9.6.0"))
	if err != nil {
		t.Fatalf("ProposeModuleOverwrite: %v", err)
	}
	if pending.Status != approval.StatusPending {
		t.Fatalf("ProposeModuleOverwrite: expected status %q, got %q", approval.StatusPending, pending.Status)
	}

	secret := approverToken(t, w.h, "operator-hopper")
	// A rejection must say why.
	if _, err := approver.Reject(ctx, proposalID, secret, ""); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Reject with an empty reason: expected codes.InvalidArgument, got %v (%v)", status.Code(err), err)
	}
	const reason = "not this sprint"
	rejected, err := approver.Reject(ctx, proposalID, secret, reason)
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if rejected.Status != approval.StatusRejected {
		t.Fatalf("Reject: expected status %q, got %q", approval.StatusRejected, rejected.Status)
	}
	if rejected.Reason != reason {
		t.Fatalf("Reject: expected stored reason %q, got %q", reason, rejected.Reason)
	}

	if _, err := proposer.ApplyModuleOverwrite(ctx, proposalID); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ApplyModuleOverwrite on a rejected proposal: expected codes.FailedPrecondition, got %v (%v)", status.Code(err), err)
	}
	assertModuleVersion(t, proposer, env, name, "9.4.1", "after the rejection")

	// A decision is terminal: approving the rejected proposal is refused.
	if _, err := approver.Approve(ctx, proposalID, secret); !approval.IsAlreadyDecided(err) {
		t.Fatalf("Approve on a rejected proposal: expected IsAlreadyDecided, got %v", err)
	}
	assertModuleVersion(t, proposer, env, name, "9.4.1", "after the refused second decision")
	t.Logf("rejected overwrite %q (%q) did not apply; a second decision was refused; the module stayed at 9.4.1", proposalID, reason)
}

// TestControlRepoAuthoring_ReplayRefused proves an approval covers one
// application of one payload. It is independent of the end-to-end walk.
//
// This is NOT a token-reuse test. Verifying a token does not consume it, so one
// approver token legitimately serves every cycle inside its lifetime; the
// single-use property under test belongs to the applied proposal, which the
// facet marks as spent.
func TestControlRepoAuthoring_ReplayRefused(t *testing.T) {
	ctx := context.Background()
	w := newHost(t)
	proposer := controlrepoauthoring.NewProposer(w.h)
	approver := controlrepoauthoring.NewApprover(w.h)

	const (
		env  = "replaying"
		name = "puppetlabs/stdlib"
	)
	if _, err := proposer.CreateEnvironment(ctx, env); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if _, err := proposer.AddModule(ctx, env, forgeModule(name, "9.4.1")); err != nil {
		t.Fatalf("AddModule: %v", err)
	}

	const proposalID = "bump-replay-1"
	if _, err := proposer.ProposeModuleOverwrite(ctx, proposalID, env, forgeModule(name, "9.6.0")); err != nil {
		t.Fatalf("ProposeModuleOverwrite: %v", err)
	}
	if _, err := approver.Approve(ctx, proposalID, approverToken(t, w.h, "operator-hopper")); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := proposer.ApplyModuleOverwrite(ctx, proposalID); err != nil {
		t.Fatalf("ApplyModuleOverwrite (first): %v", err)
	}
	assertModuleVersion(t, proposer, env, name, "9.6.0", "after the first apply")

	// 1: a repeat against the unchanged target would change nothing, so it is
	// idempotent rather than an error.
	repeat, err := proposer.ApplyModuleOverwrite(ctx, proposalID)
	if err != nil {
		t.Fatalf("ApplyModuleOverwrite (repeat on an unchanged target): expected success, got %v", err)
	}
	if repeat.GetForge().GetVersion() != "9.6.0" {
		t.Fatalf("ApplyModuleOverwrite (repeat): expected 9.6.0, got %+v", repeat)
	}
	assertModuleVersion(t, proposer, env, name, "9.6.0", "after the idempotent repeat")

	// 2: move the target by routes the gate does not cover. Removing is not an
	// overwrite, and writing a name that is no longer present is a create.
	if err := proposer.RemoveModule(ctx, env, name); err != nil {
		t.Fatalf("RemoveModule: %v", err)
	}
	if _, err := proposer.AddModule(ctx, env, forgeModule(name, "9.8.0")); err != nil {
		t.Fatalf("AddModule (third version): %v", err)
	}
	assertModuleVersion(t, proposer, env, name, "9.8.0", "after the ungated change")

	// 3: replaying the spent approval now would revert a later change nobody
	// approved, so it is refused.
	if _, err := proposer.ApplyModuleOverwrite(ctx, proposalID); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ApplyModuleOverwrite (replay after the target moved): expected codes.FailedPrecondition, got %v (%v)", status.Code(err), err)
	}
	// 4: the refused replay wrote nothing.
	assertModuleVersion(t, proposer, env, name, "9.8.0", "after the refused replay")
	t.Logf("approval %q applied once, repeated idempotently, then refused after the target moved to 9.8.0; the module stayed at 9.8.0", proposalID)
}

// TestControlRepoAuthoring_RecommendIsGrounded is the mechanical form of the
// grounding claim: the host owns every module fact and the model only orders
// them. A ranked module no registry search produced is dropped with the host's
// warning rather than shown as a suggestion.
func TestControlRepoAuthoring_RecommendIsGrounded(t *testing.T) {
	w := newHost(t)
	w.llm.rankingReply = rankingReplyFor(
		rankedEntry{"puppetlabs/ntp", "puppet-forge", "Keeps clocks in sync."},
		rankedEntry{"acme/not-real", "puppet-forge", "A module the model made up."},
	)
	rec, err := controlrepoauthoring.NewProposer(w.h).RecommendModules(context.Background(), "keep the clocks on my servers in sync", w.provider)
	if err != nil {
		t.Fatalf("RecommendModules: %v", err)
	}
	if len(rec.Suggestions) != 1 || rec.Suggestions[0].Module.Name != "puppetlabs/ntp" {
		t.Fatalf("expected exactly one suggestion, puppetlabs/ntp, got %+v", rec.Suggestions)
	}
	dropped := false
	for _, warn := range rec.Warnings {
		if warn.Code == "recommend_unknown_module_dropped" && warn.Module == "acme/not-real" {
			dropped = true
		}
	}
	if !dropped {
		t.Fatalf("expected a recommend_unknown_module_dropped warning for acme/not-real, got %+v", rec.Warnings)
	}
	t.Logf("grounded: the invented module was dropped with warning %q; one real suggestion remains", "recommend_unknown_module_dropped")
}

// assertCommonLevel checks the one authored hierarchy level and the schema
// version the Code facet reports for it.
func assertCommonLevel(t *testing.T, label string, hier *hostv1.HieraHierarchy) {
	t.Helper()
	if hier.GetVersion() != 5 || len(hier.GetLevels()) != 1 {
		t.Fatalf("%s: expected hierarchy version 5 with one level, got %+v", label, hier)
	}
	lvl := hier.Levels[0]
	if lvl.Name != "common" || lvl.Path != "common.yaml" || lvl.Datadir != "hieradata" {
		t.Fatalf("%s: expected level common / common.yaml / hieradata, got %+v", label, lvl)
	}
}

// assertImportedLevel checks an imported hierarchy: version 5, one level named
// common reading common.yaml, and the hieradata datadir the imported hiera.yaml
// declared as its default.
func assertImportedLevel(t *testing.T, label string, hier *hostv1.HieraHierarchy) {
	t.Helper()
	if hier.GetVersion() != 5 || len(hier.GetLevels()) != 1 {
		t.Fatalf("%s: expected hierarchy version 5 with one level, got %+v", label, hier)
	}
	lvl := hier.Levels[0]
	datadir := lvl.Datadir
	if datadir == "" {
		datadir = hier.DefaultDatadir
	}
	if lvl.Name != "common" || lvl.Path != "common.yaml" || datadir != "hieradata" {
		t.Fatalf("%s: expected level common / common.yaml under datadir hieradata, got %+v", label, hier)
	}
}

// assertScalarKey reads a data key back through the single-field "v"
// convention, not by comparing structs.
func assertScalarKey(t *testing.T, label string, df *hostv1.HieraDataFile, key string, want any) {
	t.Helper()
	v, ok := df.GetValues()[key]
	if !ok || v.GetValue() == nil {
		t.Fatalf("%s: expected key %q in %+v", label, key, df)
	}
	if got := v.GetValue().AsMap()["v"]; got != want {
		t.Fatalf("%s: expected %q = %v, got %v", label, key, want, got)
	}
}

// assertAuthoredSettings checks the two written fields and that the five
// unwritten ones are still unset — presence, not zero value: a setting nobody
// wrote stays absent, it does not become a Puppet default.
func assertAuthoredSettings(t *testing.T, label string, s *hostv1.EnvironmentSettings) {
	t.Helper()
	if s.GetEnvironment() != "authored" {
		t.Fatalf("%s: expected environment %q, got %q", label, "authored", s.GetEnvironment())
	}
	if s.ConfigVersion == nil || *s.ConfigVersion != "scripts/config_version.sh" {
		t.Fatalf("%s: expected ConfigVersion %q, got %v", label, "scripts/config_version.sh", s.ConfigVersion)
	}
	if s.EnvironmentTimeout == nil || *s.EnvironmentTimeout != "5m" {
		t.Fatalf("%s: expected EnvironmentTimeout %q, got %v", label, "5m", s.EnvironmentTimeout)
	}
	if s.Modulepath != nil || s.Manifest != nil || s.DisablePerEnvironmentManifest != nil || s.StaticCatalogs != nil || s.RichData != nil {
		t.Fatalf("%s: expected Modulepath, Manifest, DisablePerEnvironmentManifest, StaticCatalogs and RichData to stay unset, got %+v", label, s)
	}
}

// TestControlRepoAuthoring_ManifestDeclaresExactly pins the manifest's grant
// by equality, so an added headroom permission, a renamed route or an
// approval scope promoted into permissions fails here.
func TestControlRepoAuthoring_ManifestDeclaresExactly(t *testing.T) {
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	m, findings := manifest.Parse(raw)
	if len(findings) > 0 {
		t.Fatalf("manifest.json failed to parse: %v", findings)
	}
	if findings := manifest.Validate(m); len(findings) > 0 {
		t.Fatalf("manifest.json failed validation: %v", findings)
	}

	// The registry read permission for the manifest content rule is absent on
	// purpose: no host/local facet checks it, and the only rule that requires
	// it fires inside the Content != nil branch, which this manifest's null
	// content never enters. Do not add it back.
	wantPerms := []string{"code:rw", "code:import", "forge:rw", "forge:recommend", "secrets:rw", "tokens:issue"}
	if !reflect.DeepEqual(m.Permissions, wantPerms) {
		t.Fatalf("permissions: expected exactly %v, got %v", wantPerms, m.Permissions)
	}
	for _, p := range m.Permissions {
		if p == controlrepoauthoring.CodeKind.ApproveScope {
			t.Fatalf("permissions must never contain %q: the approval scope is a per-decision token, not a standing install-time grant", p)
		}
	}

	type wantRoute struct{ opID, scope string }
	want := []wantRoute{
		{"proposeImport", ""},
		{"proposeOverwrite", ""},
		{"approveProposal", "code:approve"},
		{"rejectProposal", "code:approve"},
	}
	if len(m.Routes) != len(want) {
		t.Fatalf("routes: expected %d, got %d: %+v", len(want), len(m.Routes), m.Routes)
	}
	for i, w := range want {
		if m.Routes[i].OperationID != w.opID {
			t.Errorf("route %d: expected operation_id %q, got %q", i, w.opID, m.Routes[i].OperationID)
		}
		if m.Routes[i].Access.Scope != w.scope {
			t.Errorf("route %q: expected access scope %q, got %q", w.opID, w.scope, m.Routes[i].Access.Scope)
		}
	}

	if m.Content != nil {
		t.Errorf("content: expected nil, got %+v", m.Content)
	}
	if m.OpenAPIPath == "" {
		t.Errorf("openapi_path: required because the manifest declares routes")
	}
}

// TestControlRepoAuthoring_CodeKindIsPinned keeps the two literals the README
// and both guides quote from drifting silently: CodeKind is built from the
// code package constants, and this test is what ties it to the strings.
func TestControlRepoAuthoring_CodeKindIsPinned(t *testing.T) {
	want := approval.Kind{Collection: "code-overwrites", ApproveScope: "code:approve"}
	if controlrepoauthoring.CodeKind != want {
		t.Fatalf("CodeKind: expected %+v, got %+v", want, controlrepoauthoring.CodeKind)
	}
}

// --- Static call-graph analysis backing TestControlRepoAuthoring_ProposerCannotSelfApprove ---

// packageIndex holds every declaration in control_repo_authoring.go's
// production source (never the _test.go files), indexed two ways: by bare
// package-level function name, and by "ReceiverType.MethodName" for every
// method. methodsByReceiver additionally groups methods by receiver type
// name so a walk can be seeded from every method of a given persona type.
type packageIndex struct {
	funcsByName       map[string]*ast.FuncDecl
	methodsByKey      map[string]*ast.FuncDecl
	methodsByReceiver map[string][]*ast.FuncDecl
	all               []*ast.FuncDecl
}

// receiverTypeName returns the identifier name of fl's single receiver
// type, with any leading pointer star removed, or "" if fl names no
// receiver (a package-level function).
func receiverTypeName(fl *ast.FieldList) string {
	if fl == nil || len(fl.List) == 0 {
		return ""
	}
	expr := fl.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// buildPackageIndex parses the controlrepoauthoring package's own directory,
// excluding every file ending in "_test.go" — the walker analyses only
// production source, so the token-minting helper living in this very test
// file cannot make the analysis trip over itself.
func buildPackageIndex(t *testing.T) *packageIndex {
	t.Helper()
	fset := token.NewFileSet()
	filter := func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}
	pkgs, err := parser.ParseDir(fset, ".", filter, 0)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}
	pkg, ok := pkgs["controlrepoauthoring"]
	if !ok {
		names := make([]string, 0, len(pkgs))
		for name := range pkgs {
			names = append(names, name)
		}
		t.Fatalf("expected package %q in %v, found packages: %v", "controlrepoauthoring", ".", names)
	}

	idx := &packageIndex{
		funcsByName:       map[string]*ast.FuncDecl{},
		methodsByKey:      map[string]*ast.FuncDecl{},
		methodsByReceiver: map[string][]*ast.FuncDecl{},
	}
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			idx.all = append(idx.all, fn)
			recvType := receiverTypeName(fn.Recv)
			if recvType == "" {
				idx.funcsByName[fn.Name.Name] = fn
				continue
			}
			idx.methodsByKey[recvType+"."+fn.Name.Name] = fn
			idx.methodsByReceiver[recvType] = append(idx.methodsByReceiver[recvType], fn)
		}
	}
	return idx
}

// reachableSelectors walks every method whose receiver type is
// receiverType, and every declaration transitively called from it,
// recording every selector expression syntactically present in each
// visited body. It over-approximates the true call graph in the safe
// direction: it records a selector's bare name for EVERY *ast.SelectorExpr
// encountered, whether or not that selector is actually invoked as a call,
// so it can only report more reachable selectors than truly exist, never
// fewer — a refusal assertion built on this can never produce a false
// pass. selectors holds every bare selector name (e.g. "Approve");
// qualified additionally holds "identifier.Selector" whenever the
// expression's left side is a plain identifier (e.g. "approval.Approve").
// resolved is the count of distinct declarations the walk actually
// visited.
func reachableSelectors(idx *packageIndex, receiverType string) (selectors map[string]bool, qualified map[string]bool, resolved int) {
	selectors = map[string]bool{}
	qualified = map[string]bool{}
	visited := map[*ast.FuncDecl]bool{}

	worklist := append([]*ast.FuncDecl{}, idx.methodsByReceiver[receiverType]...)
	for len(worklist) > 0 {
		fn := worklist[0]
		worklist = worklist[1:]
		if visited[fn] {
			continue
		}
		visited[fn] = true
		if fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				selectors[x.Sel.Name] = true
				if ident, ok := x.X.(*ast.Ident); ok {
					qualified[ident.Name+"."+x.Sel.Name] = true
				}
			case *ast.CallExpr:
				switch callee := x.Fun.(type) {
				case *ast.Ident:
					if pkgFn, ok := idx.funcsByName[callee.Name]; ok {
						worklist = append(worklist, pkgFn)
					}
				case *ast.SelectorExpr:
					if m, ok := idx.methodsByKey[receiverType+"."+callee.Sel.Name]; ok {
						worklist = append(worklist, m)
					}
				}
			}
			return true
		})
	}
	return selectors, qualified, len(visited)
}

// TestControlRepoAuthoring_ProposerCannotSelfApprove is the mechanized form
// of ROADMAP criterion 2 for this example: it parses only this package's own
// directory (the cwd under go test), so it is independently scoped from the
// Phase 5 example's identical-in-shape test, and asserts over the real AST
// that the proposing and deciding call graphs are structurally separate.
func TestControlRepoAuthoring_ProposerCannotSelfApprove(t *testing.T) {
	idx := buildPackageIndex(t)

	proposerSelectors, proposerQualified, proposerResolved := reachableSelectors(idx, "ProposerBackend")
	approverSelectors, approverQualified, approverResolved := reachableSelectors(idx, "ApproverBackend")

	// Liveness — asserted first and separately, because a refusal assertion
	// over an empty set passes for free. This block proves the walker sees
	// real call edges before its refusals mean anything.
	if !proposerQualified["approval.ProposeBody"] {
		t.Errorf("liveness: expected approval.ProposeBody reachable from ProposerBackend, got qualified selectors %v", proposerQualified)
	}
	if !proposerSelectors["ApplyEnvironmentSettings"] {
		t.Errorf("liveness: expected ApplyEnvironmentSettings reachable from ProposerBackend, got selectors %v", proposerSelectors)
	}
	if !approverQualified["approval.Approve"] {
		t.Errorf("liveness: expected approval.Approve reachable from ApproverBackend, got qualified selectors %v", approverQualified)
	}
	if !approverQualified["approval.Reject"] {
		t.Errorf("liveness: expected approval.Reject reachable from ApproverBackend, got qualified selectors %v", approverQualified)
	}
	// The apply selectors are named so the walk is proven to have reached the
	// import and per-item overwrite paths, not just the settings one.
	for _, sel := range []string{"ApplyImport", "ApplyPuppetfileModuleOverwrite"} {
		if !proposerSelectors[sel] {
			t.Errorf("liveness: expected %s reachable from ProposerBackend, got selectors %v", sel, proposerSelectors)
		}
	}
	// ProposerBackend declares 21 methods; the floor is set from that real count
	// (the walk also visits the scalarValue helper, so it resolves 22).
	if proposerResolved < 21 {
		t.Errorf("liveness: expected the walker to resolve at least 21 ProposerBackend methods, resolved %d", proposerResolved)
	}
	if approverResolved < 2 {
		t.Errorf("liveness: expected the walker to resolve at least 2 ApproverBackend methods, resolved %d", approverResolved)
	}

	// Refusal, proposer side: nothing reachable from ProposerBackend may
	// select Approve/Reject/Get on the approval package, IssueToken or Verify
	// on anything, or touch the Documents facet. Apply* selectors are
	// deliberately allowed: an apply only reads an already-approved proposal.
	for _, forbidden := range []string{"approval.Approve", "approval.Reject", "approval.Get"} {
		if proposerQualified[forbidden] {
			t.Errorf("security: ProposerBackend can reach %s — proposing and deciding have landed in one call graph, which docs/approval-pattern.md requires be treated as a security change", forbidden)
		}
	}
	for _, forbidden := range []string{"IssueToken", "Verify"} {
		if proposerSelectors[forbidden] {
			t.Errorf("security: ProposerBackend can reach a selector named %s on some receiver — the proposing persona must never mint or verify a token (docs/approval-pattern.md)", forbidden)
		}
	}
	if proposerSelectors["Documents"] {
		t.Errorf("security: ProposerBackend touches the Documents facet directly — every proposal write must go through the approval package, not raw Documents access (docs/approval-pattern.md)")
	}

	// Refusal, approver side: nothing reachable from ApproverBackend may
	// select Propose or ProposeBody on the approval package, touch the Code,
	// Forge or Documents facets, or select IssueToken.
	for _, forbidden := range []string{"approval.Propose", "approval.ProposeBody"} {
		if approverQualified[forbidden] {
			t.Errorf("security: ApproverBackend can reach %s — deciding and proposing have landed in one call graph (docs/approval-pattern.md)", forbidden)
		}
	}
	for _, forbidden := range []string{"Code", "Forge", "Documents"} {
		if approverSelectors[forbidden] {
			t.Errorf("security: ApproverBackend touches the %s facet directly — it must only decide, never author or materialize (docs/approval-pattern.md)", forbidden)
		}
	}
	if approverSelectors["IssueToken"] {
		t.Errorf("security: ApproverBackend can reach IssueToken — the approving persona must never mint its own token (docs/approval-pattern.md)")
	}

	// Refusal, whole package: IssueToken must appear in no function body
	// anywhere in the package's non-test source — the example's production
	// code cannot mint a token at all.
	for _, fn := range idx.all {
		if fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "IssueToken" {
				recv := receiverTypeName(fn.Recv)
				t.Errorf("security: function %s.%s selects IssueToken — no production code in this package may mint a token (docs/approval-pattern.md)", recv, fn.Name.Name)
			}
			return true
		})
	}
}
