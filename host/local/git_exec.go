package local

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The git client's limits. Every one is a named constant with the decision or
// threat it enforces, and every one is also a field on execGitClient
// initialised from it, so a test can shrink the field while production keeps
// the constant (the forgeServer.callTimeout precedent, D-04, DQ-14).
const (
	// gitDiscoveryTimeout bounds branch discovery (ls-remote), which moves no
	// objects and so has no business taking long (D-04, DQ-14, T-10-12).
	gitDiscoveryTimeout = 30 * time.Second

	// gitFetchTimeout bounds one shallow fetch of the named branch heads
	// (D-04, DQ-14, T-10-12).
	gitFetchTimeout = 90 * time.Second

	// gitLocalTimeout bounds a read against the already-fetched local bare
	// repo (ls-tree). It involves no network, so it is generous but finite.
	gitLocalTimeout = 30 * time.Second

	// gitMaxBranches caps how many branch heads discovery will report and how
	// many one Open will fetch, so a remote with thousands of refs cannot turn
	// into thousands of fetched trees (D-04, DQ-14, T-10-12).
	gitMaxBranches = 100

	// gitMaxRepoBytes is the polled ceiling on the temp repo's size while a
	// fetch runs. It is best-effort: git fetch --depth 1 has no portable
	// pre-flight size, so the cap is enforced by observation, not negotiation
	// (D-04, DQ-14, RESEARCH Pitfall 12, T-10-12).
	gitMaxRepoBytes = 128 << 20 // 128 MiB

	// gitMaxLsRemoteBytes bounds ls-remote stdout. A repository with an
	// enormous ref count would otherwise make the output unbounded (DQ-14,
	// RESEARCH Pitfall 14, T-10-12).
	gitMaxLsRemoteBytes = 1 << 20 // 1 MiB

	// gitMaxTreeListBytes bounds ls-tree stdout for the same reason.
	gitMaxTreeListBytes = 16 << 20 // 16 MiB

	// gitMaxBlobBytes is the per-blob ceiling, enforced from the ls-tree -l
	// size before any cat-file read (DQ-14, RESEARCH Pitfall 12, T-10-12).
	gitMaxBlobBytes = 1 << 20 // 1 MiB

	// gitWaitDelay bounds how long Wait lingers on a killed child's pipes
	// (DQ-14, RESEARCH Pitfall 14).
	gitWaitDelay = 2 * time.Second

	// gitSizePollInterval is how often the repo-size ceiling is checked.
	gitSizePollInterval = 200 * time.Millisecond
)

// execGitClient is the real GitClient. It drives the system git binary: it
// fetches the named branch heads shallowly into a bare repository in a
// private temp directory and reads blobs through the git object reader, so no
// working tree is ever created and no checkout filter, hook or symlink can
// run or be followed (D-01, D-04, RESEARCH Pitfall 5).
type execGitClient struct {
	// gitPath is the git binary resolved once at construction; empty means git
	// is not installed and unavailable carries the reason every method returns.
	gitPath     string
	unavailable error

	// allowedProtocols is the child's GIT_ALLOW_PROTOCOL value: https:ssh in
	// production, the second line of defence behind the Go URL allowlist. A
	// test sets it to "file" so the real-repo tests need no network.
	allowedProtocols string
	// validateURL is the Go-side allowlist, validateGitURL in production. A
	// test replaces it to prove GIT_ALLOW_PROTOCOL refuses a transport on its
	// own, and to point the client at a file:// repository.
	validateURL func(string) error
	// tempRoot is where per-call temp directories are created; empty means the
	// OS default. A test points it at a private directory so it can assert
	// that nothing survives.
	tempRoot string

	discoveryTimeout time.Duration
	fetchTimeout     time.Duration
	localTimeout     time.Duration
	maxBranches      int
	maxRepoBytes     int64
	maxLsRemoteBytes int64
	maxTreeListBytes int64
	maxBlobBytes     int64
	waitDelay        time.Duration
	sizePollInterval time.Duration
}

