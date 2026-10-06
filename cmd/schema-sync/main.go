// schema-sync makes the destination tree a byte-identical copy of the source
// tree. It exists so schema/proto/ (the contract agents read) can never be
// hand-edited out of step with proto/ (the contract buf builds).
//
//	schema-sync -src ../proto -dst proto
//
// Invoked by `go generate ./schema`. Exit 0 on success, 2 on usage error or a
// refused -dst. It deletes files under -dst, so -dst is validated before any
// filesystem access: it must be relative, free of "..", end in an element
// named "proto", and differ from -src.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	src := flag.String("src", "", "source tree (required)")
	dst := flag.String("dst", "", "destination tree, relative, last element must be proto (required)")
	flag.Parse()
	if flag.NArg() != 0 || *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "usage: schema-sync -src DIR -dst DIR")
		os.Exit(2)
	}
	if err := checkDst(*src, *dst); err != nil {
		fmt.Fprintln(os.Stderr, "schema-sync:", err)
		os.Exit(2)
	}
	copied, removed, err := syncTree(*src, *dst)
	if err != nil {
		fmt.Fprintln(os.Stderr, "schema-sync:", err)
		os.Exit(2)
	}
	fmt.Printf("schema-sync: %d files copied, %d removed\n", copied, removed)
}

// checkDst refuses any -dst the copier could use to delete or write outside
// the agent-facing proto tree. It touches no files.
func checkDst(src, dst string) error {
	if filepath.IsAbs(dst) || strings.HasPrefix(dst, "/") || strings.HasPrefix(dst, `\`) || filepath.VolumeName(dst) != "" {
		return fmt.Errorf("refusing -dst %q: must be a relative path", dst)
	}
	clean := filepath.Clean(dst)
	for _, el := range strings.FieldsFunc(clean, func(r rune) bool { return r == '/' || r == '\\' }) {
		if el == ".." {
			return fmt.Errorf("refusing -dst %q: must not contain ..", dst)
		}
	}
	if filepath.Base(clean) != "proto" {
		return fmt.Errorf("refusing -dst %q: last path element must be proto", dst)
	}
	if clean == filepath.Clean(src) {
		return fmt.Errorf("refusing -dst %q: equal to -src", dst)
	}
	return nil
}

// syncTree copies every regular file under src to dst with the exact source
// bytes, then removes files under dst that have no counterpart in src and
// prunes directories left empty. Symlinks are skipped, never followed.
func syncTree(src, dst string) (copied, removed int, err error) {
	if st, e := os.Stat(src); e != nil || !st.IsDir() {
		return 0, 0, fmt.Errorf("-src %q is not a readable directory", src)
	}
	want := map[string]bool{}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, e := filepath.Rel(src, p)
		if e != nil {
			return e
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		out := filepath.Join(dst, rel)
		if e := os.MkdirAll(filepath.Dir(out), 0o755); e != nil {
			return e
		}
		if e := os.WriteFile(out, b, 0o644); e != nil {
			return e
		}
		want[rel] = true
		copied++
		return nil
	})
	if err != nil {
		return copied, 0, err
	}
	if _, e := os.Stat(dst); e != nil {
		return copied, 0, nil
	}
	var dirs []string
	err = filepath.WalkDir(dst, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			if p != dst {
				dirs = append(dirs, p)
			}
			return nil
		}
		rel, e := filepath.Rel(dst, p)
		if e != nil {
			return e
		}
		if !want[rel] {
			if e := os.Remove(p); e != nil {
				return e
			}
			removed++
		}
		return nil
	})
	if err != nil {
		return copied, removed, err
	}
	// Deepest first, so a directory emptied by pruning its children goes too.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		if ents, e := os.ReadDir(d); e == nil && len(ents) == 0 {
			if e := os.Remove(d); e != nil {
				return copied, removed, e
			}
		}
	}
	return copied, removed, nil
}
