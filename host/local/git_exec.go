package local

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// execGitClient is the real GitClient. RED skeleton: every method is
// unimplemented.
type execGitClient struct {
	gitPath          string
	allowedProtocols string
	validateURL      func(string) error
	tempRoot         string
	localTimeout     time.Duration
}

func newExecGitClient() *execGitClient { return &execGitClient{} }

func (c *execGitClient) ListBranches(context.Context, GitRemote) ([]GitBranchRef, error) {
	return nil, status.Error(codes.Unimplemented, "git: not implemented")
}

func (c *execGitClient) Open(context.Context, GitRemote, []string) (GitRepo, error) {
	return nil, status.Error(codes.Unimplemented, "git: not implemented")
}
