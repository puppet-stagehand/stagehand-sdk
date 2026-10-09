package index

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/catalogfile"
	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/testreg"
)

func TestTagFor(t *testing.T) {
	if got := TagFor(time.Date(2026, 10, 9, 14, 30, 12, 0, time.UTC)); got != "20261009T143012Z" {
		t.Fatalf("got %q", got)
	}
	// A non-UTC instant is rendered in UTC; the tag never carries a colon.
	loc := time.FixedZone("x", 2*3600)
	if got := TagFor(time.Date(2026, 10, 9, 16, 30, 12, 0, loc)); got != "20261009T143012Z" || strings.Contains(got, ":") {
		t.Fatalf("got %q", got)
	}
	if !immutableTagPattern.MatchString(TagFor(time.Now())) {
		t.Fatal("TagFor must match the immutable tag pattern Promote refuses")
	}
}

// pushIndex builds at now and returns the result.
func buildOnly(t *testing.T, cat *catalogfile.File, now string) *BuildResult {
	t.Helper()
	res, fs := Build(context.Background(), BuildOptions{Catalog: cat, Feed: "official", Now: at(now)})
	if len(fs) != 0 {
		t.Fatalf("build: %v", fs)
	}
	return res
}

func TestPushRefusesAnExistingTag(t *testing.T) {
	testreg.NoDockerConfig(t)
	reg := testreg.New(t)
	c1 := testreg.PushCandidate(t, reg.Host+"/staging/hello", manifestFor(t, "hello", "0.1.0"))
	c2 := testreg.PushCandidate(t, reg.Host+"/staging/hello2", manifestFor(t, "hello", "0.2.0"))
	repo := "oci://" + reg.Host + "/catalog"
	first := buildOnly(t, catalogFor(reg.Host+"/catalog", reg.Host+"/packs", ver("0.1.0", c1, "2026-10-09")), "2026-10-09T10:00:00Z")
	other := buildOnly(t, catalogFor(reg.Host+"/catalog", reg.Host+"/packs", ver("0.1.0", c1, "2026-10-09"), ver("0.2.0", c2, "2026-10-10")), "2026-10-09T10:00:00Z")

	d1, fs := Push(context.Background(), PushOptions{Index: first.Raw, Repo: repo})
	if len(fs) != 0 {
		t.Fatal(fs)
	}
	// The same tag with different content, and the same tag with identical content: both refused.
	for name, raw := range map[string][]byte{"different content": other.Raw, "identical content": first.Raw} {
		d, fs := Push(context.Background(), PushOptions{Index: raw, Repo: repo})
		if d != "" || codesOf(fs) != "tag_exists" {
			t.Fatalf("%s: digest %q findings %q", name, d, codesOf(fs))
		}
	}
	ref, _ := name.ParseReference(reg.Host + "/catalog:" + first.Tag)
	desc, err := remote.Head(ref)
	if err != nil || desc.Digest.String() != d1 {
		t.Fatalf("the existing tag must be untouched: %v %v", desc, err)
	}
}

func TestPushRefusesWhatAConsoleWouldRefuse(t *testing.T) {
	testreg.NoDockerConfig(t)
	reg := testreg.New(t)
	repo := "oci://" + reg.Host + "/catalog"
	if _, fs := Push(context.Background(), PushOptions{Index: []byte(`{"forge_version":1}`), Repo: repo}); codesOf(fs) != "index_invalid" {
		t.Fatalf("%q", codesOf(fs))
	}
	unstamped := []byte(`{"forge_version":1,"name":"x","publisher_keys":[],"packs":[]}`)
	if _, fs := Push(context.Background(), PushOptions{Index: unstamped, Repo: repo}); codesOf(fs) != "index_unstamped" {
		t.Fatalf("%q", codesOf(fs))
	}
	stamped := []byte(`{"forge_version":1,"name":"x","generated_at":"2026-10-09T14:30:12Z","publisher_keys":[],"packs":[]}`)
	if _, fs := Push(context.Background(), PushOptions{Index: stamped, Repo: repo, Tag: "latest"}); codesOf(fs) != "tag_mismatch" {
		t.Fatalf("an immutable push must carry the generated_at tag, got %q", codesOf(fs))
	}
}

