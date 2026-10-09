// Package testreg is the test support shared by the expansion-index command and
// package tests: an in-process OCI registry (optionally requiring basic
// auth), a test-only cosign legacy signer, and helpers that push candidate
// pack images. It is a non-test package so both main_test.go and the internal
// packages' tests can import it; nothing in the shipped binary does.
//
// The signer is a port of the console's keyring_testdata_test.go: it hand-builds
// a cosign "legacy" signature artifact (sha256-<hex>.sig, one layer per
// signature, simple-signing payload, base64 DER ECDSA over sha256(payload) in
// the dev.cosignproject.cosign/signature annotation) with a minted P-256 key,
// so no cosign binary and no network are needed.
package testreg

import (
	"archive/tar"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// ManifestPath is where a pack image keeps its manifest.
const ManifestPath = "stagehand/manifest.json"

// Registry is an in-process registry.
type Registry struct {
	// Host is host:port, no scheme (ggcr treats loopback as plain http).
	Host string
	srv  *httptest.Server
}

// URL returns the registry's http base URL.
func (r *Registry) URL() string { return r.srv.URL }

func quietRegistry() http.Handler {
	return registry.New(registry.Logger(log.New(io.Discard, "", 0)))
}

// New starts an anonymous in-process registry.
func New(t testing.TB) *Registry {
	t.Helper()
	return start(t, quietRegistry())
}

// NewBasicAuth starts an in-process registry that answers every request
// without the right basic credentials with 401 and a Basic challenge.
func NewBasicAuth(t testing.TB, user, pass string) *Registry {
	t.Helper()
	inner := quietRegistry()
	return start(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		u, p, ok := req.BasicAuth()
		if !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="testreg"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, req)
	}))
}

func start(t testing.TB, h http.Handler) *Registry {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Registry{Host: strings.TrimPrefix(srv.URL, "http://"), srv: srv}
}

// File is one file of a test image layer.
type File struct {
	Path string
	Data []byte
}

// TarLayer builds a gzip-compressed tar layer holding files.
func TarLayer(t testing.TB, files ...File) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.Path, Mode: 0o644, Size: int64(len(f.Data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write(f.Data); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	raw := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil })
	if err != nil {
		t.Fatalf("layer: %v", err)
	}
	return layer
}

