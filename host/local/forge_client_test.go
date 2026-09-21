package local

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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

func TestForgeHTTPSearch_ExactEndpointQueryShapeAndAuth(t *testing.T) {
	var gotPath, gotRawQuery, gotAuth string
	ep, client := newTestForgeClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
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
					"current_release": {
						"version": "12.0.0",
						"validation_score": 87,
						"created_at": "2026-01-02 03:04:05 -0700"
					}
				}
			]
		}`))
	})
	ep.Auth = "Bearer secret-token"

	results, page, err := client.Search(context.Background(), ep, "internal", "apache", &hostv1.Page{Limit: 20})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/v3/modules" {
		t.Fatalf("expected path /v3/modules, got %q", gotPath)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("expected auth header forwarded, got %q", gotAuth)
	}
	q, err := url.ParseQuery(gotRawQuery)
	if err != nil {
		t.Fatalf("parsing query: %v", err)
	}
	if q.Get("query") != "apache" || q.Get("limit") != "20" || q.Get("offset") != "0" {
		t.Fatalf("unexpected query params: %v", q)
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
	if r.Endorsement != "supported" || r.QualityScore != 87 || r.Deprecated || r.SupersededBy != "" {
		t.Fatalf("unexpected metadata: %+v", r)
	}
	if r.ReleaseDate == nil {
		t.Fatalf("expected release date to be populated")
	}
	if page == nil || page.NextCursor != "1" {
		t.Fatalf("expected next cursor 1 (offset 0 + 1 result < total 50), got %+v", page)
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
