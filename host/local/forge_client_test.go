package local

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// newTestForgeClient starts a TLS httptest server running handler and
// returns a ForgeClient whose http.Client trusts that server's
// certificate (via srv.Client()) — validateForgeBaseURL requires https,
// so a plain httptest.NewServer target would always fail validation.
func newTestForgeClient(t *testing.T, handler http.HandlerFunc) (ForgeEndpoint, ForgeClient) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return ForgeEndpoint{BaseURL: srv.URL}, newHTTPForgeClient(srv.Client())
}

// forgeSeen records one request the fixture server saw. The handler runs on
// the server's goroutines (enrichment is concurrent), so captures go through
// forgeRecorder's mutex rather than bare variables.
type forgeSeen struct {
	path  string
	query url.Values
	auth  string
}

type forgeRecorder struct {
	mu   sync.Mutex
	seen []forgeSeen
}

func (fr *forgeRecorder) record(r *http.Request) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.seen = append(fr.seen, forgeSeen{path: r.URL.Path, query: r.URL.Query(), auth: r.Header.Get("Authorization")})
}

func (fr *forgeRecorder) byPath(path string) []forgeSeen {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	var out []forgeSeen
	for _, s := range fr.seen {
		if s.path == path {
			out = append(out, s)
		}
	}
	return out
}