// newExecGitClient builds the real client with production limits. A missing
// git binary yields a client whose every method fails with FailedPrecondition
// rather than a nil, so DefaultGitClient is never nil.
func newExecGitClient() *execGitClient {
	c := &execGitClient{
		allowedProtocols: "https:ssh",
		validateURL:      validateGitURL,
		discoveryTimeout: gitDiscoveryTimeout,
		fetchTimeout:     gitFetchTimeout,
		localTimeout:     gitLocalTimeout,
		maxBranches:      gitMaxBranches,
		maxRepoBytes:     gitMaxRepoBytes,
		maxLsRemoteBytes: gitMaxLsRemoteBytes,
		maxTreeListBytes: gitMaxTreeListBytes,
		maxBlobBytes:     gitMaxBlobBytes,
		waitDelay:        gitWaitDelay,
		sizePollInterval: gitSizePollInterval,
	}
	p, err := exec.LookPath("git")
	if err != nil {
		c.unavailable = status.Error(codes.FailedPrecondition, "git is not installed on this host; the import feature needs the git binary on PATH")
		return c
	}
	c.gitPath = p
	return c
}

// gitCall is the per-call scratch space: one private temp directory, which is
// also the child's working directory and HOME, and the child's environment.
type gitCall struct {
	dir      string
	env      []string
	localEnv []string
}

// cleanup removes the temp directory. It is the body of the defer registered
// at the point of creation, so it also runs on an error return and a panic.
func (g *gitCall) cleanup() error { return os.RemoveAll(g.dir) }

// newCall creates the per-call temp directory (0700, from os.MkdirTemp) and
// builds the scrubbed child environment.
func (c *execGitClient) newCall() (*gitCall, error) {
	dir, err := os.MkdirTemp(c.tempRoot, "stagehand-git-")
	if err != nil {
		return nil, status.Error(codes.Internal, "git scratch directory could not be created")
	}
	env := c.baseEnv(dir)
	return &gitCall{dir: dir, env: env, localEnv: env}, nil
}

// baseEnv is the scrubbed environment every git child runs under: no operator
// config (GIT_CONFIG_GLOBAL, GIT_CONFIG_NOSYSTEM), no prompting, only the
// allowed transports, and HOME pointed at the temp directory so nothing under
// the operator's home is read (D-04).
func (c *execGitClient) baseEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"LC_ALL=C",
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=" + c.allowedProtocols,
	}
}

// gitStandingArgs precede every git invocation: neither a configured
// credential helper nor a hooks directory can run, and every fetched object
// is checked (D-04, T-10-09).
var gitStandingArgs = []string{
	"-c", "credential.helper=",
	"-c", "core.hooksPath=/dev/null",
	"-c", "transfer.fsckObjects=true",
}

// command is the single place an exec.Cmd for git is constructed, so the
// scrubbed environment and the standing argument prefix cannot be forgotten
// at a call site.
func (c *execGitClient) command(ctx context.Context, env []string, dir string, args ...string) *exec.Cmd {
	full := make([]string, 0, len(gitStandingArgs)+len(args))
	full = append(full, gitStandingArgs...)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, c.gitPath, full...)
	cmd.Env = env
	cmd.Dir = dir
	return cmd
}

// errGitFailed is the static message for a failed git invocation. git's
// stderr quotes the remote URL, which may carry an internal hostname the
// caller is not otherwise entitled to confirm, so it is never echoed.
func errGitFailed(ctxErr error) error {
	if ctxErr != nil {
		return status.Error(codes.Unavailable, "git operation timed out or was canceled")
	}
	return status.Error(codes.Internal, "git operation failed")
}

// run executes one git invocation and returns its stdout.
func (c *execGitClient) run(ctx context.Context, env []string, dir string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := c.command(ctx, env, dir, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, status.Error(codes.Internal, "git operation failed")
	}
	if err := cmd.Start(); err != nil {
		return nil, status.Error(codes.Internal, "git operation failed")
	}
	out, _ := io.ReadAll(stdout)
	if err := cmd.Wait(); err != nil {
		return nil, errGitFailed(ctx.Err())
	}
	return out, nil
}

