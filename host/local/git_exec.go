package local

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	// gitVersionTimeout bounds the one git --version probe made at
	// construction (DQ-11).
	gitVersionTimeout = 10 * time.Second

	// gitStderrCapture bounds how much of git's stderr is held for the
	// substring checks that classify a failure. It is never returned.
	gitStderrCapture = 64 << 10

	// gitReaderBuffer sizes the buffer on the cat-file --batch reader.
	gitReaderBuffer = 64 << 10

	// gitMinMajor and gitMinMinor are the oldest git this client will run:
	// 2.32 is the first release that honours GIT_CONFIG_GLOBAL. An older git
	// would silently read the operator's global config, so the client fails
	// closed instead (DQ-11, RESEARCH A2, T-10-14).
	gitMinMajor = 2
	gitMinMinor = 32
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
	// knownHostsPath is the one file from the operator's home that an ssh child
	// may read: the host keys StrictHostKeyChecking=yes verifies against. It is
	// resolved once at construction and handed to ssh as an explicit
	// -o UserKnownHostsFile= argument, so the child's HOME can stay the scratch
	// directory. It is /dev/null when the home directory cannot be worked out,
	// which makes every host unverifiable and so refused (DQ-7-R-a). A test may
	// point it at a scratch file.
	knownHostsPath string

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
	versionTimeout   time.Duration
	stderrCapture    int
}

// newExecGitClient builds the real client with production limits. A missing
// git binary yields a client whose every method fails with FailedPrecondition
// rather than a nil, so DefaultGitClient is never nil.
func newExecGitClient() *execGitClient {
	c := &execGitClient{
		allowedProtocols: "https:ssh",
		validateURL:      validateGitURL,
		knownHostsPath:   defaultKnownHostsPath(),
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
		versionTimeout:   gitVersionTimeout,
		stderrCapture:    gitStderrCapture,
	}
	p, err := exec.LookPath("git")
	if err != nil {
		c.unavailable = status.Error(codes.FailedPrecondition, "git is not installed on this host; the import feature needs the git binary on PATH")
		return c
	}
	c.gitPath = p
	c.unavailable = c.probeVersion()
	return c
}

// reGitVersion extracts major and minor from "git version 2.50.1 (Apple
// Git-155)" and its cousins.
var reGitVersion = regexp.MustCompile(`^git version (\d+)\.(\d+)`)

// probeVersion runs git --version once and returns nil when the binary is at
// least the minimum, or the FailedPrecondition every method will then return
// (DQ-11, T-10-14). An unparseable version fails closed too: not knowing the
// version is not a reason to read the operator's global config.
func (c *execGitClient) probeVersion() error {
	tooOld := status.Errorf(codes.FailedPrecondition,
		"git %d.%d or newer is required; an older git ignores GIT_CONFIG_GLOBAL and would read the operator's global git config",
		gitMinMajor, gitMinMinor)
	ctx, cancel := context.WithTimeout(context.Background(), c.versionTimeout)
	defer cancel()
	cmd := c.command(ctx, c.baseEnv(os.TempDir()), os.TempDir(), "--version")
	out, err := cmd.Output()
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "git could not be run to check its version; git %d.%d or newer is required", gitMinMajor, gitMinMinor)
	}
	m := reGitVersion.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return tooOld
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < gitMinMajor || (major == gitMinMajor && minor < gitMinMinor) {
		return tooOld
	}
	return nil
}

// gitCall is the per-call scratch space: one private temp directory, which is
// also the child's working directory and HOME on every path, and the child's
// environments. env is the network environment and
// may carry a credential; localEnv never does and is the only environment the
// long-lived repo reader keeps.
type gitCall struct {
	dir      string
	credDir  string
	env      []string
	localEnv []string
}

// cleanup removes the temp directory. It is the body of the defer registered
// at the point of creation, so it also runs on an error return and a panic.
func (g *gitCall) cleanup() error { return os.RemoveAll(g.dir) }

