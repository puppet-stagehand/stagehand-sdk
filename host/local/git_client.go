package local

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
// invocation: the plaintext of a sealed Secret, revealed just before a fetch
// and never persisted. Kind says which fields apply: Username and Token for
// an HTTPS token, PrivateKey for an SSH key. No field may reach a log, a
// status message, argv, a URL or a formatted struct dump, so the type redacts
// every secret field from every fmt verb exactly as LLMProvider redacts its
// API key; only Kind stays visible.
type GitCredential struct {
	Kind       GitCredentialKind
	Username   string
	Token      string
	PrivateKey string
}

// String implements fmt.Stringer without any secret material.
func (c GitCredential) String() string {
	return fmt.Sprintf("GitCredential{Kind:%q Username:[redacted] Token:[redacted] PrivateKey:[redacted]}", string(c.Kind))
}

// GoString implements fmt.GoStringer without any secret material.
func (c GitCredential) GoString() string { return c.String() }

// Format implements fmt.Formatter so %v, %+v and %#v all stay redacted, also
// when the credential is nested inside another struct.
func (c GitCredential) Format(f fmt.State, _ rune) { _, _ = fmt.Fprint(f, c.String()) }

// GitRemote names a remote and, optionally, the credential to present to it.
// A nil Credential means an anonymous fetch.
type GitRemote struct {
	URL        string
	Credential *GitCredential
}

// GitClient is the injectable git seam, the only place this host touches a
// network it did not configure. host.Local wires the real system-git client
// by default (DefaultGitClient); tests and pack examples inject an in-memory
// fixture with WithGitClient so no other test needs a network. Every method
// takes the remote explicitly rather than binding one at construction, as
// ForgeClient does with its endpoint.
type GitClient interface {
	// ListBranches reports the branch heads the remote advertises, and only
	// those: tags and every other ref are absent. It transfers no objects, so
	// it is cheap and says nothing about a branch's content.
	ListBranches(ctx context.Context, r GitRemote) ([]GitBranchRef, error)
	// Open fetches exactly the named branch heads at depth 1 and returns a
	// read-only view of them. The view has no working tree, so no checkout
	// filter or hook can run and no symlink can be followed. The caller must
	// Close it.
	Open(ctx context.Context, r GitRemote, branches []string) (GitRepo, error)
}

// GitRepo is a read-only view of the branches Open fetched.
type GitRepo interface {
	// Commit reports the commit SHA actually fetched for branch, which is
	// authoritative over the SHA ListBranches reported: the branch may have
	// moved between the two calls.
	Commit(branch string) (string, bool)
	// ListFiles returns every blob under branch's root recursively, narrowed
	// to the given path prefixes when any are named. Entries with mode 120000
	// or 160000 are reported, not hidden, so the caller decides what to do
	// with them.
	ListFiles(branch string, pathspecs ...string) ([]GitFileEntry, error)
	// ReadFile returns a blob's committed bytes. It reads the git object, not
	// a file, so it never follows a symlink: for a 120000 entry it returns the
	// link target string.
	ReadFile(branch, path string) ([]byte, error)
	// Close always removes the repo's temp directory and stops its helper
	// process. It is safe to call twice.
	Close() error
}

// DefaultGitClient returns the real system-git-backed GitClient host.Local
// wires in by default. It adds no Go dependency: it drives the git binary on
// PATH, which is a documented runtime prerequisite.
func DefaultGitClient() GitClient { return newExecGitClient() }

// reGitSCPClone matches the scp-style user@host:path form accepted as the ssh
// scheme (DQ-12). Its first character must be alphanumeric, so a value that
// begins with "-" can never match and be read as an option, and its path may
// not begin with "-" either.
var reGitSCPClone = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9.-]*:[^\s:-][^\s]*$`)

// reGitHelperTransport matches git's "<transport>::<address>" remote-helper
// syntax (ext::, fd::, and any other helper), which can execute a command.
var reGitHelperTransport = regexp.MustCompile(`^[A-Za-z0-9+.-]+::`)

// errGitURL builds the static InvalidArgument every URL refusal returns. A
// refusal never quotes the input: url.Parse's own error text does, userinfo
// included, so that error is never wrapped.
func errGitURL(msg string) error { return status.Error(codes.InvalidArgument, "git url "+msg) }

// validateGitURL is the Go-side allowlist the exec client runs before any
// subprocess (D-04, DQ-12, RESEARCH Pitfall 6, T-10-08). It accepts https://,
// ssh:// and the scp-style user@host:path form, and refuses everything else:
// an empty value, whitespace or control bytes, a leading "-", helper
// transports (ext::, fd::), file://, git:// and http://, a dash-led host, and
// any credential in the URL.
//
// A credential in a URL is a credential in a process listing (D-02), so https
// refuses any userinfo and ssh:// refuses a password. An ssh:// URL may carry
// a bare login name (ssh://git@host:2222/org/repo.git): it is a user name, not
// a secret, it is the only way to name an ssh port, and it is the same thing
// the accepted scp-style form already carries.
//
// This is deliberately not code.gitURLOK, which governs what the Puppetfile
// model may store, accepts file://, git:// and http://, and must not be reused
// as a clone allowlist. GIT_ALLOW_PROTOCOL in the child's environment is the
// second line of defence behind this one.
func validateGitURL(raw string) error {
	if raw == "" {
		return errGitURL("is empty")
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c <= 0x20 || c == 0x7f {
			return errGitURL("must not contain whitespace or control characters")
		}
	}
	if raw[0] == '-' {
		return errGitURL("must not start with '-'")
	}
	if reGitHelperTransport.MatchString(raw) {
		return errGitURL("must not use a remote-helper transport")
	}
	if !strings.Contains(raw, "://") {
		if !reGitSCPClone.MatchString(raw) {
			return errGitURL("must use https or ssh")
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errGitURL("is not a valid URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "ssh" {
		return errGitURL("must use https or ssh")
	}
	host := u.Hostname()
	if host == "" {
		return errGitURL("must include a host")
	}
	if strings.HasPrefix(host, "-") {
		return errGitURL("has a host that starts with '-'")
	}
	if u.User != nil {
		_, hasPassword := u.User.Password()
		name := u.User.Username()
		if scheme == "https" || hasPassword || name == "" || strings.HasPrefix(name, "-") {
			return errGitURL("must not embed credentials")
		}
	}
	return nil
}

// validGitBranchName applies git's ref-name rules conservatively, plus a
// refusal of a leading "-", so a branch name interpolated into a refspec can
// never change the refspec's meaning or be read as an option.
func validGitBranchName(n string) bool {
	if n == "" || len(n) > 255 || n == "@" || n[0] == '-' {
		return false
	}
	if strings.HasPrefix(n, "/") || strings.HasSuffix(n, "/") || strings.HasSuffix(n, ".") || strings.HasSuffix(n, ".lock") {
		return false
	}
	if strings.Contains(n, "..") || strings.Contains(n, "//") || strings.Contains(n, "@{") {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		if c <= 0x20 || c == 0x7f || strings.IndexByte(":?[\\^~*", c) >= 0 {
			return false
		}
	}
	for _, comp := range strings.Split(n, "/") {
		if strings.HasPrefix(comp, ".") || strings.HasSuffix(comp, ".lock") {
			return false
		}
	}
	return true
}
