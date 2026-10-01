package local

import (
	"context"
)

// GitBranchRef is one branch head a remote advertises: its short name (no
// "refs/heads/" prefix) and the commit SHA the remote reported for it.
type GitBranchRef struct {
	Name   string
	Commit string
}

// GitFileEntry is one blob (or gitlink) in a fetched branch's tree. Path is
// slash-separated exactly as git reports it. Mode is git's octal mode string,
// so a caller can tell a regular file (100644/100755) from a symlink (120000)
// or a submodule pointer (160000) and decide what to do with it; this seam
// never follows either.
type GitFileEntry struct {
	Path string
	Mode string
	Size int64
}

// GitCredentialKind names which of a GitCredential's fields are meaningful.
type GitCredentialKind string

const (
	// GitCredentialHTTPSToken is an HTTPS username plus access token.
	GitCredentialHTTPSToken GitCredentialKind = "https_token"
	// GitCredentialSSHKey is an unencrypted SSH private key.
	GitCredentialSSHKey GitCredentialKind = "ssh_key"
)

// GitCredential is a materialised credential the host holds for one
// invocation.
type GitCredential struct {
	Kind       GitCredentialKind
	Username   string
	Token      string
	PrivateKey string
}

// GitRemote names a remote and, optionally, the credential to present to it.
// A nil Credential means an anonymous fetch.
type GitRemote struct {
	URL        string
	Credential *GitCredential
}

// GitClient is the injectable git seam.
type GitClient interface {
	ListBranches(ctx context.Context, r GitRemote) ([]GitBranchRef, error)
	Open(ctx context.Context, r GitRemote, branches []string) (GitRepo, error)
}

// GitRepo is a read-only view of the branches Open fetched.
type GitRepo interface {
	Commit(branch string) (string, bool)
	ListFiles(branch string, pathspecs ...string) ([]GitFileEntry, error)
	ReadFile(branch, path string) ([]byte, error)
	Close() error
}

// DefaultGitClient returns the real git client.
func DefaultGitClient() GitClient { return newExecGitClient() }
