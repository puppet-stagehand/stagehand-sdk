package schema

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// canonicalTree is the tree edited by hand and fed to buf. agentTree is the
// committed verbatim copy that CLAUDE.md, AGENTS.md, README.md and llms.txt
// point coding agents at. Both paths are relative to this package directory.
const (
	canonicalTree = "../proto"
	agentTree     = "proto"
)

// readTree returns every regular file under root keyed by its slash-separated
// path relative to root. Symlinks and other non-regular entries are ignored,
// which matches what cmd/schema-sync copies.
func readTree(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	return files, err
}

// compareTrees reports every way the regular files under a and b differ:
// a path present in only one tree, or the same relative path with different
// bytes. The result is sorted and empty when the trees are identical.
func compareTrees(a, b string) ([]string, error) {
	fa, err := readTree(a)
	if err != nil {
		return nil, err
	}
	fb, err := readTree(b)
	if err != nil {
		return nil, err
	}
	var diffs []string
	for p, ba := range fa {
		bb, ok := fb[p]
		switch {
		case !ok:
			diffs = append(diffs, fmt.Sprintf("only in %s: %s", a, p))
		case !bytes.Equal(ba, bb):
			diffs = append(diffs, fmt.Sprintf("bytes differ: %s", p))
		}
	}
	for p := range fb {
		if _, ok := fa[p]; !ok {
			diffs = append(diffs, fmt.Sprintf("only in %s: %s", b, p))
		}
	}
	sort.Strings(diffs)
	return diffs, nil
}

// TestSchemaProtoMatchesProto pins FND-01: schema/proto/ is a byte-identical
// copy of proto/, so an agent reading it sees the live contract.
func TestSchemaProtoMatchesProto(t *testing.T) {
	const fix = "fix: run go generate ./schema (copies proto/ to schema/proto/)"
	for _, tree := range []string{canonicalTree, agentTree} {
		files, err := readTree(tree)
		if err != nil {
			t.Fatalf("reading %s: %v\n%s", tree, err, fix)
		}
		if len(files) == 0 {
			t.Fatalf("%s holds no files: an empty or missing tree is never equal to its counterpart\n%s", tree, fix)
		}
	}
	diffs, err := compareTrees(canonicalTree, agentTree)
	if err != nil {
		t.Fatalf("comparing trees: %v\n%s", err, fix)
	}
	if len(diffs) > 0 {
		t.Fatalf("schema/proto has drifted from proto (%d difference(s)):\n  %s\n%s",
			len(diffs), strings.Join(diffs, "\n  "), fix)
	}
}

// TestCompareTreesDetectsOneSidedEdit keeps the red path of the drift test
// tested: identical trees compare clean, a one-byte edit on one side and a
// file present on one side only are each reported by name.
func TestCompareTreesDetectsOneSidedEdit(t *testing.T) {
	write := func(root, rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a, b := t.TempDir(), t.TempDir()
	for _, root := range []string{a, b} {
		write(root, "stagehand/host/v1/host.proto", "syntax = \"proto3\";\n")
		write(root, "stagehand/host/v1/common.proto", "// common\n")
	}

	diffs, err := compareTrees(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("identical trees reported differences: %v", diffs)
	}

	write(b, "stagehand/host/v1/host.proto", "syntax = \"proto3\";\n\n")
	diffs, err = compareTrees(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0] != "bytes differ: stagehand/host/v1/host.proto" {
		t.Fatalf("one-byte edit: want exactly one 'bytes differ' naming host.proto, got %v", diffs)
	}

	write(b, "stagehand/host/v1/host.proto", "syntax = \"proto3\";\n")
	write(b, "stagehand/host/v1/extra.proto", "// extra\n")
	diffs, err = compareTrees(a, b)
	if err != nil {
		t.Fatal(err)
	}
	want := "only in " + b + ": stagehand/host/v1/extra.proto"
	if len(diffs) != 1 || diffs[0] != want {
		t.Fatalf("file only in second tree: want [%s], got %v", want, diffs)
	}
}