// dropCredentials removes the askpass script and the key file and forgets the
// network environment, so a credential exists for the duration of one network
// invocation and not for the lifetime of an opened repo.
func (g *gitCall) dropCredentials() {
	if g.credDir != "" {
		_ = os.RemoveAll(g.credDir)
	}
	g.env = nil
}

// isSSHRemote reports whether raw would be dialled over ssh: an ssh:// URL or
// the scp-style user@host:path form.
func isSSHRemote(raw string) bool {
	if strings.Contains(raw, "://") {
		return strings.HasPrefix(strings.ToLower(raw), "ssh://")
	}
	return true
}

// checkCredential refuses a credential that cannot be used safely with the
// URL it accompanies, before any temp directory or file exists. Messages are
// static and never quote a credential field (D-02, T-10-11).
func checkCredential(r GitRemote) error {
	cred := r.Credential
	if cred == nil {
		return nil
	}
	bad := func(why string) error { return status.Error(codes.InvalidArgument, "git credential "+why) }
	if strings.ContainsAny(cred.Username, "\x00\r\n") || strings.ContainsAny(cred.Token, "\x00\r\n") || strings.Contains(cred.PrivateKey, "\x00") {
		return bad("is not usable")
	}
	ssh := isSSHRemote(r.URL)
	switch cred.Kind {
	case GitCredentialHTTPSToken:
		if ssh {
			return bad("does not fit an ssh url")
		}
		if cred.Token == "" {
			return bad("is not usable")
		}
	case GitCredentialSSHKey:
		if !ssh {
			return bad("does not fit an https url")
		}
		if cred.PrivateKey == "" {
			return bad("is not usable")
		}
	default:
		return bad("is not usable")
	}
	return nil
}

// askpassScript is the static GIT_ASKPASS helper. It holds no secret: it
// prints the username or token from the child's environment depending on
// which prompt git passes it. git asks "Username for ..." first and
// "Password for ..." second.
const askpassScript = `#!/bin/sh
case "$1" in
  [Uu]sername*) printf '%s\n' "$STAGEHAND_GIT_USERNAME" ;;
  *) printf '%s\n' "$STAGEHAND_GIT_TOKEN" ;;
esac
`

// defaultKnownHostsPath resolves the operator's conventional known_hosts file
// once, at client construction. It is the only thing an ssh child learns about
// the operator's home directory. When the home directory cannot be determined
// it returns /dev/null: every host then fails verification, which is the
// documented host-key refusal rather than a laxer policy. Not knowing something
// is never a reason to widen (DQ-7-R-a, the DQ-11 precedent, T-10-37).
func defaultKnownHostsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/dev/null"
	}
	return filepath.Join(home, ".ssh", "known_hosts")
}

// sshCommand builds GIT_SSH_COMMAND. ssh is configured entirely from this
// command line and reads nothing else: -F /dev/null drops the operator's ssh
// configuration (and the system-wide one), IdentityAgent=none drops any agent,
// and exactly one identity selector is given. With no credential named that is
// IdentityFile=none, which is what makes an empty credential name a genuinely
// anonymous fetch instead of implicitly the operator (D-02, CR-01, T-10-50).
// With a credential it is -i on the 0600 key file, which suppresses ssh's
// default ~/.ssh/id_* list only while that file is readable: the host writes
// the key before building this command, and the test that proves it uses a file
// that exists. IdentitiesOnly=yes stops ssh offering anything beyond that one
// identity. The key travels as a path, never as content.
//
// Host-key verification is unchanged and strict: StrictHostKeyChecking=yes
// against the single explicit knownHosts file, BatchMode=yes forbids any
// prompt, and an unknown host is refused rather than trusted on first use
// (T-10-37, T-10-52). Both paths are single-quoted because git runs this string
// through a shell.
func sshCommand(keyPath, knownHosts string) string {
	identity := "-o IdentityFile=none"
	if keyPath != "" {
		identity = "-i " + shellSingleQuote(keyPath)
	}
	return "ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=yes" +
		" -o UserKnownHostsFile=" + shellSingleQuote(knownHosts) +
		" -o IdentityAgent=none -o IdentitiesOnly=yes " + identity
}

