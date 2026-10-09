package listing

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/catalogfile"
	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/index"
	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/testreg"
)

// The golden file holds the values a live publish would show on the page. The
// registry host, the key and the digests of a test run are random or depend on
// the run, so the test checks each of them against an independent source and
// then swaps it for the fixed value below before comparing with the golden.
// These fixed values are public: the Marquee publisher key is published on the
// Marquee page, and the digests are the placeholders of the docs site's own
// fixture.
const (
	fixedFingerprint = "sha256:48ba24e6f91da7ced7540b1512d0eac37b7fa070d474d28b9f8a89a0c5955242"
	fixedPEM         = "-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAERzD9kZTJx2uOrQdybFiHNxagf+Ie\nW1NVUaOwcNILUpQmRMKbLVtmPrqAgvJygWS5KCnuaiSiiaoTWlNI1KP9vw==\n-----END PUBLIC KEY-----\n"
	fixedIndex       = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	fixedImage       = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	goldenPath       = "../../testdata/listing/marquee-golden.json"
	privateHost      = "harbor.private.invalid:5000"
)

// freeFeedYAML is the public feed; paidYAML adds a private feed with one pack
// that has a listing block and a staging reference on its own registry host.
func catalogYAML(host string, withPaid bool) string {
	s := `format: 1
feeds:
  free:
    index: ` + host + `/puppet-stagehand/catalog
    images: ` + host + `/puppet-stagehand/packs
    visibility: public
`
	if withPaid {
		s += `  paid:
    index: ` + privateHost + `/paid/catalog
    images: ` + privateHost + `/paid/packs
    visibility: private
`
	}
	s += `packs:
  - id: hello
    feed: free
    versions:
      - version: 0.1.0
        candidate: ` + host + `/staging/hello@sha256:` + strings.Repeat("a", 64) + `
        released_at: 2026-10-09
`
	if withPaid {
		s += `  - id: puppet_paid
    feed: paid
    listing:
      name: Puppet Paid Add-on
      summary: A paid add-on used only by tests.
      tier: ent
    versions:
      - version: 2.0.0
        candidate: ` + privateHost + `/staging/paid@sha256:` + strings.Repeat("b", 64) + `
        released_at: 2026-08-01
      - version: 2.1.0
        candidate: ` + privateHost + `/staging/paid@sha256:` + strings.Repeat("c", 64) + `
        released_at: 2026-09-15
`
	}
	return s
}

type env struct {
	reg         *testreg.Registry
	signer      *testreg.Signer
	keyPEM      []byte
	catalog     *catalogfile.File
	indexRef    string // oci://host/repo@sha256:...
	indexDigest string
	imageDigest string
	idx         *index.Index
}

// newEnv publishes a signed (or, with sign false, unsigned) index for the
// catalog onto an in-process registry.
func newEnv(t *testing.T, withPaid, sign bool) *env {
	t.Helper()
	reg := testreg.New(t)
	signer := testreg.NewSigner(t)

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "examples", "hello", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	candidate := testreg.PushCandidate(t, reg.Host+"/staging/hello", raw)
	imageDigest := candidate[strings.Index(candidate, "@")+1:]

	yaml := strings.Replace(catalogYAML(reg.Host, withPaid), strings.Repeat("a", 64), strings.TrimPrefix(imageDigest, "sha256:"), 1)
	cat, fs := catalogfile.Decode([]byte(yaml))
	if len(fs) > 0 {
		t.Fatalf("catalog findings: %v", fs)
	}
	res, bfs := index.Build(context.Background(), index.BuildOptions{
		Catalog: cat, Feed: "free", Now: mustTime(t, "2026-10-09T14:30:12Z"), KeyPEM: []byte(signer.PublicPEM),
	})
	if len(bfs) > 0 {
		t.Fatalf("build findings: %v", bfs)
	}
	repo := reg.Host + "/puppet-stagehand/catalog"
	digest := testreg.PushIndexArtifact(t, repo+":"+res.Tag, res.Raw)
	if sign {
		signer.Sign(t, repo+"@"+digest)
	}
	return &env{
		reg: reg, signer: signer, keyPEM: []byte(signer.PublicPEM), catalog: cat,
		indexRef: "oci://" + repo + "@" + digest, indexDigest: digest, imageDigest: imageDigest, idx: res.Index,
	}
}

func (e *env) options() Options {
	return Options{IndexRef: e.indexRef, KeyPEM: e.keyPEM, Catalog: e.catalog, Feed: "free"}
}

func (e *env) build(t *testing.T) []byte {
	t.Helper()
	doc, fs := Build(context.Background(), e.options())
	if len(fs) > 0 {
		t.Fatalf("listing findings: %v", fs)
	}
	raw, err := Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func codes(fs []catalogfile.Finding) string {
	var c []string
	for _, f := range fs {
		c = append(c, f.Code)
	}
	return strings.Join(c, ",")
}

// asMap decodes the listing JSON for field-level assertions.
func asMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("listing is not JSON: %v\n%s", err, raw)
	}
	return m
}

