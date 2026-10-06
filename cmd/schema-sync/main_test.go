package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckDstRefusesUnsafeDestinations(t *testing.T) {
	cases := []struct {
		name, src, dst string
		refused        bool
	}{
		{"canonical invocation", "../proto", "proto", false},
		{"nested proto element", "../proto", "out/proto", false},
		{"absolute dst", "proto", "/tmp/proto", true},
		{"backslash-rooted dst", "proto", `\proto`, true},
		{"parent traversal", "proto", "../proto", true},
		{"traversal mid-path", "proto", "a/../../proto", true},
		{"last element not proto", "proto", "schema/x", true},
		{"dst equals src", "proto", "proto", true},
		{"dst equals src after clean", "./proto", "proto/", true},
	}
	for _, c := range cases {
		err := checkDst(c.src, c.dst)
		if c.refused && err == nil {
			t.Errorf("%s: checkDst(%q, %q) accepted an unsafe destination", c.name, c.src, c.dst)
		}
		if !c.refused && err != nil {
			t.Errorf("%s: checkDst(%q, %q) refused a safe destination: %v", c.name, c.src, c.dst, err)
		}
	}
}

func TestSyncTreeCopiesVerbatimAndPrunesStaleFiles(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "proto")
	write := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := "line one\r\nline two\n\x00tail"
	write(filepath.Join(src, "a", "keep.proto"), want)
	write(filepath.Join(dst, "a", "keep.proto"), "old")
	write(filepath.Join(dst, "stale", "deep", "gone.proto"), "stale")

	copied, removed, err := syncTree(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if copied != 1 || removed != 1 {
		t.Errorf("copied=%d removed=%d, want 1 and 1", copied, removed)
	}
	got, err := os.ReadFile(filepath.Join(dst, "a", "keep.proto"))
	if err != nil || string(got) != want {
		t.Errorf("copied bytes differ: got %q err %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale")); !os.IsNotExist(err) {
		t.Errorf("stale directory was not pruned: %v", err)
	}
}

func TestSyncTreeSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "proto")
	outside := filepath.Join(root, "outside.txt")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "real.proto"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "link.proto")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	copied, _, err := syncTree(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if copied != 1 {
		t.Errorf("copied=%d, want 1 (symlink must be skipped)", copied)
	}
	if _, err := os.Stat(filepath.Join(dst, "link.proto")); !os.IsNotExist(err) {
		t.Errorf("symlink was copied: %v", err)
	}
}
