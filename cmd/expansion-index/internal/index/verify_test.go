package index

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/testreg"
)

func TestVerifySignatureCases(t *testing.T) {
	reg := testreg.New(t)
	signer := testreg.NewSigner(t)
	other := testreg.NewSigner(t)
	pub, _ := ParsePublicKey([]byte(signer.PublicPEM))
	keys := []*ecdsa.PublicKey{pub}
	check := func(ref string) error {
		d, _ := name.NewDigest(ref)
		return verifySignature(context.Background(), d.Context(), d.DigestStr(), keys, nil)
	}
	img := func(repo string) string {
		return testreg.PushCandidate(t, reg.Host+"/"+repo, manifestFor(t, "hello", "0.1.0"))
	}

	a := img("a")
	if err := check(a); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("no .sig artifact: %v", err)
	}
	signer.Sign(t, a)
	if err := check(a); err != nil {
		t.Fatalf("signed: %v", err)
	}

	b := img("b")
	other.Sign(t, b)
	if err := check(b); !errors.Is(err, ErrUntrustedSigner) {
		t.Fatalf("signed by another key: %v", err)
	}

	// Two layers: the first for another key, the second valid.
	c := img("c")
	d, _ := name.NewDigest(c)
	payload := testreg.Payload(t, d.Context().String(), d.DigestStr())
	testreg.PushSignatureLayers(t, c, []testreg.SignatureLayer{other.SignPayload(t, payload), signer.SignPayload(t, payload)})
	if err := check(c); err != nil {
		t.Fatalf("any layer may carry the trusted signature: %v", err)
	}

	// A valid signature over a payload naming a different image does not transfer.
	e := img("e")
	otherPayload := testreg.Payload(t, reg.Host+"/e", "sha256:"+strings.Repeat("1", 64))
	testreg.PushSignatureLayers(t, e, []testreg.SignatureLayer{signer.SignPayload(t, otherPayload)})
	if err := check(e); !errors.Is(err, ErrUntrustedSigner) {
		t.Fatalf("signature for another digest: %v", err)
	}

	// A tampered payload under a valid signature of the original.
	f := img("f")
	fd, _ := name.NewDigest(f)
	good := signer.SignPayload(t, testreg.Payload(t, fd.Context().String(), fd.DigestStr()))
	good.Payload = append(good.Payload, ' ')
	testreg.PushSignatureLayers(t, f, []testreg.SignatureLayer{good})
	if err := check(f); !errors.Is(err, ErrUntrustedSigner) {
		t.Fatalf("tampered payload: %v", err)
	}

	// An artifact with more layers than a signature ever has is treated as unsigned.
	g := img("g")
	gd, _ := name.NewDigest(g)
	gp := testreg.Payload(t, gd.Context().String(), gd.DigestStr())
	var many []testreg.SignatureLayer
	for i := 0; i < maxSigLayers+1; i++ {
		many = append(many, signer.SignPayload(t, append(append([]byte(nil), gp...), byte(' '+i))))
	}
	testreg.PushSignatureLayers(t, g, many)
	if err := check(g); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("too many layers: %v", err)
	}

	// A signature layer with no signature annotation is a hard error, not "unsigned".
	h := img("h")
	hd, _ := name.NewDigest(h)
	testreg.PushSignatureLayers(t, h, []testreg.SignatureLayer{{Payload: testreg.Payload(t, hd.Context().String(), hd.DigestStr()), SigB64: ""}})
	if err := check(h); err == nil || errors.Is(err, ErrUnsigned) || errors.Is(err, ErrUntrustedSigner) {
		t.Fatalf("missing annotation: %v", err)
	}
}

// verifyEnv publishes one signed index (listing one pack image) on a GHCR-like registry.
type verifyEnv struct {
	g      *testreg.GHCRLike
	signer *testreg.Signer
	digest string
	image  string
	repo   string
}