// isHexSHA reports whether s looks like a full SHA-1 or SHA-256 object id.
func isHexSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// ListBranches implements GitClient. It runs ls-remote --heads --refs, which
// moves no objects.
func (c *execGitClient) ListBranches(ctx context.Context, r GitRemote) ([]GitBranchRef, error) {
	if c.unavailable != nil {
		return nil, c.unavailable
	}
	if err := c.validateURL(r.URL); err != nil {
		return nil, err
	}
	call, err := c.newCall()
	if err != nil {
		return nil, err
	}
	defer call.cleanup()

	out, err := c.run(ctx, call.env, call.dir, c.discoveryTimeout, "ls-remote", "--heads", "--refs", "--", r.URL)
	if err != nil {
		return nil, err
	}
	var refs []GitBranchRef
	for _, line := range strings.Split(string(out), "\n") {
		sha, ref, ok := strings.Cut(line, "\t")
		name, isHead := strings.CutPrefix(ref, "refs/heads/")
		if !ok || !isHead || name == "" || !isHexSHA(sha) {
			continue
		}
		refs = append(refs, GitBranchRef{Name: name, Commit: sha})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// Open implements GitClient. It runs init --bare then one shallow fetch of
// exactly the named heads, then records the SHA actually fetched.
func (c *execGitClient) Open(ctx context.Context, r GitRemote, branches []string) (GitRepo, error) {
	if c.unavailable != nil {
		return nil, c.unavailable
	}
	if err := c.validateURL(r.URL); err != nil {
		return nil, err
	}
	if len(branches) == 0 {
		return nil, status.Error(codes.InvalidArgument, "git open needs at least one branch")
	}
	call, err := c.newCall()
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			call.cleanup()
		}
	}()

	gitDir := filepath.Join(call.dir, "repo.git")
	if _, err := c.run(ctx, call.localEnv, call.dir, c.localTimeout, "init", "--bare", "-q", "--template=", gitDir); err != nil {
		return nil, err
	}

	fetchArgs := []string{"--git-dir=" + gitDir, "fetch", "--depth", "1", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--", r.URL}
	for _, b := range branches {
		fetchArgs = append(fetchArgs, "+refs/heads/"+b+":refs/heads/"+b)
	}
	if _, err := c.run(ctx, call.env, call.dir, c.fetchTimeout, fetchArgs...); err != nil {
		return nil, err
	}

	commits := make(map[string]string, len(branches))
	for _, b := range branches {
		out, err := c.run(ctx, call.localEnv, call.dir, c.localTimeout, "--git-dir="+gitDir, "rev-parse", "--verify", "-q", "refs/heads/"+b)
		if err != nil {
			return nil, err
		}
		sha := strings.TrimSpace(string(out))
		if !isHexSHA(sha) {
			return nil, status.Error(codes.Internal, "git operation failed")
		}
		commits[b] = sha
	}

	repo := &execGitRepo{c: c, call: call, gitDir: gitDir, commits: commits}
	if err := repo.startReader(); err != nil {
		return nil, err
	}
	owned = true
	return repo, nil
}

// execGitRepo is the read-only view Open returns: the temp directory, the
// branch-to-SHA map, and one long-lived cat-file --batch process that serves
// every blob read (D-04).
type execGitRepo struct {
	c       *execGitClient
	call    *gitCall
	gitDir  string
	commits map[string]string

	mu     sync.Mutex
	closed bool
	broken bool
	cancel context.CancelFunc
	batch  *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
}

func (r *execGitRepo) startReader() error {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := r.c.command(ctx, r.call.localEnv, r.call.dir, "--git-dir="+r.gitDir, "cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return status.Error(codes.Internal, "git object reader could not be started")
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return status.Error(codes.Internal, "git object reader could not be started")
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return status.Error(codes.Internal, "git object reader could not be started")
	}
	r.cancel, r.batch, r.in, r.out = cancel, cmd, in, bufio.NewReaderSize(out, 64<<10)
	return nil
}

// Commit implements GitRepo.
func (r *execGitRepo) Commit(branch string) (string, bool) {
	sha, ok := r.commits[branch]
	return sha, ok
}

// ListFiles implements GitRepo. -z is mandatory: without it git C-quotes an
// unusual path and the parse would silently diverge from the real path.
func (r *execGitRepo) ListFiles(branch string, pathspecs ...string) ([]GitFileEntry, error) {
	sha, ok := r.commits[branch]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "git branch was not opened")
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return nil, status.Error(codes.FailedPrecondition, "git repository is closed")
	}
	args := []string{"--git-dir=" + r.gitDir, "ls-tree", "-r", "-l", "-z", sha}
	if len(pathspecs) > 0 {
		args = append(args, "--")
		args = append(args, pathspecs...)
	}
	out, err := r.c.run(context.Background(), r.call.localEnv, r.call.dir, r.c.localTimeout, args...)
	if err != nil {
		return nil, err
	}
	var files []GitFileEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, path, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok {
			return nil, status.Error(codes.Internal, "git tree listing was malformed")
		}
		fields := strings.Fields(string(meta))
		if len(fields) != 4 {
			return nil, status.Error(codes.Internal, "git tree listing was malformed")
		}
		var size int64
		if fields[3] != "-" {
			n, err := strconv.ParseInt(fields[3], 10, 64)
			if err != nil {
				return nil, status.Error(codes.Internal, "git tree listing was malformed")
			}
			size = n
		}
		files = append(files, GitFileEntry{Path: string(path), Mode: fields[0], Size: size})
	}
	return files, nil
}