func TestForgeHTTPSearch_ExactEndpointQueryShapeAndAuth(t *testing.T) {
	rec := &forgeRecorder{}
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v3/modules":
			_, _ = w.Write([]byte(`{
				"pagination": {"limit": 20, "offset": 0, "total": 50},
				"results": [
					{
						"slug": "puppetlabs-apache",
						"name": "apache",
						"owner": {"slug": "puppetlabs"},
						"endorsement": "supported",
						"deprecated_at": null,
						"superseded_by": null,
						"releases": [{"version": "12.0.0", "created_at": "2026-01-02 03:04:05 -0700"}]
					}
				]
			}`))
		case "/v3/releases":
			_, _ = w.Write([]byte(`{
				"pagination": {"limit": 1, "offset": 0, "total": 9},
				"results": [{
					"version": "12.0.0",
					"validation_score": 87,
					"created_at": "2026-01-02 03:04:05 -0700",
					"metadata": {"summary": "Installs, configures, and manages Apache virtual hosts."},
					"tags": ["apache", "web", "httpd"]
				}]
			}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})
	ep.Auth = "Bearer secret-token"

	results, page, err := client.Search(context.Background(), ep, "internal", "apache", &hostv1.Page{Limit: 20})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mods := rec.byPath("/v3/modules")
	if len(mods) != 1 {
		t.Fatalf("expected exactly one /v3/modules request, got %d", len(mods))
	}
	mq := mods[0].query
	if mq.Get("query") != "apache" || mq.Get("limit") != "20" || mq.Get("offset") != "0" {
		t.Fatalf("unexpected module query params: %v", mq)
	}
	if mq.Get("exclude_fields") != forgeModuleExcludeFields || forgeModuleExcludeFields != "current_release" {
		t.Fatalf("expected module request to exclude current_release, got %q", mq.Get("exclude_fields"))
	}
	if mods[0].auth != "Bearer secret-token" {
		t.Fatalf("expected auth header forwarded on the module request, got %q", mods[0].auth)
	}

	rels := rec.byPath("/v3/releases")
	if len(rels) != 1 {
		t.Fatalf("expected exactly one enrichment request, got %d", len(rels))
	}
	rq := rels[0].query
	if rq.Get("module") != "puppetlabs-apache" || rq.Get("limit") != "1" {
		t.Fatalf("unexpected enrichment query params: %v", rq)
	}
	if rq.Get("exclude_fields") != "readme changelog license reference docs" {
		t.Fatalf("expected the trimmed exclusion list on the enrichment request, got %q", rq.Get("exclude_fields"))
	}
	if rels[0].auth != "Bearer secret-token" {
		t.Fatalf("expected auth header forwarded on the enrichment request, got %q", rels[0].auth)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Name != "puppetlabs/apache" {
		t.Fatalf("expected name puppetlabs/apache (namespace/name form), got %q", r.Name)
	}
	if r.Source != "internal" {
		t.Fatalf("expected caller-supplied source stamped on result, got %q", r.Source)
	}
	if r.Version != "12.0.0" {
		t.Fatalf("expected version 12.0.0, got %q", r.Version)
	}
	if r.Endorsement != "supported" || r.QualityScore != 87 || r.Deprecated || r.SupersededBy != "" {
		t.Fatalf("unexpected metadata: %+v", r)
	}
	if r.ReleaseDate == nil {
		t.Fatalf("expected release date to be populated")
	}
	if r.Summary != "Installs, configures, and manages Apache virtual hosts." {
		t.Fatalf("expected summary mapped from metadata.summary, got %q", r.Summary)
	}
	if !reflect.DeepEqual(r.Tags, []string{"apache", "web", "httpd"}) {
		t.Fatalf("expected tags mapped, got %v", r.Tags)
	}
	if page == nil || page.NextCursor != "1" {
		t.Fatalf("expected next cursor 1 (offset 0 + 1 result < total 50), got %+v", page)
	}
}

// TestForgeHTTPSearch_LargeReleaseBodyIsNotRequested reproduces the Phase 7
// defect: Forge embeds every hit's whole current_release in a module page, so
// an everyday keyword returns megabytes. The unslimmed request here is
// answered with a body past the adapter's cap; only a request that excludes
// current_release gets the small page.
func TestForgeHTTPSearch_LargeReleaseBodyIsNotRequested(t *testing.T) {
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v3/modules":
			if r.URL.Query().Get("exclude_fields") != "current_release" {
				pad := strings.Repeat("x", forgeMaxResponseBytes+(1<<20))
				_, _ = w.Write([]byte(`{"pagination":{"limit":20,"offset":0,"total":1},"results":[{"slug":"puppetlabs-sce_windows","name":"sce_windows","owner":{"slug":"puppetlabs"},"current_release":{"version":"1.0.0","metadata":{"summary":"` + pad + `"}}}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"pagination":{"limit":20,"offset":0,"total":1},"results":[{"slug":"puppetlabs-sce_windows","name":"sce_windows","owner":{"slug":"puppetlabs"},"endorsement":"supported"}]}`))
		case "/v3/releases":
			_, _ = w.Write([]byte(`{"pagination":{"limit":1,"offset":0,"total":1},"results":[{"version":"1.0.0","validation_score":null,"created_at":"2026-01-02 03:04:05 -0700","metadata":{"summary":"Windows hardening."},"tags":["security"]}]}`))
		}
	})
	results, _, err := client.Search(context.Background(), ep, "puppet-forge", "security", nil)
	if err != nil {
		t.Fatalf("expected the slim request to succeed, got %v", err)
	}
	if len(results) != 1 || results[0].Summary != "Windows hardening." || results[0].QualityScore != 0 {
		t.Fatalf("unexpected results: %+v", results)
	}
}

// TestForgeHTTPSearchLive exercises the real public registry for keywords
// that returned megabytes (and so failed to decode) before the slim-page fix.
// Opt-in so CI stays hermetic: set STAGEHAND_LIVE_FORGE=1.
func TestForgeHTTPSearchLive(t *testing.T) {
	if os.Getenv("STAGEHAND_LIVE_FORGE") == "" {
		t.Skip("set STAGEHAND_LIVE_FORGE=1 to query the public Forge registry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, keyword := range []string{"security", "hardening"} {
		results, _, err := DefaultForgeClient().Search(ctx, ForgeEndpoint{BaseURL: DefaultForgeBaseURL}, "puppet-forge", keyword, &hostv1.Page{Limit: 20})
		if err != nil {
			t.Fatalf("live search %q: %v", keyword, err)
		}
		if len(results) == 0 {
			t.Fatalf("live search %q returned an empty page", keyword)
		}
		withSummary := 0
		for _, r := range results {
			if r.Summary != "" {
				withSummary++
			}
		}
		if withSummary == 0 {
			t.Fatalf("live search %q: no hit carried a summary (%d hits)", keyword, len(results))
		}
		t.Logf("live search %q: %d hits, %d with summary", keyword, len(results), withSummary)
	}
}

func TestForgeHTTPSearch_NoAuthHeaderForPublicSource(t *testing.T) {
	var gotAuth string
	sawAuth := false
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, sawAuth = r.Header.Get("Authorization"), r.Header.Get("Authorization") != ""
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pagination":{"limit":20,"offset":0,"total":0},"results":[]}`))
	})
	// ep.Auth left empty: the public-registry posture (D-06's default).
	if _, _, err := client.Search(context.Background(), ep, "puppet-forge", "apache", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sawAuth {
		t.Fatalf("expected no Authorization header for an empty-auth endpoint, got %q", gotAuth)
	}
}

