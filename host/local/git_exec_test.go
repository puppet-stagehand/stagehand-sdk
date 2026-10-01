package local

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
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