// The first publish of a never-created GHCR package: the existence check of the
// immutable tag must read "absent", never "denied", and the write creates it.
func TestPushNeverCreated(t *testing.T) {
	g := testreg.NewGHCRLike(t, ghcrUser, ghcrPass)
	staging := testreg.New(t)
	cand := testreg.PushCandidate(t, staging.Host+"/staging/hello", manifestFor(t, "hello", "0.1.0"))
	res := buildOnly(t, catalogFor(g.Host+"/catalog", g.Host+"/packs", ver("0.1.0", cand, "2026-10-09")), "2026-10-09T10:00:00Z")
	repo := "oci://" + g.Host + "/catalog"

	// Without push-capable credentials nothing is created and the finding says denied.
	testreg.NoDockerConfig(t)
	if d, fs := Push(context.Background(), PushOptions{Index: res.Raw, Repo: repo}); d != "" || codesOf(fs) != "push_denied" {
		t.Fatalf("anonymous push: %q %q", d, codesOf(fs))
	}
	if g.Exists("catalog") {
		t.Fatal("a denied push must not create the repository")
	}

	testreg.DockerConfig(t, g.Host, ghcrUser, ghcrPass)
	d, fs := Push(context.Background(), PushOptions{Index: res.Raw, Repo: repo})
	if len(fs) != 0 || !strings.HasPrefix(d, "sha256:") {
		t.Fatalf("push to a never-created repository with push-capable credentials: %q %v", d, fs)
	}
	if !g.Exists("catalog") {
		t.Fatal("the push must have created the repository")
	}
	want := "repository:catalog:push,pull"
	found := false
	for _, s := range g.ScopesRequested() {
		found = found || s == want
	}
	if !found {
		t.Fatalf("the existence check must use the push scope; requested %v", g.ScopesRequested())
	}
	// A second push of the same tag is now refused, with the same credentials.
	if _, fs := Push(context.Background(), PushOptions{Index: res.Raw, Repo: repo}); codesOf(fs) != "tag_exists" {
		t.Fatalf("%q", codesOf(fs))
	}
}

func TestPromote(t *testing.T) {
	testreg.NoDockerConfig(t)
	reg := testreg.New(t)
	signer := testreg.NewSigner(t)
	c1 := testreg.PushCandidate(t, reg.Host+"/staging/hello", manifestFor(t, "hello", "0.1.0"))
	repoStr := reg.Host + "/catalog"
	repo, _ := name.NewRepository(repoStr)
	pushAt := func(now string) string {
		res := buildOnly(t, catalogFor(repoStr, reg.Host+"/packs", ver("0.1.0", c1, "2026-10-09")), now)
		d, fs := Push(context.Background(), PushOptions{Index: res.Raw, Repo: "oci://" + repoStr})
		if len(fs) != 0 {
			t.Fatal(fs)
		}
		return d
	}
	latest := func() string {
		d, err := remote.Head(repo.Tag("latest"))
		if err != nil {
			return ""
		}
		return d.Digest.String()
	}
	promote := func(d string, key []byte, tag string) []Finding {
		_, fs := Promote(context.Background(), PromoteOptions{Repo: "oci://" + repoStr, Digest: d, Tag: tag, KeyPEM: key})
		return fs
	}

	d1 := pushAt("2026-10-09T10:00:00Z")
	if fs := promote(d1, []byte(signer.PublicPEM), ""); codesOf(fs) != "index_unsigned" || latest() != "" {
		t.Fatalf("an unsigned digest must not be promoted: %q latest=%q", codesOf(fs), latest())
	}
	signer.Sign(t, repoStr+"@"+d1)
	other := testreg.NewSigner(t)
	if fs := promote(d1, []byte(other.PublicPEM), ""); codesOf(fs) != "index_untrusted_signer" || latest() != "" {
		t.Fatalf("a digest signed by another key must not be promoted: %q", codesOf(fs))
	}
	if fs := promote(d1, []byte(signer.PublicPEM), ""); len(fs) != 0 || latest() != d1 {
		t.Fatalf("a correctly signed digest moves latest: %v latest=%q want %q", fs, latest(), d1)
	}

	// A newer index that nobody signed leaves latest where it was.
	d2 := pushAt("2026-10-09T11:00:00Z")
	if fs := promote(d2, []byte(signer.PublicPEM), ""); codesOf(fs) != "index_unsigned" || latest() != d1 {
		t.Fatalf("latest must stay at the signed digest: %q latest=%q", codesOf(fs), latest())
	}
	// A signature that names a different digest never transfers.
	payload := testreg.Payload(t, repoStr, d1)
	testreg.PushSignatureLayers(t, repoStr+"@"+d2, []testreg.SignatureLayer{signer.SignPayload(t, payload)})
	if fs := promote(d2, []byte(signer.PublicPEM), ""); codesOf(fs) != "index_untrusted_signer" || latest() != d1 {
		t.Fatalf("a signature for another digest must not transfer: %q", codesOf(fs))
	}
	// Immutable-looking tags and malformed digests are refused.
	if fs := promote(d1, []byte(signer.PublicPEM), "20261009T100000Z"); codesOf(fs) != "promote_immutable_tag" {
		t.Fatalf("%q", codesOf(fs))
	}
	if fs := promote("sha256:abc", []byte(signer.PublicPEM), ""); codesOf(fs) != "digest_invalid" {
		t.Fatalf("%q", codesOf(fs))
	}
	// A signed digest that is not an index artifact is not promoted either.
	notIndex := testreg.PushImage(t, repoStr, nil)
	signer.Sign(t, notIndex)
	if fs := promote(digestOfRef(notIndex), []byte(signer.PublicPEM), ""); len(fs) == 0 || latest() != d1 {
		t.Fatalf("a signed non-index must not be promoted: %q", codesOf(fs))
	}
}
