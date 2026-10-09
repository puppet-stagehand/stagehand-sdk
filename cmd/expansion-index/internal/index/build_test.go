package index

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/catalogfile"
	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/testreg"
)

const (
	ghcrUser = "robot"
	ghcrPass = "s3cret-token"
)

// manifestFor returns the SDK's hello example manifest with the given id and version.
func manifestFor(t *testing.T, id, version string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "examples", "hello", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["id"], doc["version"] = id, version
	out, _ := json.Marshal(doc)
	return out
}

func digestOfRef(ref string) string { return ref[strings.Index(ref, "@")+1:] }

// catalogFor builds a one-feed, one-pack catalog.
func catalogFor(indexRepo, imagesRepo string, vs ...catalogfile.Version) *catalogfile.File {
	return &catalogfile.File{
		Format: 1,
		Feeds:  map[string]catalogfile.Feed{"official": {Index: indexRepo, Images: imagesRepo, Visibility: "public"}},
		Packs:  []catalogfile.Pack{{ID: "hello", Feed: "official", Versions: vs}},
	}
}

func ver(version, candidate, released string) catalogfile.Version {
	return catalogfile.Version{Version: version, Candidate: candidate, ReleasedAt: released}
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func codesOf(fs []Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Code)
	}
	return strings.Join(out, ",")
}

