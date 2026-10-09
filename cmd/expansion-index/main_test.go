package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/index"
	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/testreg"
)

// runCmd runs the CLI and returns the exit code and combined stdout/stderr.
func runCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func helloManifest(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "hello", "manifest.json"))
	if err != nil {
		t.Fatalf("read the SDK's example manifest: %v", err)
	}
	return raw
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// isolateDocker keeps the default keychain away from the developer's real
// docker config.
func isolateDocker(t *testing.T) {
	t.Helper()
	t.Setenv("DOCKER_CONFIG", t.TempDir())
}

func TestRoundTrip(t *testing.T) {
	isolateDocker(t)
	reg := testreg.New(t)
	candidate := testreg.PushCandidate(t, reg.Host+"/staging/hello", helloManifest(t))
	digest := candidate[strings.Index(candidate, "@")+1:]

	dir := t.TempDir()
	catalog := filepath.Join(dir, "catalog.yaml")
	writeFile(t, catalog, `format: 1
feeds:
  official:
    index: `+reg.Host+`/catalog
    images: `+reg.Host+`/packs
    visibility: public
packs:
  - id: hello
    feed: official
    versions:
      - version: 0.1.0
        candidate: `+candidate+`
        released_at: 2026-10-09
`)
	signer := testreg.NewSigner(t)
	keyPath := testreg.WriteKey(t, dir, "cosign.pub", signer.PublicPEM)
	outPath := filepath.Join(dir, "index.json")

	code, out, errs := runCmd(t, "build", "--catalog", catalog, "--feed", "official", "--now", "2026-10-09T14:30:12Z", "--key", keyPath, "--previous", "oci://"+reg.Host+"/catalog", "--allow-missing-previous", "--out", outPath, "--format", "json")
	if code != 0 {
		t.Fatalf("build exit %d\n%s\n%s", code, out, errs)
	}
	var built struct {
		OK  bool   `json:"ok"`
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal([]byte(out), &built); err != nil || !built.OK || built.Tag != "20261009T143012Z" {
		t.Fatalf("build json %q: %v", out, err)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.DecodeStrict(raw)
	if err != nil {
		t.Fatalf("the built index must pass the console's strict decode: %v", err)
	}
	if len(idx.Packs) != 1 || idx.Packs[0].ID != "hello" {
		t.Fatalf("packs: %+v", idx.Packs)
	}
	p := idx.Packs[0]
	if p.Name != "Hello" || p.Publisher != "Example Org" || p.Tier != "core" || p.Licence != "Apache-2.0" || p.Entitlement != "none" || !strings.HasPrefix(p.Summary, "Shows a greeting") {
		t.Fatalf("manifest-owned fields not taken from the image: %+v", p)
	}
	v := p.Versions[0]
	if v.Image != reg.Host+"/packs/hello@"+digest || v.ContractVersion != 1 || v.ReleasedAt != "2026-10-09" {
		t.Fatalf("version: %+v", v)
	}
	if strings.Join(v.Permissions, ",") != "documents:rw,puppetdb:read" {
		t.Fatalf("permissions: %v", v.Permissions)
	}
	if len(idx.PublisherKeys) != 1 || !strings.HasPrefix(idx.PublisherKeys[0].ID, "sha256:") {
		t.Fatalf("publisher_keys: %+v", idx.PublisherKeys)
	}

	repo := "oci://" + reg.Host + "/catalog"
	code, out, errs = runCmd(t, "push", "--index", outPath, "--ref", repo, "--tag", built.Tag)
	if code != 0 {
		t.Fatalf("push exit %d\n%s\n%s", code, out, errs)
	}
	pushed := strings.TrimSpace(out)
	if !strings.HasPrefix(pushed, "sha256:") {
		t.Fatalf("push printed %q", pushed)
	}
	tagRef, _ := name.ParseReference(reg.Host + "/catalog:" + built.Tag)
	desc, err := remote.Get(tagRef)
	if err != nil || desc.Digest.String() != pushed {
		t.Fatalf("the immutable tag does not resolve to the pushed digest: %v", err)
	}
	art, err := desc.Image()
	if err != nil {
		t.Fatal(err)
	}
	layers, _ := art.Layers()
	if len(layers) != 1 {
		t.Fatalf("artifact has %d layers, want 1", len(layers))
	}
	if mt, _ := layers[0].MediaType(); string(mt) != index.IndexArtifactMediaType {
		t.Fatalf("layer media type %q", mt)
	}

	signer.Sign(t, reg.Host+"/catalog@"+pushed)
	ref := repo + ":" + built.Tag
	code, out, errs = runCmd(t, "verify", "--ref", ref, "--key", keyPath, "--expect-digest", pushed)
	if code != 0 {
		t.Fatalf("verify exit %d\n%s\n%s", code, out, errs)
	}

	otherKey := testreg.WriteKey(t, dir, "other.pub", testreg.NewSigner(t).PublicPEM)
	code, out, _ = runCmd(t, "verify", "--ref", ref, "--key", otherKey)
	if code != 1 || !strings.Contains(out, "untrusted signer") {
		t.Fatalf("wrong key: exit %d, out %q", code, out)
	}

	// A second index nobody signed.
	out2 := filepath.Join(dir, "index2.json")
	if code, o, e := runCmd(t, "build", "--catalog", catalog, "--feed", "official", "--now", "2026-10-09T15:00:00Z", "--previous", "oci://"+reg.Host+"/catalog", "--allow-missing-previous", "--out", out2); code != 0 {
		t.Fatalf("second build exit %d\n%s\n%s", code, o, e)
	}
	if code, o, e := runCmd(t, "push", "--index", out2, "--ref", repo); code != 0 {
		t.Fatalf("second push exit %d\n%s\n%s", code, o, e)
	}
	code, out, _ = runCmd(t, "verify", "--ref", repo+":20261009T150000Z", "--key", keyPath)
	if code != 1 || !strings.Contains(out, "unsigned") {
		t.Fatalf("unsigned index: exit %d, out %q", code, out)
	}
}

func TestDecodeStrict(t *testing.T) {
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	good := map[string]any{
		"forge_version": 1, "name": "official", "generated_at": "2026-10-09T14:30:12Z",
		"publisher_keys": []any{},
		"packs": []any{map[string]any{
			"id": "hello", "name": "Hello", "publisher": "Example Org", "tier": "core", "licence": "Apache-2.0", "entitlement": "none",
			"versions": []any{map[string]any{
				"version": "0.1.0", "contract_version": 1, "image": "ghcr.io/org/packs/hello@" + digest,
				"permissions": []any{}, "released_at": "2026-10-09",
			}},
		}},
	}
	encode := func(mutate func(map[string]any)) []byte {
		raw, _ := json.Marshal(good)
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		mutate(doc)
		out, _ := json.Marshal(doc)
		return out
	}
	ver := func(doc map[string]any) map[string]any {
		return doc["packs"].([]any)[0].(map[string]any)["versions"].([]any)[0].(map[string]any)
	}
	if _, err := index.DecodeStrict(encode(func(map[string]any) {})); err != nil {
		t.Fatalf("a good index must decode: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"extra top-level field", func(d map[string]any) { d["extra"] = true }},
		{"uppercase image ref", func(d map[string]any) { ver(d)["image"] = "GHCR.io/org/packs/hello@" + digest }},
		{"tag-pinned image", func(d map[string]any) { ver(d)["image"] = "ghcr.io/org/packs/hello:1.0.0" }},
		{"released_at with a time", func(d map[string]any) { ver(d)["released_at"] = "2026-10-09T00:00:00Z" }},
		{"forge_version 2", func(d map[string]any) { d["forge_version"] = 2 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := index.DecodeStrict(encode(c.mutate)); err == nil {
				t.Fatal("expected the console's strict rules to refuse this")
			}
		})
	}
	if _, err := index.DecodeStrict(append(encode(func(map[string]any) {}), []byte(` {}`)...)); err == nil {
		t.Fatal("trailing data must be refused")
	}
}

func catalogYAML(indexRepo, imagesRepo, candidate string) string {
	return `format: 1
feeds:
  official:
    index: ` + indexRepo + `
    images: ` + imagesRepo + `
    visibility: public
packs:
  - id: hello
    feed: official
    versions:
      - version: 0.1.0
        candidate: ` + candidate + `
        released_at: 2026-10-09
`
}

func TestBuildCandidateCredentialsFromEnv(t *testing.T) {
	isolateDocker(t)
	staging := testreg.NewBasicAuth(t, "ci", "pw-1234")
	basic := remote.WithAuth(&authn.Basic{Username: "ci", Password: "pw-1234"})
	candidate := testreg.PushCandidate(t, staging.Host+"/staging/hello", helloManifest(t), basic)
	out := testreg.New(t)

	dir := t.TempDir()
	catalog := filepath.Join(dir, "catalog.yaml")
	writeFile(t, catalog, catalogYAML(out.Host+"/catalog", out.Host+"/packs", candidate))
	idx := filepath.Join(dir, "index.json")
	args := []string{"build", "--catalog", catalog, "--feed", "official", "--now", "2026-10-09T14:30:12Z", "--previous", "oci://" + out.Host + "/catalog",
		"--allow-missing-previous", "--out", idx, "--format", "json"}

	// No credentials: the private staging repository refuses the read.
	code, o, _ := runCmd(t, args...)
	if code != 1 || !strings.Contains(o, "candidate_denied") || !strings.Contains(o, "--candidate-username-env") {
		t.Fatalf("anonymous candidate read: exit %d\n%s", code, o)
	}
	// Wrong credentials.
	t.Setenv("EI_TEST_USER", "ci")
	t.Setenv("EI_TEST_PASS", "wrong")
	code, o, _ = runCmd(t, append(args, "--candidate-username-env", "EI_TEST_USER", "--candidate-password-env", "EI_TEST_PASS")...)
	if code != 1 || !strings.Contains(o, "candidate_denied") {
		t.Fatalf("wrong candidate credentials: exit %d\n%s", code, o)
	}
	if strings.Contains(o, "wrong") || strings.Contains(o, "pw-1234") {
		t.Fatalf("credentials must never be printed:\n%s", o)
	}
	// Right credentials.
	t.Setenv("EI_TEST_PASS", "pw-1234")
	code, o, errs := runCmd(t, append(args, "--candidate-username-env", "EI_TEST_USER", "--candidate-password-env", "EI_TEST_PASS")...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, o, errs)
	}
	if strings.Contains(o+errs, "pw-1234") {
		t.Fatalf("credentials must never be printed:\n%s\n%s", o, errs)
	}
	var got struct {
		OK       bool   `json:"ok"`
		Previous string `json:"previous"`
		Images   []struct {
			Pack          string `json:"pack"`
			Version       string `json:"version"`
			Candidate     string `json:"candidate"`
			Destination   string `json:"destination"`
			AlreadyListed bool   `json:"already_listed"`
		} `json:"images"`
	}
	if err := json.Unmarshal([]byte(o), &got); err != nil || !got.OK || got.Previous != "not_found" || len(got.Images) != 1 {
		t.Fatalf("json %q: %v", o, err)
	}
	im := got.Images[0]
	if im.Pack != "hello" || im.Version != "0.1.0" || im.Candidate != candidate ||
		im.Destination != out.Host+"/packs/hello@"+candidate[strings.Index(candidate, "@")+1:] || im.AlreadyListed {
		t.Fatalf("image entry %+v", im)
	}
}

func TestBuildEnvironmentErrors(t *testing.T) {
	dir := t.TempDir()
	catalog := filepath.Join(dir, "catalog.yaml")
	writeFile(t, catalog, catalogYAML("r.example/catalog", "r.example/packs", "r.example/s/h@sha256:"+strings.Repeat("0", 64)))
	base := []string{"build", "--catalog", catalog, "--feed", "official", "--previous", "oci://r.example/catalog", "--out", filepath.Join(dir, "i.json")}
	t.Setenv("EI_SET", "x")
	for name, extra := range map[string][]string{
		"only one of the pair":    {"--candidate-username-env", "EI_SET"},
		"variable not set":        {"--candidate-username-env", "EI_SET", "--candidate-password-env", "EI_NOT_SET"},
		"previous pair half":      {"--previous-password-env", "EI_SET"},
		"bad --now":               {"--now", "yesterday"},
		"unreadable key":          {"--key", filepath.Join(dir, "missing.pub")},
		"no previous flag at all": nil,
	} {
		t.Run(name, func(t *testing.T) {
			args := append([]string(nil), base...)
			if name == "no previous flag at all" {
				args = []string{"build", "--catalog", catalog, "--feed", "official", "--out", filepath.Join(dir, "i.json")}
			}
			if code, _, errs := runCmd(t, append(args, extra...)...); code != 2 {
				t.Fatalf("exit %d, want 2 (stderr %q)", code, errs)
			}
		})
	}
	if code, _, _ := runCmd(t, "build", "--catalog", filepath.Join(dir, "nope.yaml"), "--feed", "official", "--previous", "oci://r/x", "--out", "x"); code != 2 {
		t.Fatalf("a missing catalog file is an environment error: exit %d", code)
	}
}

func TestBuildCheckIsOffline(t *testing.T) {
	dir := t.TempDir()
	catalog := filepath.Join(dir, "catalog.yaml")
	// Nothing listens at r.example and no registry is contacted.
	writeFile(t, catalog, catalogYAML("r.example/catalog", "r.example/packs", "r.example/s/h@sha256:"+strings.Repeat("0", 64)))
	out := filepath.Join(dir, "index.json")
	code, o, errs := runCmd(t, "build", "--check", "--catalog", catalog, "--feed", "official", "--out", out)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, o, errs)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("--check must write nothing")
	}
	if code, _, _ := runCmd(t, "build", "--check", "--catalog", catalog); code != 0 {
		t.Fatalf("--check needs neither --feed nor --previous: exit %d", code)
	}
	if code, o, _ := runCmd(t, "build", "--check", "--catalog", catalog, "--feed", "nope"); code != 1 || !strings.Contains(o, "unknown_feed") {
		t.Fatalf("exit %d\n%s", code, o)
	}
	writeFile(t, catalog, strings.Replace(catalogYAML("r.example/catalog", "r.example/packs", "r.example/s/h@sha256:"+strings.Repeat("0", 64)), "format: 1", "format: 7", 1))
	if code, o, _ := runCmd(t, "build", "--check", "--catalog", catalog, "--format", "json"); code != 1 || !strings.Contains(o, "catalog_format") {
		t.Fatalf("exit %d\n%s", code, o)
	}
}

