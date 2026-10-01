package local

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newTestExecClient builds the real client pointed at a fresh private temp
// root, allowing the file transport and skipping the Go-side URL allowlist so
// the real-repo tests need no network. The hardening tests assert the two
// layers separately.
func newTestExecClient(t *testing.T) *execGitClient {
	t.Helper()
	requireGit(t)
	c := newExecGitClient()
	c.allowedProtocols = "file"
	c.validateURL = func(string) error { return nil }
	c.tempRoot = t.TempDir()
	return c
}

// assertTempRootEmpty fails when a per-call temp directory survived.
func assertTempRootEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temp directory survived: %v", names)
	}
}

func TestGitExec_EndToEnd(t *testing.T) {
	requireGit(t)
	repo := newTempBareRepo(t)
	ctx := context.Background()

	t.Run("DefaultGitClient is non-nil", func(t *testing.T) {
		if DefaultGitClient() == nil {
			t.Fatal("DefaultGitClient returned nil")
		}
	})

	t.Run("New defaults the Code facet to the real client", func(t *testing.T) {
		h := New([]string{"code:rw"}, "fixture")
		inner := h.Code.(*gatedCode).inner
		if inner.git == nil {
			t.Fatal("Code facet holds a nil git client")
		}
		if _, ok := inner.git.(*execGitClient); !ok {
			t.Fatalf("Code facet git client = %T, want *execGitClient", inner.git)
		}
		if inner.secrets == nil {
			t.Fatal("Code facet was not given the secrets server")
		}
	})

	t.Run("New honours WithGitClient", func(t *testing.T) {
		fx := newGitFixture(map[string]map[string]string{"production": {"a": "b"}})
		h := New([]string{"code:rw"}, "fixture", WithGitClient(fx))
		if got := h.Code.(*gatedCode).inner.git; got != GitClient(fx) {
			t.Fatalf("Code facet git client = %T, want the injected fixture", got)
		}
	})

	t.Run("ListBranches returns both branches and no other refs", func(t *testing.T) {
		c := newTestExecClient(t)
		got, err := c.ListBranches(ctx, GitRemote{URL: repo.URL})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"dev": repo.Commits["dev"], "production": repo.Commits["production"]}
		if len(got) != len(want) {
			t.Fatalf("got %d branches %+v, want exactly %v (no tags, no refs/remotes)", len(got), got, want)
		}
		for _, b := range got {
			if want[b.Name] != b.Commit {
				t.Fatalf("branch %q commit %q, want %q", b.Name, b.Commit, want[b.Name])
			}
		}
		assertTempRootEmpty(t, c.tempRoot)
	})

	t.Run("Open reports the fetched SHA", func(t *testing.T) {
		c := newTestExecClient(t)
		r, err := c.Open(ctx, GitRemote{URL: repo.URL}, []string{"production"})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		sha, ok := r.Commit("production")
		if !ok || sha != repo.Commits["production"] {
			t.Fatalf("Commit(production) = %q, %v; want %q", sha, ok, repo.Commits["production"])
		}
		if _, ok := r.Commit("dev"); ok {
			t.Fatal("dev was not fetched but Commit reports it")
		}
	})

	t.Run("Open with an unknown branch errors without leaking", func(t *testing.T) {
		c := newTestExecClient(t)
		r, err := c.Open(ctx, GitRemote{URL: repo.URL}, []string{"no_such_branch"})
		if err == nil {
			r.Close()
			t.Fatal("Open of a missing branch succeeded")
		}
		msg := err.Error()
		if strings.Contains(msg, repo.URL) || strings.Contains(msg, repo.Path) {
			t.Fatalf("error leaks the remote URL: %q", msg)
		}
		for _, frag := range []string{"couldn't find remote ref", "fatal:", "no_such_branch"} {
			if strings.Contains(msg, frag) {
				t.Fatalf("error leaks git stderr (%q): %q", frag, msg)
			}
		}
		assertTempRootEmpty(t, c.tempRoot)
	})

	t.Run("ListFiles returns every blob recursively", func(t *testing.T) {
		c := newTestExecClient(t)
		r, err := c.Open(ctx, GitRemote{URL: repo.URL}, []string{"production"})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		files, err := r.ListFiles("production")
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]GitFileEntry{}
		for _, f := range files {
			got[f.Path] = f
		}
		for path, body := range map[string]string{
			"Puppetfile":       tempRepoProdPuppetfile,
			"data/common.yaml": tempRepoProdCommonYAML,
			"hiera.yaml":       "---\nversion: 5\n",
		} {
			e, ok := got[path]
			if !ok {
				t.Fatalf("ListFiles is missing %q: %+v", path, files)
			}
			if e.Mode != "100644" || e.Size != int64(len(body)) {
				t.Fatalf("%s entry = %+v, want mode 100644 size %d", path, e, len(body))
			}
		}
		if len(files) != 3 {
			t.Fatalf("got %d entries, want 3: %+v", len(files), files)
		}

		sub, err := r.ListFiles("production", "data")
		if err != nil {
			t.Fatal(err)
		}
		if len(sub) != 1 || sub[0].Path != "data/common.yaml" {
			t.Fatalf("ListFiles(data) = %+v", sub)
		}
		none, err := r.ListFiles("production", "no/such/prefix")
		if err != nil {
			t.Fatal(err)
		}
		if len(none) != 0 {
			t.Fatalf("ListFiles of a non-matching prefix = %+v, want nothing", none)
		}
		if _, err := r.ListFiles("dev"); err == nil {
			t.Fatal("ListFiles of a branch that was not opened succeeded")
		}
	})

	t.Run("ReadFile returns committed bytes byte-for-byte", func(t *testing.T) {
		c := newTestExecClient(t)
		r, err := c.Open(ctx, GitRemote{URL: repo.URL}, []string{"production", "dev"})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for _, tc := range []struct{ branch, path, want string }{
			{"production", "Puppetfile", tempRepoProdPuppetfile},
			{"production", "data/common.yaml", tempRepoProdCommonYAML},
			{"dev", "Puppetfile", tempRepoDevPuppetfile},
		} {
			got, err := r.ReadFile(tc.branch, tc.path)
			if err != nil {
				t.Fatalf("ReadFile(%s,%s): %v", tc.branch, tc.path, err)
			}
			if string(got) != tc.want {
				t.Fatalf("ReadFile(%s,%s) = %q, want %q", tc.branch, tc.path, got, tc.want)
			}
		}
	})

	t.Run("ReadFile of a missing path errors with no bytes", func(t *testing.T) {
		c := newTestExecClient(t)
		r, err := c.Open(ctx, GitRemote{URL: repo.URL}, []string{"production"})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		got, err := r.ReadFile("production", "no/such/file")
		if err == nil || got != nil {
			t.Fatalf("ReadFile of a missing path = %q, %v; want nil bytes and an error", got, err)
		}
		// The long-lived object reader must still be usable afterwards.
		if _, err := r.ReadFile("production", "Puppetfile"); err != nil {
			t.Fatalf("object reader unusable after a miss: %v", err)
		}
	})

	t.Run("Close removes the temp directory and is idempotent", func(t *testing.T) {
		c := newTestExecClient(t)
		r, err := c.Open(ctx, GitRemote{URL: repo.URL}, []string{"production"})
		if err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(c.tempRoot)
		if err != nil || len(entries) != 1 {
			t.Fatalf("expected one live temp directory while open, got %v (err %v)", entries, err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		assertTempRootEmpty(t, c.tempRoot)
		if err := r.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		if _, err := r.ReadFile("production", "Puppetfile"); err == nil {
			t.Fatal("ReadFile after Close succeeded")
		}
	})

	t.Run("reachable from a host built by New", func(t *testing.T) {
		c := newTestExecClient(t)
		h := New([]string{"code:rw"}, "fixture", WithGitClient(c))
		g := h.Code.(*gatedCode).inner.git
		if g != GitClient(c) {
			t.Fatalf("Code facet git client = %T, want the injected client", g)
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		refs, err := g.ListBranches(cctx, GitRemote{URL: repo.URL})
		if err != nil || len(refs) != 2 {
			t.Fatalf("ListBranches via the Code facet: %+v, %v", refs, err)
		}
		r, err := g.Open(cctx, GitRemote{URL: repo.URL}, []string{"production"})
		if err != nil {
			t.Fatal(err)
		}
		files, err := r.ListFiles("production")
		if err != nil || len(files) != 3 {
			t.Fatalf("ListFiles via the Code facet: %+v, %v", files, err)
		}
		body, err := r.ReadFile("production", "hiera.yaml")
		if err != nil || string(body) != "---\nversion: 5\n" {
			t.Fatalf("ReadFile via the Code facet: %q, %v", body, err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		assertTempRootEmpty(t, c.tempRoot)
	})
}

// TestGitCredentialRedacts proves every fmt entry point masks the secret
// material of a GitCredential, including when nested in a GitRemote.
func TestGitCredentialRedacts(t *testing.T) {
	const token = "TOKEN-SENTINEL-do-not-print"
	const keyLine = "KEYBODY-SENTINEL-AAAAB3NzaC1yc2E"
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\n" + keyLine + "\n-----END OPENSSH PRIVATE KEY-----\n"
	cred := GitCredential{Kind: GitCredentialHTTPSToken, Username: "user-sentinel", Token: token, PrivateKey: key}

	check := func(t *testing.T, label, out string) {
		t.Helper()
		for _, secret := range []string{token, keyLine, "BEGIN OPENSSH", "user-sentinel"} {
			if strings.Contains(out, secret) {
				t.Fatalf("%s leaks %q: %s", label, secret, out)
			}
		}
		if !strings.Contains(out, string(GitCredentialHTTPSToken)) {
			t.Fatalf("%s hides the kind: %s", label, out)
		}
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		check(t, "value "+verb, fmt.Sprintf(verb, cred))
		check(t, "pointer "+verb, fmt.Sprintf(verb, &cred))
		check(t, "nested "+verb, fmt.Sprintf(verb, GitRemote{URL: "https://example.invalid/x.git", Credential: &cred}))
	}
	s, ok := any(cred).(fmt.Stringer)
	if !ok {
		t.Fatal("GitCredential does not implement fmt.Stringer")
	}
	check(t, "String", s.String())
	gs, ok := any(cred).(fmt.GoStringer)
	if !ok {
		t.Fatal("GitCredential does not implement fmt.GoStringer")
	}
	check(t, "GoString", gs.GoString())
}

// ----------------------------------------------------------- shim helpers

// shellQuote single-quotes s for a POSIX shell script.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// installGitShim puts a `git` script first on PATH. The script runs body (a
// POSIX shell fragment that may exit) and then execs the real git, so a test
// can log what the client sent or simulate a hung fetch while every other
// subcommand still works. It must be installed before the client is built:
// the client resolves git from PATH at construction.
func installGitShim(t *testing.T, body string) {
	t.Helper()
	real := requireGit(t)
	if runtime.GOOS == "windows" {
		t.Skip("the git shim needs a POSIX shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nREAL=" + shellQuote(real) + "\n" + body + "\nexec \"$REAL\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// pidAlive reports whether pid names a live process.
func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// waitPIDGone polls until pid is gone or the deadline passes.
func waitPIDGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !pidAlive(pid)
}

// readPID reads the pid a shim wrote, waiting briefly for it to appear.
func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("shim never wrote a pid to %s", path)
	return 0
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s error, got nil", want)
	}
	if got := status.Code(err); got != want {
		t.Fatalf("code = %s (%v), want %s", got, err, want)
	}
}

// wantNoLeak fails when an error message contains any forbidden fragment.
func wantNoLeak(t *testing.T, err error, forbidden ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, f := range forbidden {
		if f != "" && strings.Contains(err.Error(), f) {
			t.Fatalf("error message leaks %q: %q", f, err.Error())
		}
	}
}

// hangingFetchShim simulates a fetch that never finishes: it starts a
// grandchild sleep, records its pid and waits. Every other subcommand falls
// through to the real git. The pid file is cleaned up with a kill so a failing
// test cannot leave a sleeper behind.
func hangingFetchShim(t *testing.T) (pidFile string) {
	t.Helper()
	pidFile = filepath.Join(t.TempDir(), "sleeper.pid")
	installGitShim(t, `for a in "$@"; do
  if [ "$a" = fetch ]; then
    sleep 8 &
    echo $! > `+shellQuote(pidFile)+`
    wait
    exit 1
  fi
done`)
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	return pidFile
}

// ------------------------------------------------------- URL refusal

func TestGitExec_URLRefusal(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	refused := []string{
		"",
		"-oProxyCommand=touch/tmp/pwn",
		"--upload-pack=touch /tmp/pwn",
		"https://exa mple.com/x.git",
		"https://example.com/a\tb.git",
		"https://example.com/a\nb.git",
		"https://example.com/a\x01b.git",
		"https://user@example.com/x.git",
		"https://user:pw@example.com/x.git",
		"ssh://user:pw@example.com/x.git",
		"file:///etc/passwd",
		"git://example.com/x.git",
		"http://example.com/x.git",
		"ext::sh -c touch% /tmp/pwn",
		"ext::foo",
		"fd::3",
		"-user@example.com:path",
		"_user@example.com:path",
		"user@example.com:-oProxyCommand=x",
		"ssh://-oProxyCommand=touch/x/y",
		"https://-example.com/x.git",
		"/srv/git/local-repo",
	}
	for _, in := range refused {
		for _, method := range []string{"ListBranches", "Open"} {
			t.Run(method+"/"+fmt.Sprintf("%q", in), func(t *testing.T) {
				c := newExecGitClient()
				// A temp root that cannot be created proves the URL is refused
				// first: reaching newCall would surface Internal instead.
				c.tempRoot = filepath.Join(t.TempDir(), "does-not-exist")
				var err error
				if method == "ListBranches" {
					_, err = c.ListBranches(ctx, GitRemote{URL: in})
				} else {
					_, err = c.Open(ctx, GitRemote{URL: in}, []string{"production"})
				}
				wantCode(t, err, codes.InvalidArgument)
				if len(in) >= 4 {
					wantNoLeak(t, err, in)
				}
			})
		}
	}
	for _, ok := range []string{
		"https://example.com/org/repo.git",
		"ssh://git@example.com:2222/org/repo.git",
		"ssh://example.com/org/repo.git",
		"git@example.com:org/repo.git",
	} {
		if err := validateGitURL(ok); err != nil {
			t.Errorf("validateGitURL(%q) = %v, want accepted", ok, err)
		}
	}
}

// ------------------------------------------- classification and layers

func TestGitExec_Unreachable(t *testing.T) {
	requireGit(t)
	c := newExecGitClient()
	c.tempRoot = t.TempDir()
	const u = "https://127.0.0.1:1/org/secret-internal.git"
	_, err := c.ListBranches(context.Background(), GitRemote{URL: u})
	wantCode(t, err, codes.Unavailable)
	wantNoLeak(t, err, u, "127.0.0.1", "secret-internal", "Failed to connect", "fatal")
	assertTempRootEmpty(t, c.tempRoot)
}

func TestGitExec_ProtocolLayers(t *testing.T) {
	requireGit(t)
	repo := newTempBareRepo(t)
	ctx := context.Background()

	t.Run("the Go validator refuses file:// first", func(t *testing.T) {
		c := newExecGitClient()
		c.tempRoot = t.TempDir()
		_, err := c.ListBranches(ctx, GitRemote{URL: repo.URL})
		wantCode(t, err, codes.InvalidArgument)
		wantNoLeak(t, err, repo.URL, repo.Path)
	})

	t.Run("GIT_ALLOW_PROTOCOL refuses file:// with the validator bypassed", func(t *testing.T) {
		c := newExecGitClient()
		c.tempRoot = t.TempDir()
		c.validateURL = func(string) error { return nil } // bypass layer one
		for _, call := range []func() error{
			func() error { _, err := c.ListBranches(ctx, GitRemote{URL: repo.URL}); return err },
			func() error { _, err := c.Open(ctx, GitRemote{URL: repo.URL}, []string{"production"}); return err },
		} {
			err := call()
			wantCode(t, err, codes.InvalidArgument)
			wantNoLeak(t, err, repo.URL, repo.Path, "not allowed")
		}
		assertTempRootEmpty(t, c.tempRoot)
	})

	t.Run("git refuses a dash-led ssh host with the validator bypassed", func(t *testing.T) {
		c := newExecGitClient()
		c.tempRoot = t.TempDir()
		c.validateURL = func(string) error { return nil }
		_, err := c.ListBranches(ctx, GitRemote{URL: "ssh://-oProxyCommand=touch/x/y"})
		wantCode(t, err, codes.InvalidArgument)
		wantNoLeak(t, err, "oProxyCommand")
	})

	t.Run("an unknown branch is FailedPrecondition with a static message", func(t *testing.T) {
		c := newTestExecClient(t)
		_, err := c.Open(ctx, GitRemote{URL: repo.URL}, []string{"no_such_branch"})
		wantCode(t, err, codes.FailedPrecondition)
		wantNoLeak(t, err, repo.URL, "no_such_branch", "couldn't find")
	})

	t.Run("an invalid branch name is refused before any subprocess", func(t *testing.T) {
		c := newTestExecClient(t)
		c.tempRoot = filepath.Join(t.TempDir(), "does-not-exist")
		for _, b := range []string{"", "a b", "a:b", "a..b", "-x", "/x", "x/", "x.lock", "a\nb", "a*b", "a@{b"} {
			_, err := c.Open(ctx, GitRemote{URL: repo.URL}, []string{b})
			wantCode(t, err, codes.InvalidArgument)
		}
	})
}

// ------------------------------------------ symlink and gitlink refusal

func TestGitExec_SymlinkAndGitlink(t *testing.T) {
	requireGit(t)
	hostFile := filepath.Join(t.TempDir(), "host-secret.txt")
	const hostSecret = "HOST-SECRET-SENTINEL-must-never-be-read"
	if err := os.WriteFile(hostFile, []byte(hostSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := newTempBareRepo(t, func(w *tempWork) {
		w.symlink("data/evil.yaml", hostFile)
		sub := &tempWork{t: t, dir: filepath.Join(w.dir, "vendor", "sub")}
		if err := os.MkdirAll(sub.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		sub.git("init", "-q")
		sub.write("x.txt", "1")
		sub.commitAll("sub")
	})
	c := newTestExecClient(t)
	r, err := c.Open(context.Background(), GitRemote{URL: repo.URL}, []string{"production"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	files, err := r.ListFiles("production")
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]string{}
	for _, f := range files {
		modes[f.Path] = f.Mode
	}
	if modes["data/evil.yaml"] != "120000" {
		t.Fatalf("symlink mode = %q, want 120000 (entries %+v)", modes["data/evil.yaml"], files)
	}
	if modes["vendor/sub"] != "160000" {
		t.Fatalf("gitlink mode = %q, want 160000 (entries %+v)", modes["vendor/sub"], files)
	}

	got, err := r.ReadFile("production", "data/evil.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), hostSecret) {
		t.Fatal("ReadFile followed the symlink and returned a host file")
	}
	if string(got) != hostFile {
		t.Fatalf("ReadFile of a symlink = %q, want the link target string %q", got, hostFile)
	}
	if b, err := r.ReadFile("production", "vendor/sub"); err == nil || b != nil {
		t.Fatalf("ReadFile of a gitlink = %q, %v; want an error", b, err)
	}
	if _, err := r.ReadFile("production", "Puppetfile"); err != nil {
		t.Fatalf("object reader unusable after a gitlink read: %v", err)
	}
}

// ------------------------------------------------------------- caps

func TestGitExec_Caps(t *testing.T) {
	requireGit(t)
	repo := newTempBareRepo(t)
	ctx := context.Background()

	t.Run("branch discovery cap", func(t *testing.T) {
		c := newTestExecClient(t)
		c.maxBranches = 1
		_, err := c.ListBranches(ctx, GitRemote{URL: repo.URL})
		wantCode(t, err, codes.FailedPrecondition)
		if !strings.Contains(err.Error(), "limit of 1") {
			t.Fatalf("message does not name the cap: %q", err.Error())
		}
		_, err = c.Open(ctx, GitRemote{URL: repo.URL}, []string{"production", "dev"})
		wantCode(t, err, codes.FailedPrecondition)
		assertTempRootEmpty(t, c.tempRoot)
	})

	t.Run("ls-remote stdout ceiling is detected, not truncated", func(t *testing.T) {
		c := newTestExecClient(t)
		c.maxLsRemoteBytes = 10
		_, err := c.ListBranches(ctx, GitRemote{URL: repo.URL})
		wantCode(t, err, codes.FailedPrecondition)
		if !strings.Contains(err.Error(), "limit") {
			t.Fatalf("message does not name the limit: %q", err.Error())
		}
		assertTempRootEmpty(t, c.tempRoot)
	})

	t.Run("per-blob ceiling", func(t *testing.T) {
		c := newTestExecClient(t)
		c.maxBlobBytes = 20
		r, err := c.Open(ctx, GitRemote{URL: repo.URL}, []string{"production"})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		// Before any ListFiles: the object header carries the size.
		if b, err := r.ReadFile("production", "Puppetfile"); err == nil || b != nil {
			t.Fatalf("oversized blob = %q, %v; want an error and no bytes", b, err)
		}
		// The stream stayed in sync, so a small blob still reads.
		if b, err := r.ReadFile("production", "hiera.yaml"); err != nil || string(b) != "---\nversion: 5\n" {
			t.Fatalf("small blob after a refusal = %q, %v", b, err)
		}
		// After ListFiles the ls-tree -l size refuses it before any read.
		if _, err := r.ListFiles("production"); err != nil {
			t.Fatal(err)
		}
		if b, err := r.ReadFile("production", "Puppetfile"); err == nil || b != nil {
			t.Fatalf("oversized blob after ListFiles = %q, %v; want an error and no bytes", b, err)
		}
	})

	t.Run("repo size ceiling kills the fetch", func(t *testing.T) {
		installGitShim(t, `for a in "$@"; do
  if [ "$a" = fetch ]; then
    dd if=/dev/zero of=./bloat bs=1024 count=4096 2>/dev/null
    sleep 8
    exit 1
  fi
done`)
		c := newTestExecClient(t)
		c.maxRepoBytes = 1 << 20
		c.sizePollInterval = 5 * time.Millisecond
		start := time.Now()
		_, err := c.Open(ctx, GitRemote{URL: repo.URL}, []string{"production"})
		wantCode(t, err, codes.FailedPrecondition)
		if !strings.Contains(err.Error(), strconv.Itoa(1<<20)) {
			t.Fatalf("message does not name the ceiling: %q", err.Error())
		}
		if d := time.Since(start); d > 4*time.Second {
			t.Fatalf("size ceiling took %s to trip", d)
		}
		assertTempRootEmpty(t, c.tempRoot)
	})
}

// -------------------------------------------------------- lifecycle

func TestGitExec_Lifecycle(t *testing.T) {
	requireGit(t)
	repo := newTempBareRepo(t)

	t.Run("a fetch timeout kills the process group and removes the temp dir", func(t *testing.T) {
		pidFile := hangingFetchShim(t)
		c := newTestExecClient(t)
		c.fetchTimeout = 300 * time.Millisecond
		c.waitDelay = 200 * time.Millisecond
		start := time.Now()
		_, err := c.Open(context.Background(), GitRemote{URL: repo.URL}, []string{"production"})
		wantCode(t, err, codes.Unavailable)
		if d := time.Since(start); d > 4*time.Second {
			t.Fatalf("timeout took %s to return", d)
		}
		pid := readPID(t, pidFile)
		if !waitPIDGone(pid, 2*time.Second) {
			t.Fatal("the grandchild outlived the timeout")
		}
		assertTempRootEmpty(t, c.tempRoot)
	})

	t.Run("a cancelled context returns promptly and cleans up", func(t *testing.T) {
		pidFile := hangingFetchShim(t)
		c := newTestExecClient(t)
		c.waitDelay = 200 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(300 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		_, err := c.Open(ctx, GitRemote{URL: repo.URL}, []string{"production"})
		wantCode(t, err, codes.Canceled)
		if d := time.Since(start); d > 4*time.Second {
			t.Fatalf("cancellation took %s to return", d)
		}
		pid := readPID(t, pidFile)
		if !waitPIDGone(pid, 2*time.Second) {
			t.Fatal("the grandchild outlived the cancellation")
		}
		assertTempRootEmpty(t, c.tempRoot)
	})
}

// ----------------------------------------------------------- old git

func TestGitExec_OldGit(t *testing.T) {
	requireGit(t)
	repo := newTempBareRepo(t)
	ctx := context.Background()

	shimReporting := func(t *testing.T, version string) (calls string) {
		t.Helper()
		calls = filepath.Join(t.TempDir(), "calls.log")
		installGitShim(t, `for a in "$@"; do
  if [ "$a" = "--version" ]; then
    echo `+shellQuote(version)+`
    exit 0
  fi
done
echo "$@" >> `+shellQuote(calls))
		return calls
	}

	for _, v := range []string{"git version 2.31.9", "git version 1.8.3", "git version banana"} {
		t.Run("fails closed on "+v, func(t *testing.T) {
			calls := shimReporting(t, v)
			c := newTestExecClient(t)
			_, err := c.ListBranches(ctx, GitRemote{URL: repo.URL})
			wantCode(t, err, codes.FailedPrecondition)
			if !strings.Contains(err.Error(), "2.32") {
				t.Fatalf("message does not name the minimum: %q", err.Error())
			}
			r, err := c.Open(ctx, GitRemote{URL: repo.URL}, []string{"production"})
			if r != nil {
				r.Close()
			}
			wantCode(t, err, codes.FailedPrecondition)
			if b, _ := os.ReadFile(calls); len(b) != 0 {
				t.Fatalf("git was exec'd beyond the version probe: %q", b)
			}
		})
	}

	t.Run("the minimum itself is accepted", func(t *testing.T) {
		installGitShim(t, `for a in "$@"; do
  if [ "$a" = "--version" ]; then
    echo "git version 2.32.0"
    exit 0
  fi
done`)
		c := newTestExecClient(t)
		if _, err := c.ListBranches(ctx, GitRemote{URL: repo.URL}); err != nil {
			t.Fatalf("git 2.32.0 was refused: %v", err)
		}
	})
}

// ------------------------------------------------ credential hygiene

const (
	hygieneUser  = "ci-bot"
	hygieneToken = "TOKEN-SENTINEL-s3cr3t-value"
	hygieneKey   = "-----BEGIN OPENSSH PRIVATE KEY-----\nKEYBODY-SENTINEL-AAAAB3NzaC1yc2EAAAADAQABAAABAQ\n-----END OPENSSH PRIVATE KEY-----\n"
)

// newAuthGitServer serves repoPath over real smart HTTP through git
// http-backend, behind HTTP Basic auth, so the https credential path is
// exercised end to end including a shallow fetch (RESEARCH A9). It is plain
// http, so the tests that use it set allowedProtocols to "http" and bypass the
// Go validator; the protocol allowlist itself is proven elsewhere.
func newAuthGitServer(t *testing.T, repoPath, user, token string) *httptest.Server {
	t.Helper()
	gitBin := requireGit(t)
	out, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path failed: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend is not installed (%s); the authenticated smart-HTTP tests need it", backend)
	}
	h := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Dir(repoPath),
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_NOSYSTEM=1",
			"HOME=" + t.TempDir(),
			"PATH=" + os.Getenv("PATH"),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="stagehand-test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newHTTPTestClient is newTestExecClient for the plain-http auth server.
func newHTTPTestClient(t *testing.T) *execGitClient {
	t.Helper()
	c := newTestExecClient(t)
	c.allowedProtocols = "http"
	return c
}

// loggingShim installs a git shim that, for every invocation, appends a block
// to the returned log: the argv, the askpass script's mode, the ssh key's
// path and mode, and the full environment. It is how the credential tests
// observe what a real invocation was actually handed.
func loggingShim(t *testing.T) (logPath string) {
	t.Helper()
	logPath = filepath.Join(t.TempDir(), "git-calls.log")
	installGitShim(t, `{
  echo "=== ARGV: $*"
  if [ -n "$GIT_ASKPASS" ]; then echo "ASKPASS-PATH: $GIT_ASKPASS"; echo "ASKPASS-LS: $(ls -l "$GIT_ASKPASS" 2>&1)"; fi
  if [ -n "$GIT_SSH_COMMAND" ]; then
    key=$(printf '%s' "$GIT_SSH_COMMAND" | sed -n "s/.* -i '\([^']*\)'.*/\1/p")
    if [ -n "$key" ]; then echo "KEY-PATH: $key"; echo "KEY-LS: $(ls -l "$key" 2>&1)"; fi
  fi
  env | sort
} >> `+shellQuote(logPath))
	return logPath
}

// shimBlock is one logged git invocation.
type shimBlock struct {
	argv  string
	lines []string
}

func (b shimBlock) has(prefix string) (string, bool) {
	for _, l := range b.lines {
		if v, ok := strings.CutPrefix(l, prefix); ok {
			return v, true
		}
	}
	return "", false
}

func readShimLog(t *testing.T, path string) []shimBlock {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the shim logged nothing: %v", err)
	}
	var blocks []shimBlock
	for _, chunk := range strings.Split(string(raw), "=== ARGV: ")[1:] {
		lines := strings.Split(strings.TrimRight(chunk, "\n"), "\n")
		blocks = append(blocks, shimBlock{argv: lines[0], lines: lines[1:]})
	}
	if len(blocks) == 0 {
		t.Fatal("the shim log has no invocations")
	}
	return blocks
}

func TestGitExec_CredentialHygiene(t *testing.T) {
	requireGit(t)
	repo := newTempBareRepo(t)

	t.Run("https token reaches git only through the environment", func(t *testing.T) {
		srv := newAuthGitServer(t, repo.Path, hygieneUser, hygieneToken)
		logPath := loggingShim(t)
		c := newHTTPTestClient(t)
		remote := GitRemote{
			URL:        srv.URL + "/" + filepath.Base(repo.Path),
			Credential: &GitCredential{Kind: GitCredentialHTTPSToken, Username: hygieneUser, Token: hygieneToken},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		refs, err := c.ListBranches(ctx, remote)
		if err != nil || len(refs) != 2 {
			t.Fatalf("authenticated ListBranches = %+v, %v", refs, err)
		}
		r, err := c.Open(ctx, remote, []string{"production"})
		if err != nil {
			t.Fatalf("authenticated Open: %v", err)
		}
		body, err := r.ReadFile("production", "Puppetfile")
		if err != nil || string(body) != tempRepoProdPuppetfile {
			t.Fatalf("authenticated ReadFile = %q, %v", body, err)
		}

		blocks := readShimLog(t, logPath)
		sawToken := false
		for _, b := range blocks {
			if strings.Contains(b.argv, hygieneToken) {
				t.Fatalf("the token is in argv: %q", b.argv)
			}
			if strings.Contains(b.argv, hygieneUser+"@") || strings.Contains(b.argv, "://"+hygieneUser) {
				t.Fatalf("userinfo is in the url: %q", b.argv)
			}
			if _, ok := b.has("STAGEHAND_GIT_TOKEN=" + hygieneToken); ok {
				sawToken = true
				// Only network invocations may carry it.
				if !strings.Contains(b.argv, " fetch ") && !strings.Contains(b.argv, "ls-remote") {
					t.Fatalf("the token reached a local-only invocation: %q", b.argv)
				}
				if v, _ := b.has("HOME="); strings.HasPrefix(v, os.Getenv("HOME")) && os.Getenv("HOME") != "" {
					t.Fatalf("HOME on the https path is the real home: %q", v)
				}
				if v, _ := b.has("HOME="); !strings.HasPrefix(v, c.tempRoot) {
					t.Fatalf("HOME on the https path = %q, want inside %q", v, c.tempRoot)
				}
				ls, ok := b.has("ASKPASS-LS: ")
				if !ok || !strings.HasPrefix(ls, "-rwx------") {
					t.Fatalf("askpass script listing = %q, want mode 0700", ls)
				}
				if v, _ := b.has("STAGEHAND_GIT_USERNAME="); v != hygieneUser {
					t.Fatalf("username was not passed by environment: %q", v)
				}
				if !strings.Contains(b.argv, "--") {
					t.Fatalf("network invocation has no -- terminator: %q", b.argv)
				}
			}
		}
		if !sawToken {
			t.Fatal("the token never reached a git invocation, so the credential path was not exercised")
		}
		// The script is removed as soon as the fetch finishes, not at Close.
		for _, b := range blocks {
			if p, ok := b.has("ASKPASS-PATH: "); ok {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Fatalf("askpass script survived the call: %s (%v)", p, err)
				}
			}
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		assertTempRootEmpty(t, c.tempRoot)
	})

	t.Run("a url carrying userinfo is refused before any credential handling", func(t *testing.T) {
		c := newExecGitClient()
		c.tempRoot = filepath.Join(t.TempDir(), "does-not-exist")
		_, err := c.ListBranches(context.Background(), GitRemote{
			URL:        "https://" + hygieneUser + ":" + hygieneToken + "@example.com/x.git",
			Credential: &GitCredential{Kind: GitCredentialHTTPSToken, Username: hygieneUser, Token: hygieneToken},
		})
		wantCode(t, err, codes.InvalidArgument)
		wantNoLeak(t, err, hygieneToken, hygieneUser)
	})

	t.Run("a credential that does not fit the url is refused", func(t *testing.T) {
		c := newExecGitClient()
		c.tempRoot = filepath.Join(t.TempDir(), "does-not-exist")
		for _, remote := range []GitRemote{
			{URL: "ssh://git@example.com/x.git", Credential: &GitCredential{Kind: GitCredentialHTTPSToken, Username: "u", Token: hygieneToken}},
			{URL: "https://example.com/x.git", Credential: &GitCredential{Kind: GitCredentialSSHKey, PrivateKey: hygieneKey}},
			{URL: "https://example.com/x.git", Credential: &GitCredential{Kind: GitCredentialHTTPSToken, Username: "u"}},
			{URL: "https://example.com/x.git", Credential: &GitCredential{Kind: GitCredentialHTTPSToken, Username: "u", Token: "a\nb"}},
			{URL: "ssh://git@example.com/x.git", Credential: &GitCredential{Kind: GitCredentialSSHKey}},
			{URL: "ssh://git@example.com/x.git", Credential: &GitCredential{Kind: "mystery", Token: hygieneToken}},
		} {
			_, err := c.ListBranches(context.Background(), remote)
			wantCode(t, err, codes.InvalidArgument)
			wantNoLeak(t, err, hygieneToken, "KEYBODY", "a\nb")
		}
	})

	t.Run("no credential, and a wrong credential, both read as FailedPrecondition", func(t *testing.T) {
		srv := newAuthGitServer(t, repo.Path, hygieneUser, hygieneToken)
		c := newHTTPTestClient(t)
		url := srv.URL + "/" + filepath.Base(repo.Path)
		host := strings.TrimPrefix(srv.URL, "http://")

		_, errNone := c.ListBranches(context.Background(), GitRemote{URL: url})
		wantCode(t, errNone, codes.FailedPrecondition)
		wantNoLeak(t, errNone, url, host, "terminal prompts", "Username", "fatal")

		wrong := &GitCredential{Kind: GitCredentialHTTPSToken, Username: hygieneUser, Token: "not-the-token"}
		_, errWrong := c.ListBranches(context.Background(), GitRemote{URL: url, Credential: wrong})
		wantCode(t, errWrong, codes.FailedPrecondition)
		wantNoLeak(t, errWrong, url, host, "not-the-token", "Authentication failed", "fatal")

		r, errOpen := c.Open(context.Background(), GitRemote{URL: url, Credential: wrong}, []string{"production"})
		if r != nil {
			r.Close()
		}
		wantCode(t, errOpen, codes.FailedPrecondition)
		if errNone.Error() != errWrong.Error() {
			t.Fatalf("the two rejections read differently (%q vs %q)", errNone, errWrong)
		}
		assertTempRootEmpty(t, c.tempRoot)
	})

	t.Run("ssh key is a 0600 file named by GIT_SSH_COMMAND under a strict host-key policy", func(t *testing.T) {
		if _, err := exec.LookPath("ssh"); err != nil {
			t.Skip("ssh is not on PATH; the ssh credential path needs it to be invoked")
		}
		logPath := loggingShim(t)
		c := newExecGitClient()
		c.tempRoot = t.TempDir()
		remote := GitRemote{
			URL:        "ssh://git@127.0.0.1:1/org/repo.git",
			Credential: &GitCredential{Kind: GitCredentialSSHKey, PrivateKey: hygieneKey},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := c.ListBranches(ctx, remote)
		if err == nil {
			t.Fatal("a fetch from a closed port succeeded")
		}
		wantNoLeak(t, err, "127.0.0.1", "KEYBODY", "BEGIN OPENSSH")

		var net shimBlock
		found := false
		for _, b := range readShimLog(t, logPath) {
			if strings.Contains(b.argv, "ls-remote") {
				net, found = b, true
			}
		}
		if !found {
			t.Fatal("the ls-remote invocation was not logged")
		}
		cmdline, ok := net.has("GIT_SSH_COMMAND=")
		if !ok {
			t.Fatal("GIT_SSH_COMMAND was not set on the ssh path")
		}
		keyPath, _ := net.has("KEY-PATH: ")
		if keyPath == "" || !strings.Contains(cmdline, keyPath) {
			t.Fatalf("GIT_SSH_COMMAND %q does not name the key file %q", cmdline, keyPath)
		}
		for _, want := range []string{"StrictHostKeyChecking=yes", "IdentitiesOnly=yes", "BatchMode=yes"} {
			if !strings.Contains(cmdline, want) {
				t.Fatalf("GIT_SSH_COMMAND %q lacks %s", cmdline, want)
			}
		}
		if ls, _ := net.has("KEY-LS: "); !strings.HasPrefix(ls, "-rw-------") {
			t.Fatalf("key file listing = %q, want mode 0600", ls)
		}
		if v, _ := net.has("HOME="); v != os.Getenv("HOME") {
			t.Fatalf("HOME on the ssh path = %q, want the operator's %q so known_hosts is readable", v, os.Getenv("HOME"))
		}
		if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
			t.Fatalf("the key file survived the call: %s (%v)", keyPath, err)
		}
		raw, _ := os.ReadFile(logPath)
		if strings.Contains(string(raw), "KEYBODY") {
			t.Fatal("key material appears in argv or the environment")
		}
		assertTempRootEmpty(t, c.tempRoot)
	})

	t.Run("an anonymous ssh fetch still gets the strict policy and no key", func(t *testing.T) {
		if _, err := exec.LookPath("ssh"); err != nil {
			t.Skip("ssh is not on PATH; the ssh path needs it to be invoked")
		}
		logPath := loggingShim(t)
		c := newExecGitClient()
		c.tempRoot = t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := c.ListBranches(ctx, GitRemote{URL: "git@127.0.0.1:org/repo.git"}); err == nil {
			t.Fatal("a fetch from an unreachable host succeeded")
		}
		var cmdline string
		for _, b := range readShimLog(t, logPath) {
			if strings.Contains(b.argv, "ls-remote") {
				cmdline, _ = b.has("GIT_SSH_COMMAND=")
			}
		}
		for _, want := range []string{"StrictHostKeyChecking=yes", "BatchMode=yes"} {
			if !strings.Contains(cmdline, want) {
				t.Fatalf("GIT_SSH_COMMAND %q lacks %s", cmdline, want)
			}
		}
		if strings.Contains(cmdline, " -i ") {
			t.Fatalf("an anonymous fetch named a key: %q", cmdline)
		}
	})
}
