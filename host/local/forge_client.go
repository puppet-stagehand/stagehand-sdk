package local

// This file implements the real Forge v3 HTTP transport (D-01/D-02):
// httpForgeClient is the injectable ForgeClient host.Local wires by default,
// targeting the public registry at DefaultForgeBaseURL. host.Local's
// constructor injection seam (WithForgeClient) lets tests and pack examples
// substitute a deterministic fixture instead — the interface is defined
// here rather than forge.go because forge.go's forgeServer only depends on
// it, while this file owns the concrete transport and JSON mapping.
//
// Field mapping is verified against the live Puppet Forge v3 OpenAPI
// document (https://forgeapi.puppet.com/v3/openapi.json) fetched during
// this plan's execution, not guessed from the v3 docs alone: Module.name is
// the bare module name ("apache"), Module.slug/owner.slug together form the
// "namespace/name" identity this project's Puppetfile model already uses
// (PuppetfileModule.name), Module.current_release.validation_score (0-100,
// nullable) is the "quality score" FORGE-02 asks for, and dependency
// requirements live at Release.metadata.dependencies.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// DefaultForgeBaseURL is the public Puppet Forge v3 registry host.Local's
// real HTTP client targets when a request selects the public "puppet-forge"
// source, or configures no source at all (D-01/D-02).
const DefaultForgeBaseURL = "https://forgeapi.puppet.com"

// forgeHTTPTimeout bounds every Forge v3 request's own client-side timeout,
// independent of any shorter context deadline a caller supplies. Defense in
// depth against T-07-06 (unbounded body/hang).
const forgeHTTPTimeout = 15 * time.Second

// forgeMaxResponseBytes bounds how much of a Forge response body this
// client will read, defense in depth against T-07-06 regardless of
// Content-Length honesty.
const forgeMaxResponseBytes = 8 << 20 // 8 MiB

// forgeExcludeFields trims the large prose fields (readme/changelog/
// license/reference/docs) Forge's Release representation otherwise embeds,
// since ListReleases/GetRelease only need version and dependency metadata.
const forgeExcludeFields = "readme changelog license reference docs"

// ForgeEndpoint identifies where a Forge v3 request goes and, for a private
// source, the credential to send. BaseURL must be an absolute https:// URL
// with no embedded userinfo (T-07-05). An empty Auth means "no
// Authorization header" — the posture for the public registry; a
// configured private source's Auth is sent verbatim as the Authorization
// header value, never as a query parameter (D-06/T-07-04).
type ForgeEndpoint struct {
	BaseURL string
	Auth    string
}

// ForgeRelease is one concrete module release's metadata, including the
// dependency requirements 07-03's resolver walks. It intentionally carries
// no proto dependency: nothing in the wire contract models "a release's raw
// dependency edges" (DependencyNode is the resolver's OUTPUT shape, built
// from this).
type ForgeRelease struct {
	Name         string // "namespace/name", matching PuppetfileModule.name
	Version      string
	Dependencies []ForgeDependency
}

// ForgeDependency is one declared dependency edge from a release's
// metadata.json: a module name and its Puppet-style version_requirement
// string. 07-03's resolver parses VersionRequirement with the verified
// Masterminds/semver/v3 dependency; this client never parses it.
type ForgeDependency struct {
	Name               string
	VersionRequirement string
}

// ErrForgeNotFound is returned by ListReleases/GetRelease (never Search,
// which reports an empty page instead) when the module or release
// genuinely does not exist upstream (a Forge 404) — distinct from a
// transport/server failure. 07-03's resolver uses errors.Is against
// ErrForgeNotFound to build an unresolved advisory node rather than fail
// the whole Resolve RPC (D-12).
var ErrForgeNotFound = errors.New("forge: module or release not found")

// ForgeClient is the transport-independent registry seam. host.Local wires
// a real httpForgeClient by default (DefaultForgeClient); tests and pack
// examples inject a deterministic fixture via WithForgeClient instead. Every
// method takes an explicit ForgeEndpoint so one client instance serves both
// the public registry and any number of configured private sources — the
// endpoint, not the client, carries the "which registry" identity.
type ForgeClient interface {
	// Search queries one source's Forge v3 module search endpoint and
	// returns source-tagged, metadata-rich results (FORGE-01/02) plus the
	// next page's cursor (empty when this is the last page).
	Search(ctx context.Context, ep ForgeEndpoint, source, query string, page *hostv1.Page) ([]*hostv1.ForgeSearchResult, *hostv1.PageInfo, error)
	// ListReleases returns every known release version for name (a
	// "namespace/name" module identity), in the order the API returns
	// them — 07-03's resolver is responsible for SemVer ordering, not this
	// client. Returns ErrForgeNotFound when the module itself is unknown.
	ListReleases(ctx context.Context, ep ForgeEndpoint, name string) ([]string, error)
	// GetRelease fetches one concrete release's metadata, including its
	// declared dependency requirements, for the dependency-tree walk.
	// Returns ErrForgeNotFound when that module/version is unknown.
	GetRelease(ctx context.Context, ep ForgeEndpoint, name, version string) (*ForgeRelease, error)
}