// No flag or environment variable relaxes a 401 or 403 on the previous read.
func TestBuildPreviousDeniedThroughTheCLI(t *testing.T) {
	isolateDocker(t)
	g := testreg.NewGHCRLike(t, "robot", "tok-9876")
	staging := testreg.New(t)
	candidate := testreg.PushCandidate(t, staging.Host+"/staging/hello", helloManifest(t))
	dir := t.TempDir()
	catalog := filepath.Join(dir, "catalog.yaml")
	writeFile(t, catalog, catalogYAML(g.Host+"/catalog", g.Host+"/packs", candidate))
	base := []string{"build", "--catalog", catalog, "--feed", "official", "--previous", "oci://" + g.Host + "/catalog", "--out", filepath.Join(dir, "i.json"), "--format", "json"}

	for _, allow := range [][]string{nil, {"--allow-missing-previous"}} {
		code, o, _ := runCmd(t, append(append([]string(nil), base...), allow...)...)
		if code != 1 || !strings.Contains(o, "previous_denied") || !strings.Contains(o, "--previous-username-env") {
			t.Fatalf("anonymous previous read of a never-created package: exit %d\n%s", code, o)
		}
	}
	t.Setenv("EI_PU", "robot")
	t.Setenv("EI_PP", "tok-9876")
	creds := []string{"--previous-username-env", "EI_PU", "--previous-password-env", "EI_PP"}
	code, o, _ := runCmd(t, append(append([]string(nil), base...), creds...)...)
	if code != 1 || !strings.Contains(o, "previous_missing") {
		t.Fatalf("authenticated, no flag: exit %d\n%s", code, o)
	}
	code, o, errs := runCmd(t, append(append(append([]string(nil), base...), creds...), "--allow-missing-previous")...)
	if code != 0 || !strings.Contains(o, `"previous": "not_found"`) {
		t.Fatalf("authenticated with the flag: exit %d\n%s\n%s", code, o, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "i.json")); err != nil {
		t.Fatal("a successful build writes the index")
	}
}

