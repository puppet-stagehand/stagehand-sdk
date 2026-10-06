package approval

// This file carries the approver token from decide to the Documents facet.
//
// The token is a secret. Nothing here, and nothing that reads these helpers,
// may log, format or echo a metadata value in an error message, ErrorDetail or
// test failure output (FND-03, T-13-08).
//
// It adds transport for a token decide already holds; it adds no second
// verification. decide.go keeps the package's single VerifyTokenRequest
// literal (pinned by TestApprovalScopeSourcedFromKindOnly), and the host
// verifies the same secret against the Kind's scope when it sees a status
// transition.

import (
	"context"

	"google.golang.org/grpc/metadata"
)

// TokenMetadataKey is the gRPC metadata key an approver's token travels in on
// the Documents Put that records a decision. It is lower-case because gRPC
// metadata keys are. The console's Documents interceptor must read this key
// from incoming metadata to authorize a proposal status transition.
const TokenMetadataKey = "stagehand-approver-token"

// WithApproverToken returns ctx carrying secret as the single value of
// TokenMetadataKey in both the outgoing metadata (what a dialed gRPC client
// sends) and the incoming metadata (what an in-process host.Local Documents
// server reads), so both transports behave the same. It replaces any value
// already present for the key and never appends: exactly one value travels in
// each direction.
func WithApproverToken(ctx context.Context, secret string) context.Context {
	out, _ := metadata.FromOutgoingContext(ctx)
	out = out.Copy()
	out.Set(TokenMetadataKey, secret)
	ctx = metadata.NewOutgoingContext(ctx, out)

	in, _ := metadata.FromIncomingContext(ctx)
	in = in.Copy()
	in.Set(TokenMetadataKey, secret)
	return metadata.NewIncomingContext(ctx, in)
}

// IncomingApproverTokens returns every value of TokenMetadataKey in ctx's
// incoming metadata, or nil when there are none. A host must treat anything
// other than exactly one value as no token at all.
func IncomingApproverTokens(ctx context.Context) []string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	return md.Get(TokenMetadataKey)
}

// GovernanceKeys returns a copy of the body keys this package owns (status,
// reason, decided_by, decided_at, approved_scope), so a host guard uses the
// same list ProposeBody refuses in a caller's body.
func GovernanceKeys() []string {
	out := make([]string, len(governanceKeys))
	copy(out, governanceKeys)
	return out
}
