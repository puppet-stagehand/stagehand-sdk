package index

import (
	"context"
	"net/http"

	"github.com/google/go-containerregistry/pkg/authn"
)

// CopyOptions are the inputs of Copy.
//
// RED stub: the real implementation replaces this file in the next commit.
type CopyOptions struct {
	From      string
	To        string
	FromAuth  authn.Authenticator
	ToAuth    authn.Authenticator
	Transport http.RoundTripper
}

// CopyResult is a successful copy.
type CopyResult struct {
	Digest         string `json:"digest"`
	AlreadyPresent bool   `json:"already_present"`
}

// Copy is a stub.
func Copy(ctx context.Context, opts CopyOptions) (*CopyResult, []Finding) {
	return &CopyResult{}, nil
}