// normalise swaps the run-dependent values for the fixed ones.
func (e *env) normalise(raw []byte) []byte {
	pem, _ := json.Marshal(string(e.keyPEM))
	fixedPEMJSON, _ := json.Marshal(fixedPEM)
	s := string(raw)
	s = strings.ReplaceAll(s, e.imageDigest, fixedImage)
	s = strings.ReplaceAll(s, e.indexDigest, fixedIndex)
	s = strings.ReplaceAll(s, string(pem), string(fixedPEMJSON))
	s = strings.ReplaceAll(s, e.idx.PublisherKeys[0].ID, fixedFingerprint)
	s = strings.ReplaceAll(s, e.reg.Host, "ghcr.io")
	return []byte(s)
}

func TestListingGolden(t *testing.T) {
	e := newEnv(t, true, true)
	raw := e.build(t)
	m := asMap(t, raw)

	if m["schema_version"] != float64(1) {
		t.Fatalf("schema_version: %v", m["schema_version"])
	}
	if m["generated_at"] != "2026-10-09T14:30:12Z" {
		t.Fatalf("generated_at is the verified index's: %v", m["generated_at"])
	}
	if m["index_digest"] != e.indexDigest {
		t.Fatalf("index_digest %v, want the verified digest %s", m["index_digest"], e.indexDigest)
	}
	if m["feed"] != "oci://"+e.reg.Host+"/puppet-stagehand/catalog" {
		t.Fatalf("feed: %v", m["feed"])
	}
	key := m["publisher_key"].(map[string]any)
	if key["fingerprint"] != e.idx.PublisherKeys[0].ID || !strings.HasPrefix(key["fingerprint"].(string), "sha256:") {
		t.Fatalf("fingerprint %v, want %s (sha256 of the key's DER)", key["fingerprint"], e.idx.PublisherKeys[0].ID)
	}
	if key["pem"] != string(e.keyPEM) {
		t.Fatalf("pem: %v", key["pem"])
	}
	packs := m["packs"].([]any)
	if len(packs) != 1 {
		t.Fatalf("packs: %v", packs)
	}
	p := packs[0].(map[string]any)
	if p["id"] != "hello" || p["name"] != "Hello" || p["publisher"] != "Example Org" || p["tier"] != "core" || p["entitlement"] != "none" || p["licence"] != "Apache-2.0" {
		t.Fatalf("pack fields come from the signed index: %v", p)
	}
	v := p["versions"].([]any)[0].(map[string]any)
	wantImage := e.reg.Host + "/puppet-stagehand/packs/hello@" + e.imageDigest
	if v["version"] != "0.1.0" || v["released_at"] != "2026-10-09" || v["image"] != wantImage || v["digest"] != e.imageDigest {
		t.Fatalf("version: %v", v)
	}

	got := e.normalise(raw)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read the golden (run with UPDATE_GOLDEN=1 once to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("listing differs from %s\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, want)
	}
}

func TestListingFreeOnlyHasAnEmptyPaidList(t *testing.T) {
	e := newEnv(t, false, true)
	raw := e.build(t)
	m := asMap(t, raw)
	paid, ok := m["paid_listings"].([]any)
	if !ok || len(paid) != 0 {
		t.Fatalf("paid_listings must be an empty array, got %#v", m["paid_listings"])
	}
	if !strings.HasSuffix(string(raw), "}\n") {
		t.Fatalf("output must end with one newline: %q", string(raw[max(0, len(raw)-5):]))
	}
}

func TestListingPaidListingCarriesNoImageDigestOrHost(t *testing.T) {
	e := newEnv(t, true, true)
	raw := e.build(t)
	m := asMap(t, raw)
	paid := m["paid_listings"].([]any)
	if len(paid) != 1 {
		t.Fatalf("paid_listings: %v", paid)
	}
	l := paid[0].(map[string]any)
	if l["id"] != "puppet_paid" || l["name"] != "Puppet Paid Add-on" || l["summary"] != "A paid add-on used only by tests." || l["tier"] != "ent" {
		t.Fatalf("paid listing fields come from the listing block: %v", l)
	}
	if l["label"] != "Perforce add-on, licence required" {
		t.Fatalf("label: %v", l["label"])
	}
	if PaidLabel != "Perforce add-on, licence required" {
		t.Fatalf("PaidLabel: %q", PaidLabel)
	}
	vs := l["versions"].([]any)
	if len(vs) != 2 || vs[0].(map[string]any)["version"] != "2.1.0" || vs[0].(map[string]any)["released_at"] != "2026-09-15" || vs[1].(map[string]any)["version"] != "2.0.0" {
		t.Fatalf("versions are newest first with their release dates: %v", vs)
	}
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				switch k {
				case "image", "digest", "host", "candidate", "registry", "index":
					t.Errorf("paid listing has a %q key at %s", k, path)
				}
				walk(c, path+"."+k)
			}
		case []any:
			for i, c := range x {
				walk(c, path+"[]"+string(rune('0'+i)))
			}
		}
	}
	walk(paid, "paid_listings")
	for _, leak := range []string{privateHost, "harbor", strings.Repeat("b", 64), strings.Repeat("c", 64)} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the listing leaks %q of the private feed:\n%s", leak, raw)
		}
	}
}