func TestForgeHTTPSearch_LastPageHasNoNextCursor(t *testing.T) {
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pagination":{"limit":20,"offset":0,"total":1},"results":[{"slug":"a-b","name":"b","owner":{"slug":"a"}}]}`))
	})
	_, page, err := client.Search(context.Background(), ep, "puppet-forge", "b", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if page == nil || page.NextCursor != "" {
		t.Fatalf("expected no next cursor on the last page, got %+v", page)
	}
}

func TestForgeHTTPSearch_EmptyResultsIsNotNotFound(t *testing.T) {
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pagination":{"limit":20,"offset":0,"total":0},"results":[]}`))
	})
	results, page, err := client.Search(context.Background(), ep, "puppet-forge", "no-such-module-xyz", nil)
	if err != nil {
		t.Fatalf("expected a valid empty page, not an error: %v", err)
	}
	if len(results) != 0 || page == nil || page.NextCursor != "" {
		t.Fatalf("expected empty results with no next cursor, got results=%v page=%+v", results, page)
	}
}

func TestForgeHTTPSearch_MalformedJSONIsInternal(t *testing.T) {
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not valid json`))
	})
	if _, _, err := client.Search(context.Background(), ep, "puppet-forge", "apache", nil); status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal for malformed JSON body, got %v", err)
	}
}

func TestForgeHTTPSearch_MalformedResultMissingRequiredFieldsIsInternal(t *testing.T) {
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A result with no slug/name/owner — a schema violation, not a
		// legitimately empty optional field.
		_, _ = w.Write([]byte(`{"pagination":{"limit":20,"offset":0,"total":1},"results":[{}]}`))
	})
	if _, _, err := client.Search(context.Background(), ep, "puppet-forge", "apache", nil); status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal for a malformed result shape, got %v", err)
	}
}

func TestForgeHTTPClient_StatusClassification(t *testing.T) {
	cases := []struct {
		name         string
		statusCode   int
		body         string
		wantNotFound bool
		wantCode     codes.Code
	}{
		{name: "not_found_is_typed", statusCode: http.StatusNotFound, body: `{"errors":["not found"]}`, wantNotFound: true},
		{name: "rate_limited_is_unavailable", statusCode: http.StatusTooManyRequests, body: `{}`, wantCode: codes.Unavailable},
		{name: "server_error_is_unavailable", statusCode: http.StatusInternalServerError, body: `{}`, wantCode: codes.Unavailable},
		{name: "bad_gateway_is_unavailable", statusCode: http.StatusBadGateway, body: `{}`, wantCode: codes.Unavailable},
		{name: "bad_request_is_internal", statusCode: http.StatusBadRequest, body: `{}`, wantCode: codes.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := client.ListReleases(context.Background(), ep, "puppetlabs/apache")
			if tc.wantNotFound {
				if !errors.Is(err, ErrForgeNotFound) {
					t.Fatalf("expected ErrForgeNotFound, got %v", err)
				}
				return
			}
			if status.Code(err) != tc.wantCode {
				t.Fatalf("expected code %v, got %v (err=%v)", tc.wantCode, status.Code(err), err)
			}
		})
	}
}

func TestForgeHTTPClient_TimeoutIsUnavailable(t *testing.T) {
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := client.Search(ctx, ep, "puppet-forge", "apache", nil); status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable on a canceled/expired context, got %v", err)
	}
}

func TestForgeHTTPClient_ListReleasesPaginatesAndAssembles(t *testing.T) {
	var calls int
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v3/releases" {
			t.Errorf("unexpected path: %q", r.URL.Path)
		}
		if r.URL.Query().Get("module") != "puppetlabs-apache" {
			t.Errorf("expected module=puppetlabs-apache, got %q", r.URL.Query().Get("module"))
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("offset") == "0" {
			_, _ = w.Write([]byte(`{"pagination":{"limit":100,"offset":0,"total":3},"results":[{"version":"1.0.0"},{"version":"1.1.0"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"pagination":{"limit":100,"offset":2,"total":3},"results":[{"version":"2.0.0"}]}`))
	})

	versions, err := client.ListReleases(context.Background(), ep, "puppetlabs/apache")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 paginated requests, got %d", calls)
	}
	want := []string{"1.0.0", "1.1.0", "2.0.0"}
	if !reflect.DeepEqual(versions, want) {
		t.Fatalf("expected %v, got %v", want, versions)
	}
}

func TestForgeHTTPClient_ListReleasesEmptyFirstPageIsNotFound(t *testing.T) {
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pagination":{"limit":100,"offset":0,"total":0},"results":[]}`))
	})
	if _, err := client.ListReleases(context.Background(), ep, "puppetlabs/doesnotexist"); !errors.Is(err, ErrForgeNotFound) {
		t.Fatalf("expected ErrForgeNotFound for an unknown module, got %v", err)
	}
}