// shellSingleQuote quotes s for the POSIX shell git runs GIT_SSH_COMMAND with.
func shellSingleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// newCall creates the per-call temp directory (0700, from os.MkdirTemp), the
// scrubbed child environments, and any credential files. A credential reaches
// git by exactly two routes and no others: an https token through a GIT_ASKPASS
// script in a 0700 directory that reads STAGEHAND_GIT_USERNAME and
// STAGEHAND_GIT_TOKEN from the child's environment, and an ssh key through a
// 0600 file named by GIT_SSH_COMMAND -i. Never argv, never the URL, never
// persisted (D-02, RESEARCH Pitfall 7, T-10-11).
//
// HOME is the temp directory on every path, ssh included. An earlier design
// passed the operator's real HOME to the ssh child so known_hosts was readable
// and claimed nothing else read it. That claim was false: ssh resolved the
// operator's ~/.ssh/config, agent socket and default ~/.ssh/id_* keys, so a
// credential-less import fetched private repositories as the operator (CR-01).
// Now the one thing ssh needs from the operator's home, the known_hosts file,
// crosses as an explicit -o UserKnownHostsFile= argument (c.knownHostsPath) and
// nothing else does (D-02, D-04, DQ-7-R).
//
// Documented fallback, not used: git >= 2.31 can carry an https token as
// GIT_CONFIG_COUNT/GIT_CONFIG_KEY_0=http.<url>.extraHeader. It would need URL
// scoping so the header is not re-sent after a redirect, which is why askpass,
// where git itself decides which host receives the secret, is the chosen route.
func (c *execGitClient) newCall(r GitRemote) (*gitCall, error) {
	dir, err := os.MkdirTemp(c.tempRoot, "stagehand-git-")
	if err != nil {
		return nil, status.Error(codes.Internal, "git scratch directory could not be created")
	}
	call := &gitCall{dir: dir, localEnv: c.baseEnv(dir), env: c.baseEnv(dir)}

	fail := func() (*gitCall, error) {
		call.cleanup()
		return nil, status.Error(codes.Internal, "git credential could not be prepared")
	}
	keyPath := ""
	if cred := r.Credential; cred != nil {
		call.credDir = filepath.Join(dir, "cred")
		if err := os.Mkdir(call.credDir, 0o700); err != nil {
			return fail()
		}
		switch cred.Kind {
		case GitCredentialHTTPSToken:
			script := filepath.Join(call.credDir, "askpass.sh")
			if err := writeFileMode(script, []byte(askpassScript), 0o700); err != nil {
				return fail()
			}
			call.env = append(call.env,
				"GIT_ASKPASS="+script,
				"STAGEHAND_GIT_USERNAME="+cred.Username,
				"STAGEHAND_GIT_TOKEN="+cred.Token,
			)
		case GitCredentialSSHKey:
			keyPath = filepath.Join(call.credDir, "id_key")
			key := cred.PrivateKey
			if !strings.HasSuffix(key, "\n") {
				key += "\n" // ssh refuses a key without a trailing newline
			}
			if err := writeFileMode(keyPath, []byte(key), 0o600); err != nil {
				return fail()
			}
		}
	}
	if isSSHRemote(r.URL) {
		call.env = append(call.env, "GIT_SSH_COMMAND="+sshCommand(keyPath, c.knownHostsPath))
	}
	return call, nil
}