// PushImage pushes an image made of layers (bottom first) to repoRef
// (host/repo) under the tag "candidate" and returns the digest-pinned
// reference host/repo@sha256:... .
func PushImage(t testing.TB, repoRef string, layers []v1.Layer, opts ...remote.Option) string {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layers...)
	if err != nil {
		t.Fatalf("append layers: %v", err)
	}
	tag, err := name.NewTag(repoRef + ":candidate")
	if err != nil {
		t.Fatalf("tag %q: %v", repoRef, err)
	}
	if err := remote.Write(tag, img, opts...); err != nil {
		t.Fatalf("push %s: %v", tag, err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return repoRef + "@" + d.String()
}

// PushCandidate pushes a pack image whose single layer holds manifestJSON at
// stagehand/manifest.json, and returns host/repo@sha256:... .
func PushCandidate(t testing.TB, repoRef string, manifestJSON []byte, opts ...remote.Option) string {
	t.Helper()
	return PushImage(t, repoRef, []v1.Layer{TarLayer(t, File{Path: ManifestPath, Data: manifestJSON})}, opts...)
}

// Signer is a test-only cosign legacy signer with a minted P-256 key.
type Signer struct {
	Key *ecdsa.PrivateKey
	// PublicPEM is the PKIX PEM of the public key (what --key takes).
	PublicPEM string
}

// NewSigner mints a key pair.
func NewSigner(t testing.TB) *Signer {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return &Signer{Key: priv, PublicPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}
}

// WriteKey writes pem to dir/name and returns the path.
func WriteKey(t testing.TB, dir, fileName, pemText string) string {
	t.Helper()
	p := filepath.Join(dir, fileName)
	if err := os.WriteFile(p, []byte(pemText), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return p
}

type simpleSigning struct {
	Critical struct {
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
		Identity struct {
			DockerReference string `json:"docker-reference"`
		} `json:"identity"`
		Type string `json:"type"`
	} `json:"critical"`
}

// Payload builds cosign's simple-signing payload naming digest.
func Payload(t testing.TB, dockerRef, digest string) []byte {
	t.Helper()
	var doc simpleSigning
	doc.Critical.Image.DockerManifestDigest = digest
	doc.Critical.Identity.DockerReference = dockerRef
	doc.Critical.Type = "cosign container image signature"
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	return raw
}

// SignatureLayer is one layer of a hand-built signature artifact.
type SignatureLayer struct {
	Payload []byte
	SigB64  string
}

// Sign signs the digest-pinned image reference imageRef (host/repo@sha256:...)
// and pushes the sha256-<hex>.sig artifact next to it.
func (s *Signer) Sign(t testing.TB, imageRef string, opts ...remote.Option) {
	t.Helper()
	d, err := name.NewDigest(imageRef, name.StrictValidation)
	if err != nil {
		t.Fatalf("digest ref %q: %v", imageRef, err)
	}
	payload := Payload(t, d.Context().String(), d.DigestStr())
	PushSignatureLayers(t, imageRef, []SignatureLayer{s.SignPayload(t, payload)}, opts...)
}

// SignPayload signs payload and returns the signature layer.
func (s *Signer) SignPayload(t testing.TB, payload []byte) SignatureLayer {
	t.Helper()
	sum := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, s.Key, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return SignatureLayer{Payload: payload, SigB64: base64.StdEncoding.EncodeToString(sig)}
}

// SigTag returns the cosign legacy signature tag reference for imageRef.
func SigTag(t testing.TB, imageRef string) name.Tag {
	t.Helper()
	d, err := name.NewDigest(imageRef, name.StrictValidation)
	if err != nil {
		t.Fatalf("digest ref %q: %v", imageRef, err)
	}
	return d.Context().Tag(fmt.Sprintf("sha256-%s.sig", strings.TrimPrefix(d.DigestStr(), "sha256:")))
}

// PushSignatureLayers pushes a signature artifact for imageRef with exactly
// layers, in order.
func PushSignatureLayers(t testing.TB, imageRef string, layers []SignatureLayer, opts ...remote.Option) {
	t.Helper()
	var adds []mutate.Addendum
	for _, l := range layers {
		adds = append(adds, mutate.Addendum{
			Layer:       static.NewLayer(l.Payload, types.MediaType("application/vnd.dev.cosign.simplesigning.v1+json")),
			Annotations: map[string]string{"dev.cosignproject.cosign/signature": l.SigB64},
		})
	}
	img, err := mutate.Append(empty.Image, adds...)
	if err != nil {
		t.Fatalf("build signature image: %v", err)
	}
	if err := remote.Write(SigTag(t, imageRef), img, opts...); err != nil {
		t.Fatalf("push signature: %v", err)
	}
}

// GHCRLike is an in-process registry fronted by a token service that replays
// what ghcr.io answered on 2026-10-09 (57-RESEARCH.md Pitfall 1), plus one
// deliberately pessimistic model for the case that could not be probed
// anonymously.
//
// Verified against ghcr.io (anonymous probe):
//
//   - GET /v2/ and a manifest read with no token answer 401 with a Bearer
//     challenge naming the token endpoint.
//   - An anonymous token request for a repository that does not exist answers
//     exactly like one for an existing private repository: 403
//     {"errors":[{"code":"DENIED","message":"requested access to the resource is denied"}]}.
//   - An anonymous token for an existing public repository is granted, and a
//     missing tag then answers 404 MANIFEST_UNKNOWN.
//
// Modelled, not probed (the first live publish, plan 57-11, confirms it): a
// basic-auth token request with the right credentials and the push,pull scope
// on a repository that does not exist is granted, and a read of it then
// answers 404 NAME_UNKNOWN. The same credentials with pull scope only on a
// missing repository answer 403 DENIED, so no test passes by relying on GHCR
// granting pull for a missing repository. Wrong credentials answer 401 at the
// token endpoint. A repository comes into existence when a manifest is
// successfully PUT; it is private until SetPublic.
type GHCRLike struct {
	// Host is host:port, no scheme.
	Host string

	user, pass string
	inner      http.Handler

	mu     sync.Mutex
	repos  map[string]bool // name -> public
	tokens map[string]map[string]map[string]bool
	scopes []string
}

// NewGHCRLike starts the registry. user/pass are the only credentials the
// token service accepts.
func NewGHCRLike(t testing.TB, user, pass string) *GHCRLike {
	t.Helper()
	g := &GHCRLike{user: user, pass: pass, inner: quietRegistry(), repos: map[string]bool{}, tokens: map[string]map[string]map[string]bool{}}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	g.Host = strings.TrimPrefix(srv.URL, "http://")
	return g
}

// Auth returns the registry's valid credentials.
func (g *GHCRLike) Auth() authn.Authenticator {
	return &authn.Basic{Username: g.user, Password: g.pass}
}

// SetPublic makes an existing repository public (or private again).
func (g *GHCRLike) SetPublic(repo string, public bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.repos[repo] = public
}

// Exists reports whether the repository has been created.
func (g *GHCRLike) Exists(repo string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.repos[repo]
	return ok
}

// ScopesRequested returns every scope string the token endpoint was asked for
// (for example "repository:catalog:push,pull"), in order.
func (g *GHCRLike) ScopesRequested() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.scopes...)
}