func TestForgeHTTPClient_GetReleaseMapsSlugAndDependencies(t *testing.T) {
	var gotPath string
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"slug": "puppetlabs-apache-12.0.0",
			"version": "12.0.0",
			"metadata": {
				"dependencies": [
					{"name": "puppetlabs/stdlib", "version_requirement": ">= 4.13.1 < 11.0.0"},
					{"name": "puppetlabs/concat", "version_requirement": ">= 2.2.1 < 11.0.0"}
				]
			}
		}`))
	})
	rel, err := client.GetRelease(context.Background(), ep, "puppetlabs/apache", "12.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/v3/releases/puppetlabs-apache-12.0.0" {
		t.Fatalf("expected the release_slug-escaped path, got %q", gotPath)
	}
	if rel.Name != "puppetlabs/apache" || rel.Version != "12.0.0" {
		t.Fatalf("unexpected release identity: %+v", rel)
	}
	want := []ForgeDependency{
		{Name: "puppetlabs/stdlib", VersionRequirement: ">= 4.13.1 < 11.0.0"},
		{Name: "puppetlabs/concat", VersionRequirement: ">= 2.2.1 < 11.0.0"},
	}
	if !reflect.DeepEqual(rel.Dependencies, want) {
		t.Fatalf("expected dependencies %+v, got %+v", want, rel.Dependencies)
	}
}

func TestForgeHTTPClient_GetReleaseNotFound(t *testing.T) {
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":["The requested resource cannot be found"]}`))
	})
	if _, err := client.GetRelease(context.Background(), ep, "puppetlabs/apache", "0.0.0"); !errors.Is(err, ErrForgeNotFound) {
		t.Fatalf("expected ErrForgeNotFound, got %v", err)
	}
}

func TestForgeHTTPClient_RejectsNonHTTPSBaseURL(t *testing.T) {
	client := newHTTPForgeClient(nil)
	_, _, err := client.Search(context.Background(), ForgeEndpoint{BaseURL: "http://insecure.example.test"}, "internal", "apache", nil)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for a non-https base URL, got %v", err)
	}
}

