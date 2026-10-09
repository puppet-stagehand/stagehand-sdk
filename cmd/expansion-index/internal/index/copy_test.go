package index

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/testreg"
)

// writeCounter counts the requests that change a registry.
type writeCounter struct {
	base   http.RoundTripper
	mu     sync.Mutex
	writes int
}

func (w *writeCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.mu.Lock()
		w.writes++
		w.mu.Unlock()
	}
	return w.base.RoundTrip(req)
}

func (w *writeCounter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

func basic(u, p string) authn.Authenticator { return &authn.Basic{Username: u, Password: p} }

// headDigest reads the digest repo:tag (or repo@digest) resolves to.
func headDigest(t *testing.T, ref string, a authn.Authenticator) (string, error) {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	d, err := remote.Head(r, remote.WithAuth(a))
	if err != nil {
		return "", err
	}
	return d.Digest.String(), nil
}

func TestCopyBetweenRegistries(t *testing.T) {
	src := testreg.NewBasicAuth(t, "srcuser", "srcpass")
	dst := testreg.NewBasicAuth(t, "dstuser", "dstpass")
	srcAuth, dstAuth := basic("srcuser", "srcpass"), basic("dstuser", "dstpass")
	candidate := testreg.PushCandidate(t, src.Host+"/staging/hello", manifestFor(t, "hello", "0.1.0"), remote.WithAuth(srcAuth))
	want := digestOfRef(candidate)

	wc := &writeCounter{base: remote.DefaultTransport}
	o := CopyOptions{From: candidate, To: dst.Host + "/packs/hello:0.1.0", FromAuth: srcAuth, ToAuth: dstAuth, Transport: wc}
	res, fs := Copy(context.Background(), o)
	if len(fs) > 0 {
		t.Fatalf("first copy: %v", fs)
	}
	if res.Digest != want || res.AlreadyPresent {
		t.Fatalf("first copy result %+v, want digest %s and already_present false", res, want)
	}
	got, err := headDigest(t, dst.Host+"/packs/hello:0.1.0", dstAuth)
	if err != nil || got != want {
		t.Fatalf("destination tag resolves to %q (%v), want the source digest %s", got, err, want)
	}
	if got, err := headDigest(t, dst.Host+"/packs/hello@"+want, dstAuth); err != nil || got != want {
		t.Fatalf("destination does not hold the digest: %q %v", got, err)
	}
	if wc.count() == 0 {
		t.Fatal("the first copy must have written to the destination")
	}

	before := wc.count()
	res, fs = Copy(context.Background(), o)
	if len(fs) > 0 {
		t.Fatalf("second copy: %v", fs)
	}
	if res.Digest != want || !res.AlreadyPresent {
		t.Fatalf("second copy result %+v, want already_present true", res)
	}
	if wc.count() != before {
		t.Fatalf("an already-present digest must write nothing: %d write requests the second time", wc.count()-before)
	}
}

func TestCopyRefusesTagPinnedSource(t *testing.T) {
	reg := testreg.New(t)
	candidate := testreg.PushCandidate(t, reg.Host+"/staging/hello", manifestFor(t, "hello", "0.1.0"))
	repo := candidate[:strings.Index(candidate, "@")]
	for _, from := range []string{repo + ":candidate", repo, "oci://" + repo + ":candidate"} {
		res, fs := Copy(context.Background(), CopyOptions{From: from, To: reg.Host + "/packs/hello:0.1.0"})
		if res != nil || codesOfFindings(fs) != "source_not_pinned" {
			t.Fatalf("From %q: result %v findings %s", from, res, codesOfFindings(fs))
		}
		if fs[0].Fix == "" {
			t.Fatalf("finding without a fix line: %v", fs[0])
		}
	}
	// Nothing was written to the destination.
	if _, err := headDigest(t, reg.Host+"/packs/hello:0.1.0", authn.Anonymous); err == nil {
		t.Fatal("a refused copy wrote to the destination")
	}
}

func codesOfFindings(fs []Finding) string {
	var c []string
	for _, f := range fs {
		c = append(c, f.Code)
	}
	return strings.Join(c, ",")
}

// rewritingRegistry is an in-process registry whose HEAD/GET of a manifest by
// TAG reports a digest other than the stored one, as a registry that rewrote
// the manifest on the way in would.
func rewritingRegistry(t *testing.T) (host, badDigest string) {
	t.Helper()
	badDigest = "sha256:" + strings.Repeat("e", 64)
	inner := registry.New(registry.Logger(log.New(&strings.Builder{}, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		i := strings.LastIndex(req.URL.Path, "/manifests/")
		if i >= 0 && (req.Method == http.MethodHead || req.Method == http.MethodGet) && !strings.HasPrefix(req.URL.Path[i+len("/manifests/"):], "sha256:") {
			w.Header().Set("Docker-Content-Digest", badDigest)
		}
		inner.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), badDigest
}

func TestCopyDigestMismatch(t *testing.T) {
	src := testreg.New(t)
	candidate := testreg.PushCandidate(t, src.Host+"/staging/hello", manifestFor(t, "hello", "0.1.0"))
	dstHost, bad := rewritingRegistry(t)
	res, fs := Copy(context.Background(), CopyOptions{From: candidate, To: dstHost + "/packs/hello:0.1.0"})
	if res != nil || codesOfFindings(fs) != "digest_mismatch" {
		t.Fatalf("result %v findings %s; a destination whose digest differs from the source must fail", res, codesOfFindings(fs))
	}
	if !strings.Contains(fs[0].Message, bad) || !strings.Contains(fs[0].Message, digestOfRef(candidate)) {
		t.Fatalf("the finding must name both digests: %s", fs[0].Message)
	}
}

func TestCopyNeverCreatedDestination(t *testing.T) {
	src := testreg.New(t)
	candidate := testreg.PushCandidate(t, src.Host+"/staging/hello", manifestFor(t, "hello", "0.1.0"))
	want := digestOfRef(candidate)

	ghcr := testreg.NewGHCRLike(t, "pusher", "pushpass")
	ghcr.AddReadOnlyUser("reader", "readpass")
	const repo = "puppet-stagehand/packs/hello"
	if ghcr.Exists(repo) {
		t.Fatal("precondition: the package must not exist yet")
	}

	// Read-only credentials: the push scope is refused and nothing is created.
	res, fs := Copy(context.Background(), CopyOptions{From: candidate, To: ghcr.Host + "/" + repo + ":0.1.0", ToAuth: ghcr.ReadOnlyAuth()})
	if res != nil || codesOfFindings(fs) != "destination_denied" {
		t.Fatalf("read-only credentials: result %v findings %s, want destination_denied", res, codesOfFindings(fs))
	}
	if !strings.Contains(fs[0].Message, ghcr.Host+"/"+repo) || fs[0].Fix == "" {
		t.Fatalf("the finding must name the destination and carry a fix: %v", fs[0])
	}
	if ghcr.Exists(repo) {
		t.Fatal("a denied copy created the package")
	}

	// Push-capable credentials create it.
	res, fs = Copy(context.Background(), CopyOptions{From: candidate, To: ghcr.Host + "/" + repo + ":0.1.0", ToAuth: ghcr.Auth()})
	if len(fs) > 0 {
		t.Fatalf("copy to a never-created package: %v", fs)
	}
	if res.Digest != want || res.AlreadyPresent {
		t.Fatalf("result %+v, want digest %s and already_present false", res, want)
	}
	if !ghcr.Exists(repo) {
		t.Fatal("the copy did not create the package")
	}
	// The existence check asked for the push scope (a pull-only token is
	// refused for a package that does not exist yet).
	var pushScoped bool
	for _, s := range ghcr.ScopesRequested() {
		if s == "repository:"+repo+":push,pull" {
			pushScoped = true
		}
	}
	if !pushScoped {
		t.Fatalf("no push,pull token was requested for the destination: %v", ghcr.ScopesRequested())
	}

	// Now that it exists, an identical copy is a no-op even with the same credentials.
	res, fs = Copy(context.Background(), CopyOptions{From: candidate, To: ghcr.Host + "/" + repo + ":0.1.0", ToAuth: ghcr.Auth()})
	if len(fs) > 0 || !res.AlreadyPresent {
		t.Fatalf("second copy: %+v %v", res, fs)
	}
	// And read-only credentials still cannot push a different image into it.
	other := testreg.PushCandidate(t, src.Host+"/staging/hello2", manifestFor(t, "hello", "0.2.0"))
	res, fs = Copy(context.Background(), CopyOptions{From: other, To: ghcr.Host + "/" + repo + ":0.2.0", ToAuth: ghcr.ReadOnlyAuth()})
	if res != nil || codesOfFindings(fs) != "destination_denied" {
		t.Fatalf("read-only credentials on an existing package: result %v findings %s", res, codesOfFindings(fs))
	}
}

func TestCopyDeniedOnEitherSide(t *testing.T) {
	src := testreg.NewBasicAuth(t, "srcuser", "srcpass")
	dst := testreg.NewBasicAuth(t, "dstuser", "dstpass")
	srcAuth, dstAuth := basic("srcuser", "srcpass"), basic("dstuser", "dstpass")
	candidate := testreg.PushCandidate(t, src.Host+"/staging/hello", manifestFor(t, "hello", "0.1.0"), remote.WithAuth(srcAuth))
	to := dst.Host + "/packs/hello:0.1.0"

	res, fs := Copy(context.Background(), CopyOptions{From: candidate, To: to, FromAuth: basic("srcuser", "wrong"), ToAuth: dstAuth})
	if res != nil || codesOfFindings(fs) != "source_denied" {
		t.Fatalf("wrong source credentials: result %v findings %s", res, codesOfFindings(fs))
	}
	res, fs = Copy(context.Background(), CopyOptions{From: candidate, To: to, FromAuth: srcAuth, ToAuth: basic("dstuser", "wrong")})
	if res != nil || codesOfFindings(fs) != "destination_denied" {
		t.Fatalf("wrong destination credentials: result %v findings %s", res, codesOfFindings(fs))
	}
	missing := src.Host + "/staging/nonesuch@sha256:" + strings.Repeat("9", 64)
	res, fs = Copy(context.Background(), CopyOptions{From: missing, To: to, FromAuth: srcAuth, ToAuth: dstAuth})
	if res != nil || codesOfFindings(fs) != "source_not_found" {
		t.Fatalf("missing source: result %v findings %s", res, codesOfFindings(fs))
	}
}

func TestCopyRefusesABadDestination(t *testing.T) {
	reg := testreg.New(t)
	candidate := testreg.PushCandidate(t, reg.Host+"/staging/hello", manifestFor(t, "hello", "0.1.0"))
	for _, to := range []string{reg.Host + "/packs/hello", reg.Host + "/packs/hello@sha256:" + strings.Repeat("1", 64), "not a ref"} {
		res, fs := Copy(context.Background(), CopyOptions{From: candidate, To: to})
		if res != nil || codesOfFindings(fs) != "destination_invalid" {
			t.Fatalf("To %q: result %v findings %s", to, res, codesOfFindings(fs))
		}
	}
}