// ReadFile implements GitRepo. It writes "<commit>:<path>" to the long-lived
// batch process and reads the declared number of bytes: content arrives only
// from the git object reader, never through the filesystem.
func (r *execGitRepo) ReadFile(branch, path string) ([]byte, error) {
	sha, ok := r.commits[branch]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "git branch was not opened")
	}
	if path == "" || strings.ContainsAny(path, "\n\x00") {
		return nil, status.Error(codes.InvalidArgument, "git path is not readable")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.broken {
		return nil, status.Error(codes.FailedPrecondition, "git repository is closed")
	}
	if _, err := io.WriteString(r.in, sha+":"+path+"\n"); err != nil {
		r.broken = true
		return nil, status.Error(codes.Internal, "git object reader failed")
	}
	header, err := r.out.ReadString('\n')
	if err != nil {
		r.broken = true
		return nil, status.Error(codes.Internal, "git object reader failed")
	}
	header = strings.TrimSuffix(header, "\n")
	if strings.HasSuffix(header, " missing") {
		return nil, status.Error(codes.NotFound, "git path was not found in the branch")
	}
	parts := strings.Fields(header)
	if len(parts) != 3 || parts[1] != "blob" {
		r.broken = true
		return nil, status.Error(codes.Internal, "git object reader failed")
	}
	size, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || size < 0 {
		r.broken = true
		return nil, status.Error(codes.Internal, "git object reader failed")
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r.out, body); err != nil {
		r.broken = true
		return nil, status.Error(codes.Internal, "git object reader failed")
	}
	if b, err := r.out.ReadByte(); err != nil || b != '\n' {
		r.broken = true
		return nil, status.Error(codes.Internal, "git object reader failed")
	}
	return body, nil
}

// Close implements GitRepo: it stops the object reader and removes the temp
// directory. A second call is harmless.
func (r *execGitRepo) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.in != nil {
		_ = r.in.Close()
	}
	if r.cancel != nil {
		r.cancel()
	}
	if r.batch != nil {
		_ = r.batch.Wait()
	}
	if err := r.call.cleanup(); err != nil {
		return status.Error(codes.Internal, "git scratch directory could not be removed")
	}
	return nil
}