func TestForgeHTTPClient_RejectsBaseURLWithEmbeddedCredentials(t *testing.T) {
	client := newHTTPForgeClient(nil)
	_, err := client.ListReleases(context.Background(), ForgeEndpoint{BaseURL: "https://user:pass@forge.example.test"}, "ns/mod")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for a base URL with embedded userinfo, got %v", err)
	}
}

// assertNoLeak fails when an error message carries the test server's host or
// the endpoint's auth value (T-08-23).
func assertNoLeak(t *testing.T, err error, ep ForgeEndpoint) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error to inspect")
	}
	u, perr := url.Parse(ep.BaseURL)
	if perr != nil {
		t.Fatalf("parsing endpoint: %v", perr)
	}
	msg := err.Error()
	if strings.Contains(msg, u.Host) {
		t.Fatalf("error leaks the server host %q: %s", u.Host, msg)
	}
	if ep.Auth != "" && strings.Contains(msg, ep.Auth) {
		t.Fatalf("error leaks the auth value: %s", msg)
	}
}

// paddedForgeBody returns valid Forge page JSON whitespace-padded to exactly n bytes.
func paddedForgeBody(n int) []byte {
	const doc = `{"pagination":{"limit":20,"offset":0,"total":0},"results":[]}`
	return append([]byte(doc), bytes.Repeat([]byte(" "), n-len(doc))...)
}

func TestForgeHTTPResponseOverflowIsReported(t *testing.T) {
	t.Run("one_byte_over_the_cap_is_internal_and_not_decoded", func(t *testing.T) {
		ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(paddedForgeBody(forgeMaxResponseBytes + 1))
		})
		ep.Auth = "Bearer secret-token"
		_, _, err := client.Search(context.Background(), ep, "puppet-forge", "security", nil)
		if status.Code(err) != codes.Internal {
			t.Fatalf("expected Internal for an oversized body, got %v", err)
		}
		if !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("expected the error to name the size limit (not a malformed body), got %v", err)
		}
		if strings.Contains(err.Error(), "malformed") {
			t.Fatalf("oversize must not be reported as malformed JSON: %v", err)
		}
		assertNoLeak(t, err, ep)
	})
	t.Run("exactly_the_cap_decodes", func(t *testing.T) {
		ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(paddedForgeBody(forgeMaxResponseBytes))
		})
		results, _, err := client.Search(context.Background(), ep, "puppet-forge", "security", nil)
		if err != nil || len(results) != 0 {
			t.Fatalf("expected an at-cap body to decode to an empty page, got results=%v err=%v", results, err)
		}
	})
	t.Run("the_cap_is_unchanged_from_phase_7", func(t *testing.T) {
		if forgeMaxResponseBytes != 8<<20 {
			t.Fatalf("forgeMaxResponseBytes must stay at 8 MiB, got %d", forgeMaxResponseBytes)
		}
	})
}