func TestListingRefusesAnIndexThatDoesNotVerify(t *testing.T) {
	t.Run("unsigned", func(t *testing.T) {
		e := newEnv(t, false, false)
		doc, fs := Build(context.Background(), e.options())
		if doc != nil || !strings.Contains(codes(fs), "index_unsigned") {
			t.Fatalf("an unsigned index must produce findings and no document: doc=%v findings=%s", doc, codes(fs))
		}
		for _, f := range fs {
			if f.Fix == "" {
				t.Fatalf("finding without a fix line: %v", f)
			}
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		e := newEnv(t, false, true)
		o := e.options()
		o.KeyPEM = []byte(testreg.NewSigner(t).PublicPEM)
		doc, fs := Build(context.Background(), o)
		if doc != nil || !strings.Contains(codes(fs), "index_untrusted_signer") {
			t.Fatalf("doc=%v findings=%s", doc, codes(fs))
		}
	})
	t.Run("bad key file", func(t *testing.T) {
		e := newEnv(t, false, true)
		o := e.options()
		o.KeyPEM = []byte("not a key")
		doc, fs := Build(context.Background(), o)
		if doc != nil || !strings.Contains(codes(fs), "key_invalid") {
			t.Fatalf("doc=%v findings=%s", doc, codes(fs))
		}
	})
}

// recorder notes every host the registry client dials.
type recorder struct {
	base  http.RoundTripper
	mu    sync.Mutex
	hosts map[string]int
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.hosts[req.URL.Host]++
	r.mu.Unlock()
	return r.base.RoundTrip(req)
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for h := range r.hosts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// record routes every registry client in the process through a recorder for
// the rest of the test. Tests in this package do not run in parallel.
func record(t *testing.T) *recorder {
	t.Helper()
	rec := &recorder{base: remote.DefaultTransport, hosts: map[string]int{}}
	orig := remote.DefaultTransport
	remote.DefaultTransport = rec
	t.Cleanup(func() { remote.DefaultTransport = orig })
	return rec
}

func TestListingContactsOnlyTheIndexRegistry(t *testing.T) {
	e := newEnv(t, true, true) // the catalog also declares a private feed on its own host
	rec := record(t)
	e.build(t)
	got := rec.seen()
	if len(got) != 1 || got[0] != e.reg.Host {
		t.Fatalf("listing dialled %v; only the index registry %s may be contacted (D-14)", got, e.reg.Host)
	}
}

func TestListingRefusesAPrivateFeedWithoutAnyNetwork(t *testing.T) {
	e := newEnv(t, true, true)
	rec := record(t)
	o := e.options()
	o.Feed = "paid"
	doc, fs := Build(context.Background(), o)
	if doc != nil || !strings.Contains(codes(fs), "listing_feed_private") {
		t.Fatalf("doc=%v findings=%s", doc, codes(fs))
	}
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("a private feed must be refused before any connection, dialled %v", got)
	}
}

func TestListingIndexRefMustBeTheFeedsIndex(t *testing.T) {
	e := newEnv(t, false, true)
	rec := record(t)
	o := e.options()
	o.IndexRef = "oci://" + e.reg.Host + "/someone-else/catalog@" + e.indexDigest
	doc, fs := Build(context.Background(), o)
	if doc != nil || !strings.Contains(codes(fs), "index_ref_mismatch") {
		t.Fatalf("doc=%v findings=%s", doc, codes(fs))
	}
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("a mismatched reference must be refused before any connection, dialled %v", got)
	}
	o.Feed = "nonesuch"
	if _, fs := Build(context.Background(), o); !strings.Contains(codes(fs), "unknown_feed") {
		t.Fatalf("findings=%s", codes(fs))
	}
}

func TestListingPaidListingNeedsASummary(t *testing.T) {
	e := newEnv(t, true, true)
	for i := range e.catalog.Packs {
		if e.catalog.Packs[i].Listing != nil {
			e.catalog.Packs[i].Listing.Summary = ""
		}
	}
	doc, fs := Build(context.Background(), e.options())
	if doc != nil || !strings.Contains(codes(fs), "listing_summary_missing") {
		t.Fatalf("doc=%v findings=%s", doc, codes(fs))
	}
}
