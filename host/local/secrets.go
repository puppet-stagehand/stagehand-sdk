package local

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

const localSealKeyID = "host-local-v1"

type secretsServer struct {
	hostv1.UnimplementedSecretsServer
	packID string
	key    [32]byte
	mu     sync.Mutex
	store  map[string][]byte // ref -> plaintext
}

func newSecretsServer(packID string) *secretsServer {
	var key [32]byte
	_, _ = rand.Read(key[:]) // host.Local's own key; never used outside this process
	return &secretsServer{packID: packID, key: key, store: map[string][]byte{}}
}

func (s *secretsServer) refFor(name string) string {
	return fmt.Sprintf("expansion/%s/%s", s.packID, name)
}

func (s *secretsServer) Store(ctx context.Context, req *hostv1.StoreSecretRequest) (*hostv1.SecretRef, error) {
	ref := s.refFor(req.Name)
	s.mu.Lock()
	s.store[ref] = append([]byte(nil), req.Plaintext...)
	s.mu.Unlock()
	return &hostv1.SecretRef{Ref: ref}, nil
}

func (s *secretsServer) Reveal(ctx context.Context, req *hostv1.SecretRef) (*hostv1.SecretValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pt, ok := s.store[req.Ref]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no secret at ref %q", req.Ref)
	}
	return &hostv1.SecretValue{Plaintext: append([]byte(nil), pt...)}, nil
}

func (s *secretsServer) Delete(ctx context.Context, req *hostv1.SecretRef) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.store, req.Ref)
	return &emptypb.Empty{}, nil
}

func (s *secretsServer) Seal(ctx context.Context, req *hostv1.SecretValue) (*hostv1.Sealed, error) {
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, status.Errorf(codes.Internal, "seal: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "seal: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, status.Errorf(codes.Internal, "seal: %v", err)
	}
	ct := gcm.Seal(nil, nonce, req.Plaintext, nil)
	return &hostv1.Sealed{Ciphertext: ct, Nonce: nonce, KeyId: localSealKeyID}, nil
}

func (s *secretsServer) Open(ctx context.Context, req *hostv1.Sealed) (*hostv1.SecretValue, error) {
	if req.KeyId != localSealKeyID {
		return nil, status.Errorf(codes.FailedPrecondition, "unknown key_id %q", req.KeyId)
	}
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, status.Errorf(codes.Internal, "open: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "open: %v", err)
	}
	pt, err := gcm.Open(nil, req.Nonce, req.Ciphertext, nil)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "open: %v", err)
	}
	return &hostv1.SecretValue{Plaintext: pt}, nil
}

// gatedSecrets wraps secretsServer with the secrets:rw permission check
// every method requires.
type gatedSecrets struct {
	hostv1.UnimplementedSecretsServer
	perms  map[string]bool
	packID string
	inner  *secretsServer
}

func (g *gatedSecrets) check() error {
	if !g.perms["secrets:rw"] {
		return ErrPermissionDenied("secrets:rw")
	}
	return nil
}

func (g *gatedSecrets) Store(ctx context.Context, req *hostv1.StoreSecretRequest) (*hostv1.SecretRef, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.Store(ctx, req)
}
func (g *gatedSecrets) Reveal(ctx context.Context, req *hostv1.SecretRef) (*hostv1.SecretValue, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.Reveal(ctx, req)
}
func (g *gatedSecrets) Seal(ctx context.Context, req *hostv1.SecretValue) (*hostv1.Sealed, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.Seal(ctx, req)
}
func (g *gatedSecrets) Open(ctx context.Context, req *hostv1.Sealed) (*hostv1.SecretValue, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.Open(ctx, req)
}
func (g *gatedSecrets) Delete(ctx context.Context, req *hostv1.SecretRef) (*emptypb.Empty, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.Delete(ctx, req)
}