// threeHitPage answers /v3/modules with three hits (a, b, c under owner "ns").
func threeHitPage(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"pagination":{"limit":20,"offset":0,"total":3},"results":[
		{"slug":"ns-a","name":"a","owner":{"slug":"ns"},"endorsement":"supported","releases":[{"version":"1.0.0","created_at":"2026-01-02 03:04:05 -0700"}]},
		{"slug":"ns-b","name":"b","owner":{"slug":"ns"},"endorsement":"approved","releases":[{"version":"2.0.0","created_at":"2026-02-02 03:04:05 -0700"}]},
		{"slug":"ns-c","name":"c","owner":{"slug":"ns"},"releases":[{"version":"3.0.0","created_at":"2026-03-02 03:04:05 -0700"}]}
	]}`))
}

func TestForgeHTTPSearchEnrichmentDegradesOneHit(t *testing.T) {
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v3/modules":
			threeHitPage(w)
		case "/v3/releases":
			mod := r.URL.Query().Get("module")
			if mod == "ns-b" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{}`))
				return
			}
			_, _ = w.Write([]byte(`{"pagination":{"limit":1,"offset":0,"total":1},"results":[{"version":"9.9.9","validation_score":70,"created_at":"2026-04-02 03:04:05 -0700","metadata":{"summary":"Summary for ` + mod + `"},"tags":["t"]}]}`))
		}
	})
	ep.Auth = "Bearer secret-token"
	results, _, err := client.Search(context.Background(), ep, "puppet-forge", "x", nil)
	if err != nil {
		t.Fatalf("one failed enrichment must not fail the page: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	byName := map[string]*hostv1.ForgeSearchResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	b := byName["ns/b"]
	if b == nil || b.Summary != "" || len(b.Tags) != 0 || b.QualityScore != 0 {
		t.Fatalf("expected ns/b degraded (no summary/tags/score), got %+v", b)
	}
	if b.Version != "2.0.0" || b.ReleaseDate == nil || b.Endorsement != "approved" {
		t.Fatalf("expected ns/b to keep its slim-page version, date and endorsement, got %+v", b)
	}
	a := byName["ns/a"]
	if a == nil || a.Summary != "Summary for ns-a" || a.QualityScore != 70 || a.Version != "9.9.9" {
		t.Fatalf("expected ns/a fully enriched, got %+v", a)
	}
	if c := byName["ns/c"]; c == nil || c.Summary == "" {
		t.Fatalf("expected ns/c fully enriched, got %+v", c)
	}
}

func TestForgeHTTPSearchAllEnrichmentTransportFailuresAreUnavailable(t *testing.T) {
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/modules" {
			w.Header().Set("Content-Type", "application/json")
			threeHitPage(w)
			return
		}
		// Drop the connection without answering: a transport-level failure.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("response writer cannot hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	})
	ep.Auth = "Bearer secret-token"
	results, _, err := client.Search(context.Background(), ep, "puppet-forge", "x", nil)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable when every enrichment call fails in transport, got results=%v err=%v", results, err)
	}
	assertNoLeak(t, err, ep)
}

func TestForgeHTTPSearchToleratesThinSource(t *testing.T) {
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v3/modules":
			_, _ = w.Write([]byte(`{"pagination":{"limit":20,"offset":0,"total":1},"results":[{"slug":"ns-a","name":"a","owner":{"slug":"ns"},"endorsement":"supported"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":["not found"]}`))
		}
	})
	ep.Auth = "Bearer secret-token"
	results, _, err := client.Search(context.Background(), ep, "private", "a", nil)
	if err != nil {
		t.Fatalf("a thin source must still search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Name != "ns/a" || r.Source != "private" || r.Endorsement != "supported" {
		t.Fatalf("expected identity fields populated, got %+v", r)
	}
	if r.Version != "" || r.Summary != "" || len(r.Tags) != 0 || r.ReleaseDate != nil {
		t.Fatalf("expected nothing fabricated for a thin source, got %+v", r)
	}
}