func newVerifyEnv(t *testing.T) *verifyEnv {
	t.Helper()
	g := testreg.NewGHCRLike(t, ghcrUser, ghcrPass)
	testreg.DockerConfig(t, g.Host, ghcrUser, ghcrPass)
	// The candidate is pushed straight into the destination repository, as the
	// catalog workflow's copy step leaves it, so that repository exists.
	cand := testreg.PushCandidate(t, g.Host+"/packs/hello", manifestFor(t, "hello", "0.1.0"), remote.WithAuth(g.Auth()))
	res, bfs := Build(context.Background(), BuildOptions{
		Catalog: catalogFor(g.Host+"/catalog", g.Host+"/packs", ver("0.1.0", cand, "2026-10-09")), Feed: "official", Now: at("2026-10-09T10:00:00Z"),
		CandidateRemote: []remote.Option{remote.WithAuth(g.Auth())},
	})
	if len(bfs) != 0 {
		t.Fatal(bfs)
	}
	d, fs := Push(context.Background(), PushOptions{Index: res.Raw, Repo: "oci://" + g.Host + "/catalog"})
	if len(fs) != 0 {
		t.Fatal(fs)
	}
	signer := testreg.NewSigner(t)
	auth := remote.WithAuth(g.Auth())
	signer.Sign(t, g.Host+"/catalog@"+d, auth)
	return &verifyEnv{g: g, signer: signer, digest: d, image: res.Images[0].Destination, repo: "oci://" + g.Host + "/catalog"}
}

func (e *verifyEnv) verify(images bool, expect string, auth bool) (*VerifyResult, []Finding) {
	o := VerifyOptions{Ref: e.repo + "@" + e.digest, KeyPEM: []byte(e.signer.PublicPEM), Images: images, ExpectDigest: expect}
	if auth {
		o.Remote = []remote.Option{remote.WithAuth(e.g.Auth())}
	}
	return Verify(context.Background(), o)
}

func TestVerifyImagesNamesTheUnsignedPack(t *testing.T) {
	e := newVerifyEnv(t)
	res, fs := e.verify(true, "", true)
	if codesOf(fs) != "image_unsigned" || !strings.Contains(fs[0].Message, "pack hello") || !strings.Contains(fs[0].Message, "0.1.0") {
		t.Fatalf("an unsigned listed image must be named: %v", fs)
	}
	if res == nil || len(res.Images) != 1 || res.Images[0].Verified == nil || *res.Images[0].Verified {
		t.Fatalf("result %+v", res)
	}
	e.signer.Sign(t, e.image, remote.WithAuth(e.g.Auth()))
	res, fs = e.verify(true, e.digest, true)
	if len(fs) != 0 || res.Digest != e.digest || res.GeneratedAt != "2026-10-09T10:00:00Z" || !*res.Images[0].Verified {
		t.Fatalf("%v %+v", fs, res)
	}
	// Without --images the image signatures are not checked and "verified" is omitted.
	res, fs = e.verify(false, "", true)
	if len(fs) != 0 || res.Images[0].Verified != nil || res.Images[0].Image != e.image {
		t.Fatalf("%v %+v", fs, res)
	}
}

func TestVerifyExpectDigestMismatch(t *testing.T) {
	e := newVerifyEnv(t)
	_, fs := e.verify(false, "sha256:"+strings.Repeat("2", 64), true)
	if codesOf(fs) != "digest_mismatch" {
		t.Fatalf("%q", codesOf(fs))
	}
}

// The catalog workflow's anonymous gate: a private repository read anonymously is a denied read.
func TestVerifyAnonymousPrivateIsDenied(t *testing.T) {
	e := newVerifyEnv(t)
	for _, ref := range []string{e.repo + ":latest", e.repo + "@" + e.digest} {
		_, fs := Verify(context.Background(), VerifyOptions{Ref: ref, KeyPEM: []byte(e.signer.PublicPEM)})
		if codesOf(fs) != "registry_denied" || !strings.Contains(fs[0].Message, "denied") {
			t.Fatalf("%s: %q", ref, codesOf(fs))
		}
	}
	e.g.SetPublic("catalog", true)
	if _, fs := Verify(context.Background(), VerifyOptions{Ref: e.repo + "@" + e.digest, KeyPEM: []byte(e.signer.PublicPEM)}); len(fs) != 0 {
		t.Fatalf("once public, an anonymous verify passes: %v", fs)
	}
}

func TestVerifyBadKeyAndRef(t *testing.T) {
	if _, fs := Verify(context.Background(), VerifyOptions{Ref: "oci://x.example/y", KeyPEM: []byte("nope")}); codesOf(fs) != "key_invalid" {
		t.Fatalf("%q", codesOf(fs))
	}
	signer := testreg.NewSigner(t)
	if _, fs := Verify(context.Background(), VerifyOptions{Ref: "oci://UPPER/case:", KeyPEM: []byte(signer.PublicPEM)}); codesOf(fs) != "ref_invalid" {
		t.Fatalf("%q", codesOf(fs))
	}
}