// writeFileMode writes data with exactly mode, whatever the umask.
func writeFileMode(path string, data []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// baseEnv is the scrubbed environment every git child runs under: no operator
// config (GIT_CONFIG_GLOBAL, GIT_CONFIG_NOSYSTEM), no prompting, only the
// allowed transports, and HOME pointed at the per-call temp directory on every
// path, ssh included, so nothing under the operator's home is reachable by a
// child; the one known_hosts file ssh needs is named explicitly instead
// (D-04, DQ-7-R).
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
	// exec.CommandContext kills only the direct child, and an ssh grandchild
	// would outlive the deadline holding the pipes. Run the child in its own
	// process group and kill the whole group on timeout or cancellation, with
	// WaitDelay as the backstop for a pipe that stays open (RESEARCH Pitfall
	// 14, T-10-12).
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = c.waitDelay
	return cmd
}

// classifyGitFailure turns a failed invocation into a host-authored static
// error. git's stderr quotes the remote URL, which may carry an internal
// hostname the caller is not otherwise entitled to confirm, so stderr is
// inspected by substring comparison only and never echoed (D-02, 7 D-03,
// T-10-11). The ladder, in order:
//
//	context canceled                                -> Canceled
//	context deadline exceeded                       -> Unavailable
//	refused transport, strange hostname, bad URL    -> InvalidArgument
//	branch gone from the remote                     -> FailedPrecondition
//	ssh host key not verified                       -> FailedPrecondition
//	rejected or missing credential, repo not found  -> FailedPrecondition
//	unreachable host, connection or TLS failure     -> Unavailable
//	anything unexpected                             -> Internal
func classifyGitFailure(stderr []byte, ctxErr error) error {
	switch {
	case errors.Is(ctxErr, context.Canceled):
		return status.Error(codes.Canceled, "git operation was canceled")
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return status.Error(codes.Unavailable, "git operation timed out")
	}
	e := strings.ToLower(string(stderr))
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(e, s) {
				return true
			}
		}
		return false
	}
	switch {
	case (has("transport '") && has("not allowed")) || has("strange hostname", "strange pathname", "unable to find remote helper", "protocol is not allowed"):
		return status.Error(codes.InvalidArgument, "git transport or url was refused")
	case has("couldn't find remote ref"):
		return status.Error(codes.FailedPrecondition, "git branch is no longer on the remote")
	case has("host key verification failed"):
		return status.Error(codes.FailedPrecondition, "git ssh host key could not be verified against the host's known_hosts")
	case has("authentication failed", "could not read username", "could not read password", "terminal prompts disabled",
		"permission denied", "invalid username or password", "error: 401", "error: 403", "access denied"):
		return status.Error(codes.FailedPrecondition, "git remote rejected the credential or requires one")
	case has("error: 404", "repository not found", "does not appear to be a git repository"):
		return status.Error(codes.FailedPrecondition, "git repository was not found or is not readable with the supplied credential")
	case has("could not resolve host", "could not resolve hostname", "connection refused", "connection timed out", "timed out",
		"network is unreachable", "no route to host", "failed to connect", "couldn't connect", "connection reset",
		"ssl", "tls", "unable to access", "could not read from remote repository", "early eof", "remote end hung up"):
		return status.Error(codes.Unavailable, "git remote is unreachable (network error or timeout)")
	}
	return status.Error(codes.Internal, "git operation failed")
}

// boundedBuffer keeps at most limit bytes of a stream and silently discards
// the rest, so a chatty git cannot grow host memory through stderr.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.buf.Write(p[:room])
	}
	return len(p), nil
}

// dirSize sums the sizes of the regular files under root. It never follows a
// symlink and ignores entries that vanish mid-walk.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// gitRun describes the bounds on one invocation.
type gitRun struct {
	// timeout bounds the whole invocation.
	timeout time.Duration
	// stdoutLimit bounds stdout; one byte past it is read so an oversized
	// payload is detected rather than silently truncated.
	stdoutLimit int64
	// watchDir, when set, is polled while the process runs and the process
	// group is killed once its size passes maxDirBytes. Best-effort: git
	// fetch --depth 1 has no portable pre-flight size, so the ceiling is
	// enforced by observation, not negotiation (RESEARCH Pitfall 12).
	watchDir    string
	maxDirBytes int64
}

