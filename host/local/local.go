// Package local implements host.Host entirely in-process, backed by plain
// in-memory maps. It is a test double for Expansion Pack authors and for
// this SDK's own tests, not a preview of how the console host will
// actually implement these services (that's Postgres-backed, built later
// in stagehand-console).
package local

import (
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
)

// Option configures optional behavior of a host.Local instance built by
// New. WithDiscoverCandidates (D-05) and WithGroupClasses follow the same
// constructor-injectable-with-default pattern.
type Option func(*config)

// config holds the settings New builds from its opts before constructing
// the returned host.Host.
type config struct {
	discoverCandidates    []*hostv1.Node
	discoverCandidatesSet bool
	groupClasses          map[string][]*hostv1.Class
	groupClassesSet       bool
	forgeClient           ForgeClient
}

// defaultConfig returns a config seeded with a private clone of the
// built-in Discover fixture set and the built-in class-assignment fixture
// set, used whenever WithDiscoverCandidates / WithGroupClasses is never
// applied at all.
func defaultConfig() *config {
	return &config{
		discoverCandidates: cloneNodeSlice(defaultDiscoverCandidates),
		groupClasses:       cloneGroupClassesMap(defaultGroupClasses),
	}
}

// defaultDiscoverCandidates is host.Local's built-in Discover fixture
// data — not a preview of any real discovery source. Two nodes so
// ordering is observable in tests.
var defaultDiscoverCandidates = []*hostv1.Node{
	{Id: "db-01.example.test", DisplayName: "db-01", Status: hostv1.Node_DISCOVERED, Environment: "staging"},
	{Id: "web-01.example.test", DisplayName: "web-01", Status: hostv1.Node_DISCOVERED, Environment: "production"},
}

// WithDiscoverCandidates overrides host.Local's Discover fixture set with
// the given candidates. Calling it with zero arguments means "this host
// has no discoverable candidates" — an explicit empty set, distinct from
// never applying the option at all, which falls back to
// defaultDiscoverCandidates (D-05).
func WithDiscoverCandidates(candidates ...*hostv1.Node) Option {
	return func(c *config) {
		c.discoverCandidates = candidates
		c.discoverCandidatesSet = true
	}
}

// defaultGroupClasses is host.Local's built-in class-assignment fixture
// data (D-05 pattern) — not a preview of any real rule engine's output.
// Exactly one group, "webservers", with two classes so ordering is
// observable in tests.
var defaultGroupClasses = map[string][]*hostv1.Class{
	"webservers": {
		{Name: "profile::base"},
		{Name: "profile::web"},
	},
}

// WithGroupClasses overrides host.Local's class-assignment fixture set
// with the given group-id-to-classes map. This seam exists because the
// locked contract has no RPC that writes a class — without a
// constructor-injected source, ListClasses could only ever return the
// built-in default, and Phase 5's proof example needs to define classes
// that mean something for its own scenario (mirrors D-05's reasoning for
// the Discover candidate set exactly).
//
// Applying it with a nil or empty map means "this host has no class
// assignments" — an explicit empty set, distinct from never applying the
// option at all, which falls back to defaultGroupClasses.
func WithGroupClasses(classes map[string][]*hostv1.Class) Option {
	return func(c *config) {
		c.groupClasses = classes
		c.groupClassesSet = true
	}
}

// WithForgeClient injects the registry implementation used by host.Local.
// Tests and pack examples should use a deterministic fixture; production
// callers may leave this unset for the default HTTP client seam.
func WithForgeClient(client ForgeClient) Option {
	return func(c *config) { c.forgeClient = client }
}

// New builds an in-process host.Host scoped to a manifest's declared
// permissions. Documents is always available (scoped to packID's own
// namespace instead of permission-gated); Settings is always available;
// Secrets requires "secrets:rw", Auth requires "tokens:issue", Inventory
// requires "inventory:rw", and Code requires "code:rw" — calling a gated
// facet without its permission declared returns the same PERMISSION_DENIED
// a real Expansion Pack would get from the console's gRPC interceptor.
//
// All six in-scope facets (Documents, Settings, Secrets, Auth, Inventory,
// Code) are real implementations as of this plan (06-01).
func New(permissions []string, packID string, opts ...Option) *host.Host {
	perms := make(map[string]bool, len(permissions))
	for _, p := range permissions {
		perms[p] = true
	}
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(cfg)
	}
	docs := newDocumentsServer(packID)
	secrets := newSecretsServer(packID)
	return &host.Host{
		Documents: docs,
		Settings:  newSettingsServer(),
		Secrets:   &gatedSecrets{perms: perms, packID: packID, inner: secrets},
		Auth:      &gatedAuth{perms: perms, packID: packID, inner: newAuthServer(packID)},
		Inventory: &gatedInventory{perms: perms, packID: packID, inner: newInventoryServer(packID, docs, cfg.discoverCandidates, cfg.groupClasses)},
		Code:      &gatedCode{perms: perms, packID: packID, inner: newCodeServer(packID, docs)},
		Forge:     &gatedForge{perms: perms, packID: packID, inner: newForgeServer(packID, docs, secrets, cfg.forgeClient)},
	}
}

// ErrPermissionDenied builds the exact PERMISSION_DENIED status this
// package returns for an undeclared facet, exported so tests outside this
// package can assert on it without string-matching.
func ErrPermissionDenied(perm string) error {
	st := status.New(codes.PermissionDenied, fmt.Sprintf("facet not declared: %s", perm))
	withDetails, err := st.WithDetails(&hostv1.ErrorDetail{
		Code:    "facet_not_declared",
		Message: fmt.Sprintf("this Expansion Pack's manifest does not declare the %q permission", perm),
		Fix:     fmt.Sprintf("add %q to the manifest's permissions list", perm),
	})
	if err != nil {
		return st.Err() // details are best-effort; the status itself must never fail to construct
	}
	return withDetails.Err()
}