var repoPathRe = regexp.MustCompile(`^/v2/(.+)/(manifests|blobs|tags)/`)

func (g *GHCRLike) challenge(w http.ResponseWriter, req *http.Request, scope string) {
	h := fmt.Sprintf(`Bearer realm="http://%s/token",service="ghcr.io"`, req.Host)
	if scope != "" {
		h += fmt.Sprintf(`,scope="%s"`, scope)
	}
	w.Header().Set("WWW-Authenticate", h)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`))
}

func writeErrors(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(c int) { s.status = c; s.ResponseWriter.WriteHeader(c) }

func (g *GHCRLike) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/token" {
		g.serveToken(w, req)
		return
	}
	grants := g.grantsFor(req)
	if req.URL.Path == "/v2/" || req.URL.Path == "/v2" {
		if grants == nil {
			g.challenge(w, req, "")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
		return
	}
	m := repoPathRe.FindStringSubmatch(req.URL.Path)
	if m == nil {
		http.NotFound(w, req)
		return
	}
	repo := m[1]
	action := "pull"
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		action = "push"
	}
	if !grants[repo][action] {
		g.challenge(w, req, fmt.Sprintf("repository:%s:%s", repo, action))
		return
	}
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	g.inner.ServeHTTP(sw, req)
	if m[2] == "manifests" && req.Method == http.MethodPut && sw.status >= 200 && sw.status < 300 {
		g.mu.Lock()
		if _, ok := g.repos[repo]; !ok {
			g.repos[repo] = false
		}
		g.mu.Unlock()
	}
}

func (g *GHCRLike) grantsFor(req *http.Request) map[string]map[string]bool {
	auth := req.Header.Get("Authorization")
	tok := strings.TrimPrefix(auth, "Bearer ")
	if tok == auth || tok == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if gr, ok := g.tokens[tok]; ok {
		return gr
	}
	return nil
}

func (g *GHCRLike) serveToken(w http.ResponseWriter, req *http.Request) {
	authed := false
	if u, p, ok := req.BasicAuth(); ok {
		if u != g.user || p != g.pass {
			writeErrors(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		authed = true
	}
	grants := map[string]map[string]bool{}
	g.mu.Lock()
	for _, sc := range req.URL.Query()["scope"] {
		g.scopes = append(g.scopes, sc)
		parts := strings.Split(sc, ":")
		if len(parts) != 3 || parts[0] != "repository" {
			g.mu.Unlock()
			writeErrors(w, http.StatusBadRequest, "UNSUPPORTED", "unsupported scope")
			return
		}
		repo := parts[1]
		want := map[string]bool{}
		for _, a := range strings.Split(parts[2], ",") {
			want[a] = true
		}
		public, exists := g.repos[repo]
		ok := false
		switch {
		case authed && exists:
			ok = true
		case authed && !exists:
			ok = want["push"] // a repository the caller may create; pull alone is not enough (pessimistic model)
		case !authed && exists && public:
			ok = want["pull"] && !want["push"]
		}
		if !ok {
			g.mu.Unlock()
			writeErrors(w, http.StatusForbidden, "DENIED", "requested access to the resource is denied")
			return
		}
		grants[repo] = want
	}
	var raw [12]byte
	_, _ = rand.Read(raw[:])
	tok := hex.EncodeToString(raw[:])
	g.tokens[tok] = grants
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "access_token": tok, "expires_in": 300})
}

// DockerConfig points the default keychain (DOCKER_CONFIG) at a fresh docker
// config that logs host in with user/pass, for the duration of the test.
func DockerConfig(t testing.TB, host, user, pass string) {
	t.Helper()
	dir := t.TempDir()
	cfg := map[string]any{"auths": map[string]any{host: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))}}}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
}

// NoDockerConfig points the default keychain at an empty docker config.
func NoDockerConfig(t testing.TB) { t.Setenv("DOCKER_CONFIG", t.TempDir()) }

// PushIndexArtifact writes raw as a one-layer index artifact at ref (a tag
// reference), without any of Push's checks, so tests can plant documents a
// well-behaved publisher never would.
func PushIndexArtifact(t testing.TB, ref string, raw []byte, opts ...remote.Option) string {
	t.Helper()
	img, err := mutate.Append(empty.Image, mutate.Addendum{Layer: static.NewLayer(raw, types.MediaType("application/vnd.stagehand.expansion-index.v1+json"))})
	if err != nil {
		t.Fatal(err)
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.MediaType("application/vnd.stagehand.expansion-index.config.v1+json"))
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img, opts...); err != nil {
		t.Fatalf("push %s: %v", ref, err)
	}
	d, _ := img.Digest()
	return d.String()
}