func TestPromoteAndVerifyJSONThroughTheCLI(t *testing.T) {
	isolateDocker(t)
	reg := testreg.New(t)
	candidate := testreg.PushCandidate(t, reg.Host+"/packs/hello", helloManifest(t))
	dir := t.TempDir()
	catalog := filepath.Join(dir, "catalog.yaml")
	writeFile(t, catalog, catalogYAML(reg.Host+"/catalog", reg.Host+"/packs", candidate))
	signer := testreg.NewSigner(t)
	keyPath := testreg.WriteKey(t, dir, "cosign.pub", signer.PublicPEM)
	idx := filepath.Join(dir, "index.json")
	repo := "oci://" + reg.Host + "/catalog"

	if code, o, e := runCmd(t, "build", "--catalog", catalog, "--feed", "official", "--now", "2026-10-09T14:30:12Z", "--previous", repo, "--allow-missing-previous", "--out", idx); code != 0 {
		t.Fatalf("build %d\n%s\n%s", code, o, e)
	}
	code, o, e := runCmd(t, "push", "--index", idx, "--ref", repo)
	if code != 0 {
		t.Fatalf("push %d\n%s\n%s", code, o, e)
	}
	digest := strings.TrimSpace(o)

	// Unsigned: promote refuses and latest does not exist.
	if code, o, _ := runCmd(t, "promote", "--ref", repo, "--digest", digest, "--key", keyPath); code != 1 || !strings.Contains(o, "unsigned") {
		t.Fatalf("promote of an unsigned digest: %d\n%s", code, o)
	}
	if _, err := remote.Head(mustRef(t, reg.Host+"/catalog:latest")); err == nil {
		t.Fatal("latest must not exist after a refused promote")
	}
	signer.Sign(t, reg.Host+"/catalog@"+digest)
	if code, o, e := runCmd(t, "promote", "--ref", repo, "--digest", digest, "--key", keyPath); code != 0 {
		t.Fatalf("promote %d\n%s\n%s", code, o, e)
	}
	// The image is unsigned: plain verify passes, --images does not.
	code, o, _ = runCmd(t, "verify", "--ref", repo, "--key", keyPath, "--images", "--format", "json")
	var doc struct {
		OK          bool   `json:"ok"`
		Digest      string `json:"digest"`
		GeneratedAt string `json:"generated_at"`
		Images      []struct {
			Pack     string `json:"pack"`
			Verified *bool  `json:"verified"`
		} `json:"images"`
		Findings []struct {
			Code string `json:"code"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(o), &doc); err != nil {
		t.Fatalf("json %q: %v", o, err)
	}
	if code != 1 || doc.OK || doc.Digest != digest || doc.GeneratedAt != "2026-10-09T14:30:12Z" || len(doc.Images) != 1 || doc.Images[0].Verified == nil || *doc.Images[0].Verified || len(doc.Findings) != 1 {
		t.Fatalf("exit %d, doc %+v", code, doc)
	}
	signer.Sign(t, reg.Host+"/packs/hello@"+candidate[strings.Index(candidate, "@")+1:])
	if code, o, _ := runCmd(t, "verify", "--ref", repo, "--key", keyPath, "--images", "--expect-digest", digest); code != 0 {
		t.Fatalf("verify --images %d\n%s", code, o)
	}
	if code, _, _ := runCmd(t, "verify", "--ref", repo, "--key", keyPath, "--expect-digest", "sha256:"+strings.Repeat("3", 64)); code != 1 {
		t.Fatalf("a different --expect-digest must exit 1, got %d", code)
	}
	if code, _, _ := runCmd(t, "promote", "--ref", repo); code != 2 {
		t.Fatalf("missing flags: %d", code)
	}
}

func mustRef(t *testing.T, s string) name.Reference {
	t.Helper()
	r, err := name.ParseReference(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
