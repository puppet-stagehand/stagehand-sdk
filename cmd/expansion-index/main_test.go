package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	code, out, errs := runCmd(t, "build", "--catalog", catalog, "--feed", "official", "--now", "2026-10-09T14:30:12Z", "--key", keyPath, "--out", outPath, "--format", "json")
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
	if code, o, e := runCmd(t, "build", "--catalog", catalog, "--feed", "official", "--now", "2026-10-09T15:00:00Z", "--out", out2); code != 0 {
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
