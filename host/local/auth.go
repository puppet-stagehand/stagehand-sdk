package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

type issuedToken struct {
	tokenID   string
	secret    string
	scope     string
	label     string
	expiresAt time.Time
}

// authServer is the real in-memory Auth facet implementation: an
// in-process token store keyed by both secret (for Verify) and token id
// (for Revoke). It is not permission-gated on its own — gatedAuth wraps
// it with the tokens:issue check every method requires.
type authServer struct {
	hostv1.UnimplementedAuthServer
	packID   string
	mu       sync.Mutex
	bySecret map[string]*issuedToken
	byID     map[string]*issuedToken
}

func newAuthServer(packID string) *authServer {
	return &authServer{packID: packID, bySecret: map[string]*issuedToken{}, byID: map[string]*issuedToken{}}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *authServer) IssueToken(ctx context.Context, req *hostv1.IssueTokenRequest) (*hostv1.IssuedToken, error) {
	tok := &issuedToken{
		tokenID:   randomHex(8),
		secret:    randomHex(24),
		scope:     req.Scope,
		label:     req.Label,
		expiresAt: time.Now().Add(time.Duration(req.TtlSeconds) * time.Second),
	}
	s.mu.Lock()
	s.bySecret[tok.secret] = tok
	s.byID[tok.tokenID] = tok
	s.mu.Unlock()
	return &hostv1.IssuedToken{
		TokenId:   tok.tokenID,
		Secret:    tok.secret,
		ExpiresAt: timestamppb.New(tok.expiresAt),
	}, nil
}

func (s *authServer) Verify(ctx context.Context, req *hostv1.VerifyTokenRequest) (*hostv1.Principal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.bySecret[req.Secret]
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "unknown token")
	}
	if time.Now().After(tok.expiresAt) {
		return nil, status.Error(codes.Unauthenticated, "token expired")
	}
	if tok.scope != req.Scope {
		return nil, status.Errorf(codes.PermissionDenied, "token was not issued with scope %q", req.Scope)
	}
	return &hostv1.Principal{TokenId: tok.tokenID, Label: tok.label, Scopes: []string{tok.scope}}, nil
}

func (s *authServer) Revoke(ctx context.Context, req *hostv1.TokenRef) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tok, ok := s.byID[req.TokenId]; ok {
		delete(s.bySecret, tok.secret)
		delete(s.byID, req.TokenId)
	}
	return &emptypb.Empty{}, nil
}

// gatedAuth wraps authServer with the tokens:issue permission check every
// method requires.
type gatedAuth struct {
	hostv1.UnimplementedAuthServer
	perms  map[string]bool
	packID string
	inner  *authServer
}

func (g *gatedAuth) check() error {
	if !g.perms["tokens:issue"] {
		return ErrPermissionDenied("tokens:issue")
	}
	return nil
}

func (g *gatedAuth) IssueToken(ctx context.Context, req *hostv1.IssueTokenRequest) (*hostv1.IssuedToken, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.IssueToken(ctx, req)
}
func (g *gatedAuth) Verify(ctx context.Context, req *hostv1.VerifyTokenRequest) (*hostv1.Principal, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.Verify(ctx, req)
}
func (g *gatedAuth) Revoke(ctx context.Context, req *hostv1.TokenRef) (*emptypb.Empty, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.Revoke(ctx, req)
}