// httpForgeClient is the real net/http-backed ForgeClient implementation
// (D-02). httpClient is injectable so tests can point it at an
// httptest.NewTLSServer's own trusted client while production code gets a
// plain bounded-timeout client.
type httpForgeClient struct {
	httpClient *http.Client
}

// newHTTPForgeClient builds an httpForgeClient. A nil hc gets a default
// client bounded by forgeHTTPTimeout; tests pass httptest's server.Client()
// so the TLS trust root matches the test server's self-signed certificate.
func newHTTPForgeClient(hc *http.Client) *httpForgeClient {
	if hc == nil {
		hc = &http.Client{Timeout: forgeHTTPTimeout}
	}
	return &httpForgeClient{httpClient: hc}
}

// DefaultForgeClient returns the real HTTP-backed ForgeClient host.Local
// wires in by default (D-01/D-02).
func DefaultForgeClient() ForgeClient { return newHTTPForgeClient(nil) }

// ---------------------------------------------------------------- Search

func (c *httpForgeClient) Search(ctx context.Context, ep ForgeEndpoint, source, query string, page *hostv1.Page) ([]*hostv1.ForgeSearchResult, *hostv1.PageInfo, error) {
	base, err := validateForgeBaseURL(ep.BaseURL)
	if err != nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, "forge: %v", err)
	}
	limit, offset, err := forgePageParams(page)
	if err != nil {
		return nil, nil, err
	}

	u := base.JoinPath("v3", "modules")
	q := u.Query()
	q.Set("query", query)
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", strconv.Itoa(offset))
	u.RawQuery = q.Encode()

	var payload forgeModuleCollectionJSON
	if err := c.doJSON(ctx, ep, u.String(), &payload); err != nil {
		if errors.Is(err, ErrForgeNotFound) {
			// The search endpoint reports "no matches" as an empty results
			// page, never a 404; an actual 404 here means the upstream
			// contract diverged from what this adapter expects.
			return nil, nil, status.Error(codes.Internal, "forge: unexpected not-found response from search endpoint")
		}
		return nil, nil, err
	}

	results := make([]*hostv1.ForgeSearchResult, 0, len(payload.Results))
	for _, m := range payload.Results {
		r, err := mapForgeSearchResult(m, source)
		if err != nil {
			return nil, nil, err
		}
		results = append(results, r)
	}

	pageInfo := &hostv1.PageInfo{}
	next := offset + len(payload.Results)
	if len(payload.Results) > 0 && next < payload.Pagination.Total {
		pageInfo.NextCursor = strconv.Itoa(next)
	}
	return results, pageInfo, nil
}

// ------------------------------------------------------------ ListReleases

func (c *httpForgeClient) ListReleases(ctx context.Context, ep ForgeEndpoint, name string) ([]string, error) {
	base, err := validateForgeBaseURL(ep.BaseURL)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "forge: %v", err)
	}
	forgeSlug, err := forgeModuleSlug(name)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "forge: %v", err)
	}

	const pageSize = 100
	var versions []string
	offset := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, status.Errorf(codes.Unavailable, "forge: context ended during pagination: %v", err)
		}

		u := base.JoinPath("v3", "releases")
		q := u.Query()
		q.Set("module", forgeSlug)
		q.Set("limit", strconv.Itoa(pageSize))
		q.Set("offset", strconv.Itoa(offset))
		q.Set("exclude_fields", forgeExcludeFields)
		u.RawQuery = q.Encode()

		var payload forgeReleaseCollectionJSON
		if err := c.doJSON(ctx, ep, u.String(), &payload); err != nil {
			return nil, err // ErrForgeNotFound or a status error, propagated as-is
		}
		if len(payload.Results) == 0 {
			if offset == 0 {
				return nil, ErrForgeNotFound
			}
			break
		}
		for _, r := range payload.Results {
			if r.Version == "" {
				return nil, status.Error(codes.Internal, "forge: malformed release (missing version)")
			}
			versions = append(versions, r.Version)
		}
		offset += len(payload.Results)
		if offset >= payload.Pagination.Total {
			break
		}
	}
	return versions, nil
}

// -------------------------------------------------------------- GetRelease

