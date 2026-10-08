package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunUsage(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no subcommand", nil, 2, "usage: expansion-build"},
		{"unknown subcommand", []string{"image"}, 2, `unknown subcommand "image"`},
		{"help", []string{"-h"}, 0, "usage: expansion-build"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if got := run(c.args, &out, &errb); got != c.code {
				t.Fatalf("exit %d, want %d (stderr %q)", got, c.code, errb.String())
			}
			if !strings.Contains(out.String()+errb.String(), c.want) {
				t.Fatalf("output %q lacks %q", out.String()+errb.String(), c.want)
			}
		})
	}
}

func TestUIExitCodes(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"ui", "--bogus"}, &out, &errb); code != 2 {
		t.Fatalf("bad flag: exit %d", code)
	}
	out.Reset()
	errb.Reset()
	if code := run([]string{"ui", "--src", t.TempDir(), "--out", filepath.Join(t.TempDir(), "o"), "--manifest", filepath.Join(t.TempDir(), "nope.json")}, &out, &errb); code != 2 {
		t.Fatalf("unreadable manifest: exit %d, stderr %q", code, errb.String())
	}
}

func TestUIFindingsExitOneAndJSON(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"id":"x"`), 0o644)
	var out, errb bytes.Buffer
	code := run([]string{"ui", "--format", "json", "--src", dir, "--out", filepath.Join(dir, "o"), "--manifest", filepath.Join(dir, "manifest.json")}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	var got struct {
		OK       bool              `json:"ok"`
		Findings []json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.OK || len(got.Findings) == 0 {
		t.Fatalf("%v %s", err, out.String())
	}
}

func TestUIBuildsTheFixture(t *testing.T) {
	root := t.TempDir()
	for _, c := range [][2]string{{"hello-src", "ui/src"}, {"node_modules", "ui/node_modules"}} {
		from, to := filepath.Join("testdata", c[0]), filepath.Join(root, c[1])
		_ = filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
			rel, _ := filepath.Rel(from, p)
			if d.IsDir() {
				return os.MkdirAll(filepath.Join(to, rel), 0o755)
			}
			b, _ := os.ReadFile(p)
			return os.WriteFile(filepath.Join(to, rel), b, 0o644)
		})
	}
	_ = os.Rename(filepath.Join(root, "ui/src/manifest.json"), filepath.Join(root, "manifest.json"))
	var out, errb bytes.Buffer
	code := run([]string{"ui", "--src", filepath.Join(root, "ui/src"), "--out", filepath.Join(root, "dist/ui"), "--manifest", filepath.Join(root, "manifest.json")}, &out, &errb)
	if code != 0 || !strings.Contains(out.String(), "ok: built 2 file(s)") || !strings.Contains(out.String(), "ui_digest sha256:") {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out.String(), errb.String())
	}
}
