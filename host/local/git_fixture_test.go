package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// ------------------------------------------------- in-memory GitClient

// gitFixture is a deterministic GitClient/GitRepo double over a map of branch
// name to file map, so every import test other than the real-client ones runs
// without a network or a git binary. It records which branches were opened,
// so a later plan can assert that a non-importable branch was never fetched.
//
// modes optionally overrides a file's git mode ("branch\x00path" -> mode) so a
// test can model a symlink (120000) or a submodule pointer (160000); the
// default is 100644.
type gitFixture struct {
	mu       sync.Mutex
	branches map[string]map[string]string
	modes    map[string]string
	listErr  error
	openErr  error

	listed int
	opened []string
}

func newGitFixture(branches map[string]map[string]string) *gitFixture {
	return &gitFixture{branches: branches, modes: map[string]string{}}
}

func gitFixtureModeKey(branch, path string) string { return branch + "\x00" + path }

func (f *gitFixture) fixtureCommit(branch string) string {
	return fmt.Sprintf("%040x", len(branch)*7919+int(branch[0]))
}

func (f *gitFixture) ListBranches(_ context.Context, _ GitRemote) ([]GitBranchRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed++
	if f.listErr != nil {
		return nil, f.listErr
	}
	names := make([]string, 0, len(f.branches))
	for n := range f.branches {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]GitBranchRef, 0, len(names))
	for _, n := range names {
		out = append(out, GitBranchRef{Name: n, Commit: f.fixtureCommit(n)})
	}
	return out, nil
}

func (f *gitFixture) Open(_ context.Context, _ GitRemote, branches []string) (GitRepo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openErr != nil {
		return nil, f.openErr
	}
	for _, b := range branches {
		if _, ok := f.branches[b]; !ok {
			return nil, errors.New("gitFixture: no such branch")
		}
	}
	f.opened = append(f.opened, branches...)
	return &gitFixtureRepo{f: f, branches: append([]string(nil), branches...)}, nil
}

// openedBranches returns every branch name Open was ever asked for.
func (f *gitFixture) openedBranches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.opened...)
}

type gitFixtureRepo struct {
	f        *gitFixture
	branches []string
	closed   bool
}

func (r *gitFixtureRepo) has(branch string) bool {
	for _, b := range r.branches {
		if b == branch {
			return true
		}
	}
	return false
}

func (r *gitFixtureRepo) Commit(branch string) (string, bool) {
	if !r.has(branch) {
		return "", false
	}
	return r.f.fixtureCommit(branch), true
}

func (r *gitFixtureRepo) ListFiles(branch string, pathspecs ...string) ([]GitFileEntry, error) {
	if !r.has(branch) {
		return nil, errors.New("gitFixture: branch not opened")
	}
	files := r.f.branches[branch]
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []GitFileEntry
	for _, p := range paths {
		if len(pathspecs) > 0 {
			match := false
			for _, ps := range pathspecs {
				if p == ps || strings.HasPrefix(p, strings.TrimSuffix(ps, "/")+"/") {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		mode := "100644"
		if m, ok := r.f.modes[gitFixtureModeKey(branch, p)]; ok {
			mode = m
		}
		out = append(out, GitFileEntry{Path: p, Mode: mode, Size: int64(len(files[p]))})
	}
	return out, nil
}

func (r *gitFixtureRepo) ReadFile(branch, path string) ([]byte, error) {
	if !r.has(branch) {
		return nil, errors.New("gitFixture: branch not opened")
	}
	body, ok := r.f.branches[branch][path]
	if !ok {
		return nil, errors.New("gitFixture: no such file")
	}
	return []byte(body), nil
}

func (r *gitFixtureRepo) Close() error { r.closed = true; return nil }

// ------------------------------------------------- real temp bare repo

// requireGit skips the calling test when no git binary is on PATH. A skip is
// the documented condition for the real-client tests, not a pass: it means the
// real client was never exercised.
func requireGit(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH; the real git client cannot be exercised")
	}
	return p
}

// tempRepo describes a real bare repository built by newTempBareRepo.
type tempRepo struct {
	// URL is the file:// URL of the bare repository.
	URL string
	// Path is its filesystem path.
	Path string
	// Commits maps branch name to the commit SHA the branch points at.
	Commits map[string]string
}

// tempWork is the scratch working repository a customiser mutates before the
// bare copy is made.
type tempWork struct {
	t   *testing.T
	dir string
}

// hermeticGitEnv is the environment every fixture git invocation runs under:
// no operator or system config, a fixed identity, and a throwaway HOME.
func hermeticGitEnv(home string) []string {
	return append(os.Environ(),
		"HOME="+home,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
}

func (w *tempWork) git(args ...string) string {
	w.t.Helper()
	full := append([]string{
		"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
		"-c", "init.defaultBranch=production", "-c", "commit.gpgsign=false",
		"-c", "protocol.file.allow=always",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = w.dir
	cmd.Env = hermeticGitEnv(w.dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (w *tempWork) write(path, content string) {
	w.t.Helper()
	full := filepath.Join(w.dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *tempWork) symlink(path, target string) {
	w.t.Helper()
	full := filepath.Join(w.dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		w.t.Fatal(err)
	}
}

func (w *tempWork) commitAll(msg string) {
	w.t.Helper()
	w.git("add", "-A")
	w.git("commit", "-q", "-m", msg)
}

// Fixture file bodies the end-to-end tests assert byte-for-byte.
const (
	tempRepoProdPuppetfile = "forge 'https://forgeapi.puppet.com'\n\nmod 'puppetlabs-stdlib', '9.0.0'  # pinned # twice\n"
	tempRepoProdCommonYAML = "---\nmotd: \"héllo wörld — ünïcode # not a comment\"\n"
	tempRepoDevPuppetfile  = "mod 'puppetlabs-apache', '12.0.0'\n"
)

// newTempBareRepo builds a real bare repository with two branches,
// "production" and "dev", a tag, and a refs/remotes ref (so a test can prove
// ListBranches reports branch heads only), and returns its file:// URL. Each
// customiser runs against the scratch working repository on the production
// branch before the bare copy is made; it is how the hardening tests add a
// symlink or a submodule pointer.
func newTempBareRepo(t *testing.T, customise ...func(*tempWork)) tempRepo {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	w := &tempWork{t: t, dir: filepath.Join(root, "work")}
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	w.git("init", "-q")
	w.write("Puppetfile", tempRepoProdPuppetfile)
	w.write("data/common.yaml", tempRepoProdCommonYAML)
	w.write("hiera.yaml", "---\nversion: 5\n")
	for _, c := range customise {
		c(w)
	}
	w.commitAll("production")
	w.git("tag", "v1")
	w.git("checkout", "-q", "-b", "dev")
	w.write("Puppetfile", tempRepoDevPuppetfile)
	w.commitAll("dev")
	w.git("checkout", "-q", "production")

	bare := filepath.Join(root, "remote.git")
	w.git("clone", "-q", "--bare", w.dir, bare)
	b := &tempWork{t: t, dir: bare}
	prod := b.git("rev-parse", "refs/heads/production")
	dev := b.git("rev-parse", "refs/heads/dev")
	b.git("update-ref", "refs/remotes/origin/extra", prod)
	return tempRepo{
		URL:     "file://" + bare,
		Path:    bare,
		Commits: map[string]string{"production": prod, "dev": dev},
	}
}