func (c *httpForgeClient) GetRelease(ctx context.Context, ep ForgeEndpoint, name, version string) (*ForgeRelease, error) {
	base, err := validateForgeBaseURL(ep.BaseURL)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "forge: %v", err)
	}
	forgeSlug, err := forgeModuleSlug(name)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "forge: %v", err)
	}
	if version == "" {
		return nil, status.Error(codes.InvalidArgument, "forge: version is required")
	}

	u := base.JoinPath("v3", "releases", forgeSlug+"-"+version)
	q := u.Query()
	q.Set("exclude_fields", forgeExcludeFields)
	u.RawQuery = q.Encode()

	var payload forgeReleaseJSON
	if err := c.doJSON(ctx, ep, u.String(), &payload); err != nil {
		return nil, err
	}
	if payload.Slug == "" || payload.Version == "" {
		return nil, status.Error(codes.Internal, "forge: malformed release (missing slug/version)")
	}

	deps := make([]ForgeDependency, 0, len(payload.Metadata.Dependencies))
	for _, d := range payload.Metadata.Dependencies {
		if d.Name == "" {
			return nil, status.Error(codes.Internal, "forge: malformed dependency entry (missing name)")
		}
		deps = append(deps, ForgeDependency{Name: d.Name, VersionRequirement: d.VersionRequirement})
	}
	return &ForgeRelease{Name: name, Version: payload.Version, Dependencies: deps}, nil
}

// ------------------------------------------------------------- transport

// doJSON performs a GET against rawURL, applies ep's auth header, classifies
// the response per D-03 (network/timeout/429/5xx -> Unavailable, a 404 ->
// ErrForgeNotFound, any other non-2xx or a malformed body -> Internal), and
// decodes a 2xx body into out.
func (c *httpForgeClient) doJSON(ctx context.Context, ep ForgeEndpoint, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return status.Errorf(codes.Internal, "forge: building request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if ep.Auth != "" {
		req.Header.Set("Authorization", ep.Auth)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Never surface err.Error() here: net/http wraps it around the
		// full request URL (and, historically, redirected URLs), which is
		// exactly the credential/URL leakage T-07-04 forbids even though
		// this adapter never puts auth in the URL itself.
		return status.Error(codes.Unavailable, "forge: request failed (network error, timeout, or canceled context)")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, forgeMaxResponseBytes))
	if err != nil {
		return status.Error(codes.Unavailable, "forge: reading response failed")
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrForgeNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		return status.Error(codes.Unavailable, "forge: rate limited (429)")
	case resp.StatusCode >= 500:
		return status.Errorf(codes.Unavailable, "forge: upstream error (%d)", resp.StatusCode)
	case resp.StatusCode >= 400:
		return status.Errorf(codes.Internal, "forge: unexpected status %d", resp.StatusCode)
	}

	if err := json.Unmarshal(body, out); err != nil {
		return status.Error(codes.Internal, "forge: malformed response body")
	}
	return nil
}

// ------------------------------------------------------------- validation

// validateForgeBaseURL enforces T-07-05: an absolute https:// URL with a
// host and no embedded userinfo. Applied to every request (public default
// included), not just private sources — defense in depth.
func validateForgeBaseURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("forge base URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid forge base URL: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("forge base URL must use https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("forge base URL must include a host")
	}
	if u.User != nil {
		return nil, errors.New("forge base URL must not embed credentials")
	}
	return u, nil
}

// forgeModuleSlug converts this project's "namespace/name" module identity
// (matching PuppetfileModule.name) into Forge's native hyphenated slug
// form ("namespace-name"). Only the first separator is replaced: a name
// that already arrives in hyphen form, or a module name that itself
// contains a hyphen, passes through unambiguously either way because the
// namespace/name split only ever happens once.
func forgeModuleSlug(name string) (string, error) {
	if name == "" {
		return "", errors.New("module name is required")
	}
	if strings.Count(name, "/") > 1 {
		return "", fmt.Errorf("module name %q must be a single namespace/name identity", name)
	}
	return strings.Replace(name, "/", "-", 1), nil
}

// forgePageParams validates and defaults a request Page into a Forge v3
// limit/offset pair. Forge's own default page size is 20; this adapter
// keeps that default and caps limit at 100 to bound response size
// (defense in depth, T-07-06).
func forgePageParams(page *hostv1.Page) (limit, offset int, err error) {
	limit = 20
	if page != nil && page.Limit > 0 {
		limit = int(page.Limit)
	}
	if limit > 100 {
		return 0, 0, status.Errorf(codes.InvalidArgument, "page limit must be at most 100, got %d", limit)
	}
	if page != nil && page.Cursor != "" {
		n, cerr := strconv.Atoi(page.Cursor)
		if cerr != nil || n < 0 {
			return 0, 0, status.Errorf(codes.InvalidArgument, "invalid page cursor %q", page.Cursor)
		}
		offset = n
	}
	return limit, offset, nil
}