// WR-01: the Forge transport must never follow a redirect, so a private
// source's Authorization value is not re-sent in clear and an internal host is
// not fetched on a registry's say-so.
func TestForgeHTTPClient_DoesNotFollowRedirects(t *testing.T) {
	var targetHits int
	var mu sync.Mutex
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targetHits++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"pagination":{"total":0},"results":[]}`))
	}))
	t.Cleanup(target.Close)

	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v3/modules", http.StatusFound)
	})
	ep.Auth = "Bearer sealed-token"

	for name, call := range map[string]func() error{
		"search": func() error {
			_, _, err := client.Search(context.Background(), ep, "internal", "apache", nil)
			return err
		},
		"list_releases": func() error {
			_, err := client.ListReleases(context.Background(), ep, "puppetlabs/apache")
			return err
		},
		"get_release": func() error {
			_, err := client.GetRelease(context.Background(), ep, "puppetlabs/apache", "1.0.0")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("expected FailedPrecondition for a redirect, got %v", err)
			}
			if strings.Contains(err.Error(), target.URL) {
				t.Fatalf("redirect target leaked into the error: %v", err)
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if targetHits != 0 {
		t.Fatalf("redirect target was contacted %d times; redirects must not be followed", targetHits)
	}
}

// WR-02: a version containing path syntax must be refused before any request,
// not collapsed by path cleaning onto another endpoint of the registry host.
func TestForgeHTTPClient_GetReleaseRejectsPathyVersion(t *testing.T) {
	var calls atomic.Int32
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	for _, v := range []string{"1.0/../../../admin", "../x", "1.0.0/", "1.0.0?x=y", "1.0.0#frag", "1 0", "%2e%2e", "-1.0.0"} {
		_, err := client.GetRelease(context.Background(), ep, "puppetlabs/apache", v)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("version %q: expected InvalidArgument, got %v", v, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("expected no upstream request for an invalid version, saw %d", n)
	}
	// Real-world SemVer shapes remain accepted (validation, not a 400).
	for _, v := range []string{"12.0.0", "1.0.0-rc.1", "1.0.0+build.5", "0.1.0-beta_2"} {
		if _, err := client.GetRelease(context.Background(), ep, "puppetlabs/apache", v); status.Code(err) == codes.InvalidArgument {
			t.Fatalf("version %q was wrongly rejected: %v", v, err)
		}
	}
}

// WR-03: a malformed or non-https base URL must not echo any part of the raw
// (possibly credential-bearing) value in the error it returns.
func TestValidateForgeBaseURL_ErrorsDoNotEchoTheRawURL(t *testing.T) {
	for _, raw := range []string{
		"https://tok:abc%zz@forge.example.test",
		"tok:abc@forge.example.test",
		"%zz-secret-%zz",
	} {
		_, err := validateForgeBaseURL(raw)
		if err == nil {
			t.Fatalf("%q: expected an error", raw)
		}
		for _, frag := range []string{"abc", "tok", "secret", "forge.example.test"} {
			if strings.Contains(err.Error(), frag) {
				t.Fatalf("%q: error %q leaks fragment %q", raw, err.Error(), frag)
			}
		}
	}
}

// WR-04: a registry that never stops paging, or keeps repeating a page, must
// end in a bounded error rather than loop until the caller's context expires.
func TestForgeHTTPClient_ListReleasesPaginationIsBounded(t *testing.T) {
	t.Run("endless_distinct_pages_hit_the_cap", func(t *testing.T) {
		var calls atomic.Int32
		ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
			n := int(calls.Add(1))
			var sb strings.Builder
			sb.WriteString(`{"pagination":{"total":1000000000},"results":[`)
			for i := 0; i < 100; i++ {
				if i > 0 {
					sb.WriteString(",")
				}
				sb.WriteString(`{"version":"1.`)
				sb.WriteString(strconv.Itoa(n))
				sb.WriteString(`.`)
				sb.WriteString(strconv.Itoa(i))
				sb.WriteString(`"}`)
			}
			sb.WriteString(`]}`)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(sb.String()))
		})
		_, err := client.ListReleases(context.Background(), ep, "puppetlabs/apache")
		if status.Code(err) != codes.Internal {
			t.Fatalf("expected Internal once the release cap is exceeded, got %v", err)
		}
		if n := calls.Load(); n > forgeMaxReleases/100+2 {
			t.Fatalf("made %d requests; the cap should stop paging near %d", n, forgeMaxReleases/100+1)
		}
	})
	t.Run("repeated_page_is_rejected", func(t *testing.T) {
		var calls atomic.Int32
		ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"pagination":{"total":1000000000},"results":[{"version":"1.0.0"}]}`))
		})
		_, err := client.ListReleases(context.Background(), ep, "puppetlabs/apache")
		if status.Code(err) != codes.Internal {
			t.Fatalf("expected Internal for a repeating registry, got %v", err)
		}
		if n := calls.Load(); n > 3 {
			t.Fatalf("made %d requests for a repeating page; expected it to stop at the second", n)
		}
	})
}