// run executes one git invocation and returns its stdout. Every failure is
// already classified into a static error; nothing it returns echoes the URL,
// git's stderr or a credential.
func (c *execGitClient) run(ctx context.Context, env []string, dir string, o gitRun, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	cmd := c.command(ctx, env, dir, args...)
	stderr := &boundedBuffer{limit: c.stderrCapture}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, status.Error(codes.Internal, "git operation failed")
	}
	if err := cmd.Start(); err != nil {
		return nil, status.Error(codes.Internal, "git operation failed")
	}

	var sizeTripped atomic.Bool
	done := make(chan struct{})
	var watchers sync.WaitGroup
	if o.watchDir != "" {
		watchers.Add(1)
		go func() {
			defer watchers.Done()
			t := time.NewTicker(c.sizePollInterval)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					if dirSize(o.watchDir) > o.maxDirBytes {
						sizeTripped.Store(true)
						_ = killProcessGroup(cmd)
						return
					}
				}
			}
		}()
	}

	var data []byte
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		data, _ = io.ReadAll(io.LimitReader(stdout, o.stdoutLimit+1))
	}()
	select {
	case <-readDone:
	case <-ctx.Done():
		// The group kill closes every pipe writer; WaitDelay is the backstop
		// for a descendant that escaped the group and still holds the pipe.
		select {
		case <-readDone:
		case <-time.After(c.waitDelay):
			_ = stdout.Close()
			<-readDone
		}
	}
	over := int64(len(data)) > o.stdoutLimit
	if over {
		_ = killProcessGroup(cmd)
	}
	waitErr := cmd.Wait()
	close(done)
	watchers.Wait()

	switch {
	case sizeTripped.Load():
		return nil, status.Errorf(codes.FailedPrecondition, "git fetch exceeded the repository size limit of %d bytes", o.maxDirBytes)
	case over:
		return nil, status.Errorf(codes.FailedPrecondition, "git output exceeded the limit of %d bytes", o.stdoutLimit)
	case waitErr != nil:
		return nil, classifyGitFailure(stderr.buf.Bytes(), ctx.Err())
	}
	if o.watchDir != "" && dirSize(o.watchDir) > o.maxDirBytes {
		return nil, status.Errorf(codes.FailedPrecondition, "git fetch exceeded the repository size limit of %d bytes", o.maxDirBytes)
	}
	return data, nil
}