// parseForgeTime parses a Forge v3 iso8601 timestamp. The live API returns
// "2006-01-02 15:04:05 -0700" (verified against forgeapi.puppet.com during
// this plan), not strict RFC3339; RFC3339 is accepted too since the OpenAPI
// document only promises "iso8601" and a private/compatible source may
// format it differently.
func parseForgeTime(s string) (time.Time, error) {
	layouts := []string{"2006-01-02 15:04:05 -0700", time.RFC3339}
	var lastErr error
	for _, layout := range layouts {
		t, err := time.Parse(layout, s)
		if err == nil {
			return t, nil
		}
		lastErr = err
	}
	return time.Time{}, lastErr
}

// ------------------------------------------------------------- JSON shapes
//
// These structs model only the fields this adapter maps, verified against
// the live https://forgeapi.puppet.com/v3/openapi.json document and live
// sample responses fetched during this plan's execution. Unknown JSON
// fields are ignored by encoding/json, which is intentional — Forge's real
// payloads carry many fields (readme, tags, owner metadata, …) this facet
// has no use for.

type forgePaginationJSON struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

type forgeModuleOwnerJSON struct {
	Slug string `json:"slug"`
}

type forgeModuleSupersededByJSON struct {
	Slug string `json:"slug"`
}

// forgeReleaseSummaryJSON models the subset of Module.current_release this
// adapter reads: version, quality/validation score, and the release date.
type forgeReleaseSummaryJSON struct {
	Version         string `json:"version"`
	ValidationScore *int   `json:"validation_score"`
	CreatedAt       string `json:"created_at"`
}

type forgeModuleJSON struct {
	Slug           string                       `json:"slug"`
	Name           string                       `json:"name"`
	Owner          forgeModuleOwnerJSON         `json:"owner"`
	DeprecatedAt   *string                      `json:"deprecated_at"`
	SupersededBy   *forgeModuleSupersededByJSON `json:"superseded_by"`
	Endorsement    *string                      `json:"endorsement"`
	CurrentRelease *forgeReleaseSummaryJSON     `json:"current_release"`
}

type forgeModuleCollectionJSON struct {
	Pagination forgePaginationJSON `json:"pagination"`
	Results    []forgeModuleJSON   `json:"results"`
}

type forgeReleaseCollectionJSON struct {
	Pagination forgePaginationJSON       `json:"pagination"`
	Results    []forgeReleaseSummaryJSON `json:"results"`
}

type forgeDependencyJSON struct {
	Name               string `json:"name"`
	VersionRequirement string `json:"version_requirement"`
}

type forgeReleaseMetadataJSON struct {
	Dependencies []forgeDependencyJSON `json:"dependencies"`
}

type forgeReleaseJSON struct {
	Slug     string                   `json:"slug"`
	Version  string                   `json:"version"`
	Metadata forgeReleaseMetadataJSON `json:"metadata"`
}

// mapForgeSearchResult maps one Module JSON object to the proto
// ForgeSearchResult FORGE-01/02 requires, preserving endorsement, quality,
// release date, deprecation and supersession — never reducing to
// slug/version. source is stamped by the caller (D-08): this adapter has
// no source identity of its own, only the endpoint it was told to call.
func mapForgeSearchResult(m forgeModuleJSON, source string) (*hostv1.ForgeSearchResult, error) {
	if m.Slug == "" || m.Name == "" || m.Owner.Slug == "" {
		return nil, status.Error(codes.Internal, "forge: malformed module result (missing slug, name, or owner)")
	}
	r := &hostv1.ForgeSearchResult{
		Name:   m.Owner.Slug + "/" + m.Name,
		Source: source,
	}
	if m.Endorsement != nil {
		r.Endorsement = *m.Endorsement
	}
	if m.DeprecatedAt != nil && *m.DeprecatedAt != "" {
		r.Deprecated = true
	}
	if m.SupersededBy != nil {
		r.SupersededBy = m.SupersededBy.Slug
	}
	if m.CurrentRelease != nil {
		r.Version = m.CurrentRelease.Version
		if m.CurrentRelease.ValidationScore != nil {
			r.QualityScore = float64(*m.CurrentRelease.ValidationScore)
		}
		if m.CurrentRelease.CreatedAt != "" {
			t, err := parseForgeTime(m.CurrentRelease.CreatedAt)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "forge: malformed release date: %v", err)
			}
			r.ReleaseDate = timestamppb.New(t)
		}
	}
	return r, nil
}
