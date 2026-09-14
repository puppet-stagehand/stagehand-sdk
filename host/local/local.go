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

// New builds an in-process host.Host scoped to a manifest's declared
// permissions. Documents is always available (scoped to packID's own
// namespace instead of permission-gated); Settings is always available;
// Secrets requires "secrets:rw" and Auth requires "tokens:issue" — calling
// either without the permission declared returns the same PERMISSION_DENIED
// a real Expansion Pack would get from the console's gRPC interceptor.
//
// Auth is still a placeholder hostv1.UnimplementedAuthServer value as of
// this plan (01-06) — Plan 01-07 adds the real authServer implementation
// (plus the gatedAuth permission wrapper) and is expected to rewire the
// field below to use it. Settings (01-05) and Secrets (01-06) are real as
// of this plan.
func New(permissions []string, packID string) *host.Host {
	perms := make(map[string]bool, len(permissions))
	for _, p := range permissions {
		perms[p] = true
	}
	docs := newDocumentsServer(packID)
	return &host.Host{
		Documents: docs,
		Settings:  newSettingsServer(),
		Secrets:   &gatedSecrets{perms: perms, packID: packID, inner: newSecretsServer(packID)},
		Auth:      &hostv1.UnimplementedAuthServer{},
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