// hasControlByte reports whether s holds a space, control byte or DEL, none of
// which a real branch name can contain; a name that does came from a hostile
// remote and is dropped from discovery.
func hasControlByte(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
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
	if err := checkCredential(r); err != nil {
		return nil, err
	}
	call, err := c.newCall(r)
	if err != nil {
		return nil, err
	}
	defer call.cleanup()

	out, err := c.run(ctx, call.env, call.dir, gitRun{timeout: c.discoveryTimeout, stdoutLimit: c.maxLsRemoteBytes},
		"ls-remote", "--heads", "--refs", "--", r.URL)
	if err != nil {
		return nil, err
	}
	var refs []GitBranchRef
	for _, line := range strings.Split(string(out), "\n") {
		sha, ref, ok := strings.Cut(line, "\t")
		name, isHead := strings.CutPrefix(ref, "refs/heads/")
		if !ok || !isHead || name == "" || !isHexSHA(sha) || hasControlByte(name) {
			continue
		}
		refs = append(refs, GitBranchRef{Name: name, Commit: sha})
	}
	if len(refs) > c.maxBranches {
		return nil, status.Errorf(codes.FailedPrecondition, "git remote has more branches than the limit of %d", c.maxBranches)
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
	if err := checkCredential(r); err != nil {
		return nil, err
	}
	if len(branches) == 0 {
		return nil, status.Error(codes.InvalidArgument, "git open needs at least one branch")
	}
	// Dedupe, then validate every name before any temp directory exists: a
	// name is interpolated into a refspec, so it must not be able to change
	// the refspec's meaning.
	seen := make(map[string]bool, len(branches))
	uniq := make([]string, 0, len(branches))
	for _, b := range branches {
		if !validGitBranchName(b) {
			return nil, status.Error(codes.InvalidArgument, "git branch name is not valid")
		}
		if !seen[b] {
			seen[b] = true
			uniq = append(uniq, b)
		}
	}
	branches = uniq
	if len(branches) > c.maxBranches {
		return nil, status.Errorf(codes.FailedPrecondition, "git open names more branches than the limit of %d", c.maxBranches)
	}
	call, err := c.newCall(r)
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
	if _, err := c.run(ctx, call.localEnv, call.dir, gitRun{timeout: c.localTimeout, stdoutLimit: c.maxLsRemoteBytes}, "init", "--bare", "-q", "--template=", gitDir); err != nil {
		return nil, err
	}

	fetchArgs := []string{"--git-dir=" + gitDir, "fetch", "--depth", "1", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--", r.URL}
	for _, b := range branches {
		fetchArgs = append(fetchArgs, "+refs/heads/"+b+":refs/heads/"+b)
	}
	fetch := gitRun{timeout: c.fetchTimeout, stdoutLimit: c.maxLsRemoteBytes, watchDir: call.dir, maxDirBytes: c.maxRepoBytes}
	_, fetchErr := c.run(ctx, call.env, call.dir, fetch, fetchArgs...)
	// The credential has done its one job; do not hold it for the repo's life.
	call.dropCredentials()
	if fetchErr != nil {
		return nil, fetchErr
	}

	commits := make(map[string]string, len(branches))
	for _, b := range branches {
		out, err := c.run(ctx, call.localEnv, call.dir, gitRun{timeout: c.localTimeout, stdoutLimit: c.maxLsRemoteBytes},
			"--git-dir="+gitDir, "rev-parse", "--verify", "-q", "refs/heads/"+b)
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

	mu sync.Mutex
	// sizes records each listed blob's size from ls-tree -l, so the per-blob
	// ceiling refuses an oversized blob before any cat-file read.
	sizes  map[string]int64
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
	r.cancel, r.batch, r.in, r.out = cancel, cmd, in, bufio.NewReaderSize(out, gitReaderBuffer)
	return nil
}

// Commit implements GitRepo.
func (r *execGitRepo) Commit(branch string) (string, bool) {
	sha, ok := r.commits[branch]
	return sha, ok
}

// ListFiles implements GitRepo. -z is mandatory: without it git C-quotes an
// unusual path and the parse would silently diverge from the real path. The
// mode is surfaced verbatim: a committed symlink is 120000 and a submodule
// pointer is 160000, and this client never follows either (T-10-10); the caller
// decides what to do with them.
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
	out, err := r.c.run(context.Background(), r.call.localEnv, r.call.dir, gitRun{timeout: r.c.localTimeout, stdoutLimit: r.c.maxTreeListBytes}, args...)
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
	r.mu.Lock()
	if r.sizes == nil {
		r.sizes = map[string]int64{}
	}
	for _, f := range files {
		r.sizes[sha+"\x00"+f.Path] = f.Size
	}
	r.mu.Unlock()
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
	errTooBig := status.Errorf(codes.FailedPrecondition, "git file exceeds the per-file limit of %d bytes", r.c.maxBlobBytes)
	if known, ok := r.sizes[sha+"\x00"+path]; ok && known > r.c.maxBlobBytes {
		return nil, errTooBig
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
	if size > r.c.maxBlobBytes {
		// Skip the body so the stream stays in sync for the next read. Bounded
		// by the repo-size ceiling the fetch already enforced.
		if _, err := io.CopyN(io.Discard, r.out, size+1); err != nil {
			r.broken = true
		}
		return nil, errTooBig
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