// publish pushes res as an immutable tag of repo and points latest at it.
func publish(t *testing.T, res *BuildResult, repo string, latest bool, ropts ...remote.Option) string {
	t.Helper()
	d, fs := Push(context.Background(), PushOptions{Index: res.Raw, Repo: "oci://" + repo})
	if len(fs) > 0 {
		t.Fatalf("push: %v", fs)
	}
	if latest {
		r, _ := name.NewRepository(repo)
		desc, err := remote.Get(r.Digest(d), ropts...)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Tag(r.Tag("latest"), desc, ropts...); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

type ghcrEnv struct {
	g         *testreg.GHCRLike
	staging   *testreg.Registry
	cand1     string
	cand2     string
	indexRepo string
	imagesRep string
}

func newGHCREnv(t *testing.T) *ghcrEnv {
	t.Helper()
	g := testreg.NewGHCRLike(t, ghcrUser, ghcrPass)
	staging := testreg.New(t)
	return &ghcrEnv{
		g: g, staging: staging,
		cand1:     testreg.PushCandidate(t, staging.Host+"/staging/hello", manifestFor(t, "hello", "0.1.0")),
		cand2:     testreg.PushCandidate(t, staging.Host+"/staging/hello2", manifestFor(t, "hello", "0.2.0")),
		indexRepo: g.Host + "/catalog",
		imagesRep: g.Host + "/packs",
	}
}

func (e *ghcrEnv) catalog(vs ...catalogfile.Version) *catalogfile.File {
	return catalogFor(e.indexRepo, e.imagesRep, vs...)
}

func (e *ghcrEnv) build(t *testing.T, cat *catalogfile.File, now string, prev *PreviousOptions) (*BuildResult, []Finding) {
	t.Helper()
	return Build(context.Background(), BuildOptions{Catalog: cat, Feed: "official", Now: at(now), Previous: prev})
}

func (e *ghcrEnv) prev(allow bool, authed bool) *PreviousOptions {
	p := &PreviousOptions{Repo: "oci://" + e.indexRepo, AllowMissing: allow}
	if authed {
		p.Auth = e.g.Auth()
	}
	return p
}

// A never-created GHCR package answers an anonymous reader exactly like a
// private one (403 DENIED at the token endpoint), so it must stop the build
// with or without --allow-missing-previous.
func TestPreviousReadNeverCreatedAnonymous(t *testing.T) {
	e := newGHCREnv(t)
	cat := e.catalog(ver("0.1.0", e.cand1, "2026-10-09"))
	for _, allow := range []bool{false, true} {
		res, fs := e.build(t, cat, "2026-10-09T10:00:00Z", e.prev(allow, false))
		if res != nil || codesOf(fs) != "previous_denied" {
			t.Fatalf("allow=%v: res %v findings %q", allow, res != nil, codesOf(fs))
		}
		if !strings.Contains(fs[0].Fix, "--previous-username-env/--previous-password-env") {
			t.Fatalf("the fix must point at the push credentials: %q", fs[0].Fix)
		}
	}
}

// With the credentials that will push, the same never-created repository is
// requested with the push,pull scope and reads as not found.
func TestPreviousReadNeverCreatedPushScope(t *testing.T) {
	e := newGHCREnv(t)
	cat := e.catalog(ver("0.1.0", e.cand1, "2026-10-09"))
	res, fs := e.build(t, cat, "2026-10-09T10:00:00Z", e.prev(true, true))
	if len(fs) != 0 || res == nil || res.Previous != PreviousNotFound {
		t.Fatalf("findings %q, res %+v", codesOf(fs), res)
	}
	want := "repository:catalog:push,pull"
	found := false
	for _, s := range e.g.ScopesRequested() {
		if s == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("the previous read must request %q, requested %v", want, e.g.ScopesRequested())
	}
	if e.g.Exists("catalog") {
		t.Fatal("a read must not create the repository")
	}
	_, fs = e.build(t, cat, "2026-10-09T10:00:00Z", e.prev(false, true))
	if codesOf(fs) != "previous_missing" {
		t.Fatalf("without the flag: %q", codesOf(fs))
	}
}

func TestPreviousReadExistingPrivateAnonymous(t *testing.T) {
	e := newGHCREnv(t)
	testreg.DockerConfig(t, e.g.Host, ghcrUser, ghcrPass)
	cat := e.catalog(ver("0.1.0", e.cand1, "2026-10-09"))
	first, fs := e.build(t, cat, "2026-10-09T10:00:00Z", e.prev(true, true))
	if len(fs) != 0 {
		t.Fatal(fs)
	}
	publish(t, first, e.indexRepo, true, remote.WithAuth(e.g.Auth()))
	for _, allow := range []bool{false, true} {
		res, fs := e.build(t, cat, "2026-10-09T11:00:00Z", e.prev(allow, false))
		if res != nil || codesOf(fs) != "previous_denied" {
			t.Fatalf("allow=%v: a private existing feed read anonymously must stop the build, got %q", allow, codesOf(fs))
		}
	}
}

func TestPreviousReadWrongCredentials(t *testing.T) {
	e := newGHCREnv(t)
	testreg.DockerConfig(t, e.g.Host, ghcrUser, ghcrPass)
	cat := e.catalog(ver("0.1.0", e.cand1, "2026-10-09"))
	first, _ := e.build(t, cat, "2026-10-09T10:00:00Z", e.prev(true, true))
	publish(t, first, e.indexRepo, true, remote.WithAuth(e.g.Auth()))
	bad := e.prev(true, false)
	bad.Auth = &authn.Basic{Username: ghcrUser, Password: "wrong"}
	res, fs := e.build(t, cat, "2026-10-09T11:00:00Z", bad)
	if res != nil || codesOf(fs) != "previous_denied" {
		t.Fatalf("wrong credentials must stop the build even with --allow-missing-previous, got %q", codesOf(fs))
	}
}

func TestPreviousReadNoLatestYet(t *testing.T) {
	e := newGHCREnv(t)
	testreg.DockerConfig(t, e.g.Host, ghcrUser, ghcrPass)
	cat := e.catalog(ver("0.1.0", e.cand1, "2026-10-09"))
	first, _ := e.build(t, cat, "2026-10-09T10:00:00Z", e.prev(true, true))
	publish(t, first, e.indexRepo, false) // immutable tag only: the state after the first-run stop
	res, fs := e.build(t, cat, "2026-10-09T11:00:00Z", e.prev(true, true))
	if len(fs) != 0 || res.Previous != PreviousNotFound {
		t.Fatalf("a repository with a tag and no latest reads as not found: %q", codesOf(fs))
	}
	_, fs = e.build(t, cat, "2026-10-09T11:00:00Z", e.prev(false, true))
	if codesOf(fs) != "previous_missing" {
		t.Fatalf("without the flag: %q", codesOf(fs))
	}
}

func TestPreviousReadFound(t *testing.T) {
	e := newGHCREnv(t)
	testreg.DockerConfig(t, e.g.Host, ghcrUser, ghcrPass)
	authOpt := remote.WithAuth(e.g.Auth())
	first, fs := e.build(t, e.catalog(ver("0.1.0", e.cand1, "2026-10-09")), "2026-10-09T10:00:00Z", e.prev(true, true))
	if len(fs) != 0 {
		t.Fatal(fs)
	}
	publish(t, first, e.indexRepo, true, authOpt)

	cat := e.catalog(ver("0.1.0", e.cand1, "2026-10-09"), ver("0.2.0", e.cand2, "2026-10-10"))
	res, fs := e.build(t, cat, "2026-10-10T10:00:00Z", e.prev(false, true))
	if len(fs) != 0 {
		t.Fatalf("findings %q", codesOf(fs))
	}
	if res.Previous != PreviousFound {
		t.Fatalf("previous = %q", res.Previous)
	}
	listed := map[string]bool{}
	for _, im := range res.Images {
		listed[im.Version] = im.AlreadyListed
		if !strings.HasPrefix(im.Destination, e.imagesRep+"/hello@sha256:") {
			t.Fatalf("destination %q", im.Destination)
		}
	}
	if !listed["0.1.0"] || listed["0.2.0"] {
		t.Fatalf("already_listed = %v, want 0.1.0 true and 0.2.0 false", listed)
	}
	// Anonymous reads of a public feed work too, and the freshness rule still applies.
	e.g.SetPublic("catalog", true)
	if _, fs := e.build(t, cat, "2026-10-09T10:00:00Z", e.prev(false, false)); codesOf(fs) != "generated_at_not_increasing" {
		t.Fatalf("an anonymous read of a public feed applies the freshness rule: %q", codesOf(fs))
	}
}

// generatedAtEnv publishes one index at 2026-10-09T10:00:00Z and tags latest.
func generatedAtEnv(t *testing.T) (*testreg.Registry, *catalogfile.File, string) {
	t.Helper()
	testreg.NoDockerConfig(t)
	reg := testreg.New(t)
	cand := testreg.PushCandidate(t, reg.Host+"/staging/hello", manifestFor(t, "hello", "0.1.0"))
	cat := catalogFor(reg.Host+"/catalog", reg.Host+"/packs", ver("0.1.0", cand, "2026-10-09"))
	first, fs := Build(context.Background(), BuildOptions{Catalog: cat, Feed: "official", Now: at("2026-10-09T10:00:00Z")})
	if len(fs) != 0 {
		t.Fatal(fs)
	}
	publish(t, first, reg.Host+"/catalog", true)
	return reg, cat, "oci://" + reg.Host + "/catalog"
}

func buildWithPrev(cat *catalogfile.File, prev, now string) (*BuildResult, []Finding) {
	return Build(context.Background(), BuildOptions{Catalog: cat, Feed: "official", Now: at(now), Previous: &PreviousOptions{Repo: prev}})
}

func TestBuildGeneratedAtEqual(t *testing.T) {
	_, cat, prev := generatedAtEnv(t)
	res, fs := buildWithPrev(cat, prev, "2026-10-09T10:00:00Z")
	if res != nil || codesOf(fs) != "generated_at_not_increasing" {
		t.Fatalf("an equal generated_at must be refused, got %q", codesOf(fs))
	}
}

func TestBuildGeneratedAtEarlier(t *testing.T) {
	_, cat, prev := generatedAtEnv(t)
	res, fs := buildWithPrev(cat, prev, "2026-10-09T09:59:59Z")
	if res != nil || codesOf(fs) != "generated_at_not_increasing" {
		t.Fatalf("an earlier generated_at must be refused, got %q", codesOf(fs))
	}
}

func TestBuildGeneratedAtLater(t *testing.T) {
	_, cat, prev := generatedAtEnv(t)
	res, fs := buildWithPrev(cat, prev, "2026-10-09T10:00:01Z")
	if len(fs) != 0 || res.Index.GeneratedAt != "2026-10-09T10:00:01Z" || res.Tag != "20261009T100001Z" {
		t.Fatalf("a later generated_at must succeed: %q", codesOf(fs))
	}
}

func TestBuildGeneratedAtPreviousUnstamped(t *testing.T) {
	reg, cat, prev := generatedAtEnv(t)
	raw := []byte(`{"forge_version":1,"name":"official","publisher_keys":[],"packs":[]}`)
	testreg.PushIndexArtifact(t, reg.Host+"/catalog:latest", raw)
	res, fs := buildWithPrev(cat, prev, "2027-01-01T00:00:00Z")
	if res != nil || codesOf(fs) != "previous_unstamped" {
		t.Fatalf("a previous index without generated_at must be refused, got %q", codesOf(fs))
	}
}

func TestPreviousReadInvalidArtifact(t *testing.T) {
	reg, cat, prev := generatedAtEnv(t)
	testreg.PushIndexArtifact(t, reg.Host+"/catalog:latest", []byte(`{"forge_version":2}`))
	_, fs := buildWithPrev(cat, prev, "2027-01-01T00:00:00Z")
	if codesOf(fs) != "previous_invalid" {
		t.Fatalf("got %q", codesOf(fs))
	}
}

func TestClassifyRegistryError(t *testing.T) {
	mk := func(status int, codes ...transport.ErrorCode) error {
		e := &transport.Error{StatusCode: status}
		for _, c := range codes {
			e.Errors = append(e.Errors, transport.Diagnostic{Code: c})
		}
		return e
	}
	cases := []struct {
		name string
		err  error
		want PreviousOutcome
	}{
		{"404 NAME_UNKNOWN", mk(404, transport.NameUnknownErrorCode), NotFound},
		{"404 MANIFEST_UNKNOWN", mk(404, transport.ManifestUnknownErrorCode), NotFound},
		{"404 empty body", mk(404), NotFound},
		{"404 other code", mk(404, transport.BlobUnknownErrorCode), Failed},
		{"401", mk(401, transport.UnauthorizedErrorCode), Denied},
		{"401 bare", mk(401), Denied},
		{"403", mk(403), Denied},
		{"403 DENIED from the token endpoint", mk(403, transport.DeniedErrorCode), Denied},
		{"500", mk(500), Failed},
		{"TLS", x509.UnknownAuthorityError{}, Failed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyRegistryError(c.err); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
	// A 404 that carries a non-registry body (a proxy's HTML page) is not "not found".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte("<html>no such thing</html>"))
	}))
	defer srv.Close()
	ref, _ := name.ParseReference(strings.TrimPrefix(srv.URL, "http://") + "/x/y:latest")
	_, err := remote.Get(ref)
	if got := classifyRegistryError(err); got != Failed {
		t.Fatalf("a 404 with an HTML body: got %v, want Failed (%v)", got, err)
	}
	// A real registry's unknown repository is NotFound.
	reg := testreg.New(t)
	ref, _ = name.ParseReference(reg.Host + "/never/created:latest")
	_, err = remote.Get(ref)
	if got := classifyRegistryError(err); got != NotFound {
		t.Fatalf("got %v (%v)", got, err)
	}
	_, err = remote.Head(ref)
	if got := classifyRegistryError(err); got != NotFound {
		t.Fatalf("HEAD: got %v (%v)", got, err)
	}
}

func buildHello(t *testing.T, reg *testreg.Registry, mutate func(c *catalogfile.Version, m *[]byte)) (*BuildResult, []Finding) {
	t.Helper()
	m := manifestFor(t, "hello", "0.1.0")
	v := ver("0.1.0", "", "2026-10-09")
	mutate(&v, &m)
	if v.Candidate == "" {
		v.Candidate = testreg.PushCandidate(t, reg.Host+"/staging/hello", m)
	}
	cat := catalogFor(reg.Host+"/catalog", reg.Host+"/packs", v)
	return Build(context.Background(), BuildOptions{Catalog: cat, Feed: "official", Now: at("2026-10-09T10:00:00Z")})
}

func TestBuildManifestCrossCheck(t *testing.T) {
	reg := testreg.New(t)
	t.Run("version in the image differs from catalog", func(t *testing.T) {
		_, fs := buildHello(t, reg, func(v *catalogfile.Version, m *[]byte) { *m = manifestFor(t, "hello", "0.2.0") })
		if codesOf(fs) != "manifest_version_mismatch" || !strings.Contains(fs[0].Message, "hello") || !strings.Contains(fs[0].Message, "0.1.0") {
			t.Fatalf("%v", fs)
		}
	})
	t.Run("id in the image differs from catalog", func(t *testing.T) {
		_, fs := buildHello(t, reg, func(v *catalogfile.Version, m *[]byte) { *m = manifestFor(t, "other", "0.1.0") })
		if codesOf(fs) != "manifest_id_mismatch" {
			t.Fatalf("%v", fs)
		}
	})
	t.Run("image has no manifest", func(t *testing.T) {
		_, fs := buildHello(t, reg, func(v *catalogfile.Version, m *[]byte) {
			v.Candidate = testreg.PushImage(t, reg.Host+"/staging/empty", []v1.Layer{testreg.TarLayer(t, testreg.File{Path: "etc/motd", Data: []byte("hi")})})
		})
		if codesOf(fs) != "candidate_no_manifest" {
			t.Fatalf("%v", fs)
		}
	})
	t.Run("manifest fails the SDK's own validation", func(t *testing.T) {
		_, fs := buildHello(t, reg, func(v *catalogfile.Version, m *[]byte) {
			*m = []byte(strings.Replace(string(*m), `"tier": "core"`, `"tier": "gold"`, 1))
			*m = []byte(strings.Replace(string(*m), `"tier":"core"`, `"tier":"gold"`, 1))
		})
		if !strings.Contains(codesOf(fs), "candidate_manifest_invalid") {
			t.Fatalf("%v", fs)
		}
	})
	t.Run("candidate is not digest-pinned", func(t *testing.T) {
		_, fs := buildHello(t, reg, func(v *catalogfile.Version, m *[]byte) { v.Candidate = reg.Host + "/staging/hello:candidate" })
		if codesOf(fs) != "candidate_not_pinned" {
			t.Fatalf("%v", fs)
		}
	})
	t.Run("upper layer overrides and a whiteout deletes", func(t *testing.T) {
		old := manifestFor(t, "hello", "0.0.9")
		cur := manifestFor(t, "hello", "0.1.0")
		ref := testreg.PushImage(t, reg.Host+"/staging/layered", []v1.Layer{
			testreg.TarLayer(t, testreg.File{Path: testreg.ManifestPath, Data: old}),
			testreg.TarLayer(t, testreg.File{Path: "./" + testreg.ManifestPath, Data: cur}),
		})
		res, fs := buildHello(t, reg, func(v *catalogfile.Version, m *[]byte) { v.Candidate = ref })
		if len(fs) != 0 || res.Index.Packs[0].Versions[0].Version != "0.1.0" {
			t.Fatalf("the topmost layer's manifest must win: %v", fs)
		}
		gone := testreg.PushImage(t, reg.Host+"/staging/whiteout", []v1.Layer{
			testreg.TarLayer(t, testreg.File{Path: testreg.ManifestPath, Data: cur}),
			testreg.TarLayer(t, testreg.File{Path: "stagehand/.wh.manifest.json", Data: nil}),
		})
		_, fs = buildHello(t, reg, func(v *catalogfile.Version, m *[]byte) { v.Candidate = gone })
		if codesOf(fs) != "candidate_no_manifest" {
			t.Fatalf("a whiteout means the manifest was deleted: %v", fs)
		}
	})
	t.Run("candidate that does not exist names the pack and version", func(t *testing.T) {
		_, fs := buildHello(t, reg, func(v *catalogfile.Version, m *[]byte) {
			v.Candidate = reg.Host + "/staging/ghost@sha256:" + strings.Repeat("a", 64)
		})
		if codesOf(fs) != "candidate_unreadable" || !strings.Contains(fs[0].Message, "hello") || !strings.Contains(fs[0].Message, "0.1.0") {
			t.Fatalf("%v", fs)
		}
	})
}

// D-12: pulling a version means rebuilding without it; nothing else changes.
func TestBuildRemovalDiffersOnlyByThatVersion(t *testing.T) {
	testreg.NoDockerConfig(t)
	reg := testreg.New(t)
	c1 := testreg.PushCandidate(t, reg.Host+"/staging/hello", manifestFor(t, "hello", "0.1.0"))
	c2 := testreg.PushCandidate(t, reg.Host+"/staging/hello2", manifestFor(t, "hello", "0.2.0"))
	full := catalogFor(reg.Host+"/catalog", reg.Host+"/packs", ver("0.1.0", c1, "2026-10-09"), ver("0.2.0", c2, "2026-10-10"))
	one, fs := Build(context.Background(), BuildOptions{Catalog: full, Feed: "official", Now: at("2026-10-10T10:00:00Z")})
	if len(fs) != 0 {
		t.Fatal(fs)
	}
	publish(t, one, reg.Host+"/catalog", true)
	trimmed := catalogFor(reg.Host+"/catalog", reg.Host+"/packs", ver("0.1.0", c1, "2026-10-09"))
	two, fs := Build(context.Background(), BuildOptions{Catalog: trimmed, Feed: "official", Now: at("2026-10-11T10:00:00Z"),
		Previous: &PreviousOptions{Repo: "oci://" + reg.Host + "/catalog"}})
	if len(fs) != 0 {
		t.Fatal(fs)
	}
	want := *one.Index
	want.GeneratedAt = "2026-10-11T10:00:00Z"
	want.Packs = []IndexPack{one.Index.Packs[0]}
	var kept []IndexVersion
	for _, v := range one.Index.Packs[0].Versions {
		if v.Version != "0.2.0" {
			kept = append(kept, v)
		}
	}
	want.Packs[0].Versions = kept
	if !reflect.DeepEqual(&want, two.Index) {
		a, _ := json.MarshalIndent(&want, "", " ")
		b, _ := json.MarshalIndent(two.Index, "", " ")
		t.Fatalf("removing a version changed more than that version:\nwant %s\ngot  %s", a, b)
	}
}

// The final image reference lives in the destination repository, at the candidate's digest.
func TestBuildDestinationKeepsDigest(t *testing.T) {
	reg := testreg.New(t)
	res, fs := buildHello(t, reg, func(v *catalogfile.Version, m *[]byte) {})
	if len(fs) != 0 {
		t.Fatal(fs)
	}
	im := res.Images[0]
	if im.Destination != reg.Host+"/packs/hello@"+digestOfRef(im.Candidate) || im.AlreadyListed {
		t.Fatalf("%+v", im)
	}
}
