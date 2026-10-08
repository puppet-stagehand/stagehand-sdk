// Package build runs the expansion-build ui pipeline.
package build

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-build/internal/bundle"
	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-build/internal/lint"
	"github.com/puppet-stagehand/stagehand-sdk/manifest"
	"github.com/puppet-stagehand/stagehand-sdk/uibundle"
)

// Options are the three paths a build needs.
type Options struct{ Src, Out, Manifest string }

// Result describes a successful build.
type Result struct {
	Digest string   // sha256:...
	Files  []string // slash paths relative to Out, sorted, including ui.manifest.json
}

var entryNames = []string{"index.tsx", "index.ts", "index.jsx", "index.js"}
var sandboxNames = []string{"sandbox.tsx", "sandbox.ts", "sandbox.jsx", "sandbox.js"}

func first(dir string, names []string) string {
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return n
		}
	}
	return ""
}

// contentType is trivial in slice 1: bundle refuses every non-.js output.
func contentType(string) string { return "text/javascript" }

// Run builds the UI. Findings are author-fixable; the error is environment trouble.
func Run(o Options) (*Result, []manifest.Finding, error) {
	// A trailing separator would make filepath.Dir(o.Out) be o.Out itself.
	o.Out = filepath.Clean(o.Out)
	// 1. manifest
	mraw, err := os.ReadFile(o.Manifest)
	if err != nil {
		return nil, nil, err
	}
	pm, fs := manifest.Parse(mraw)
	if pm != nil {
		fs = append(fs, manifest.Validate(pm)...)
	}
	if len(fs) > 0 {
		return nil, fs, nil
	}
	if !reDigestField.Match(mraw) {
		return nil, []manifest.Finding{{Code: "ui_digest_key_missing", Path: "/ui_digest",
			Message: "manifest.json has no ui_digest key",
			Fix:     `Add "ui_digest": "sha256:` + strings.Repeat("0", 64) + `" to manifest.json; expansion-build replaces the value.`}}, nil
	}
	if fs := checkOut(o); len(fs) > 0 {
		return nil, fs, nil
	}
	// 2. entry file and slots
	entry := first(o.Src, entryNames)
	if entry == "" {
		return nil, []manifest.Finding{{Code: "ui_src_entry_missing", Path: o.Src,
			Message: "no entry module found; expected one of " + strings.Join(entryNames, ", "),
			Fix:     "Create index.tsx in the source directory; it default-exports { contract_version: 1, slots }."}}, nil
	}
	slots, fs, err := readSlots(o.Src, pm)
	if err != nil {
		return nil, nil, err
	}
	if len(fs) > 0 {
		return nil, fs, nil
	}
	// 3. lints
	var srcs []lint.Source
	err = filepath.WalkDir(o.Src, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".ts", ".tsx", ".js", ".jsx", ".css":
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			rel, _ := filepath.Rel(o.Src, p)
			srcs = append(srcs, lint.Source{Path: filepath.ToSlash(rel), Text: string(b)})
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if fs := lint.Run(srcs); len(fs) > 0 {
		return nil, fs, nil
	}
	// 4-5. bundles
	files, fs := bundle.Entry(o.Src, entry)
	if len(fs) > 0 {
		return nil, fs, nil
	}
	um := uibundle.Manifest{Format: uibundle.FormatVersion, Entry: "index.js", Slots: slots, Files: map[string]uibundle.File{}}
	if sb := first(o.Src, sandboxNames); sb != "" {
		sfiles, sfs := bundle.Sandbox(o.Src, sb)
		if len(sfs) > 0 {
			return nil, sfs, nil
		}
		for n, b := range sfiles {
			files[n] = b
		}
		um.SandboxEntry = "sandbox.js"
	}
	// 6. write into a temp dir next to --out
	if err := os.MkdirAll(filepath.Dir(o.Out), 0o755); err != nil {
		return nil, nil, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(o.Out), ".expansion-build-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)
	names := make([]string, 0, len(files)+1)
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sum := sha256.Sum256(files[n])
		um.Files[n] = uibundle.File{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(files[n])), ContentType: contentType(n)}
		dst := filepath.Join(tmp, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(dst, files[n], 0o644); err != nil {
			return nil, nil, err
		}
	}
	idx, err := json.MarshalIndent(um, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	idx = append(idx, '\n')
	if err := os.WriteFile(filepath.Join(tmp, uibundle.ManifestPath), idx, 0o644); err != nil {
		return nil, nil, err
	}
	// 7-8. digest, then self-check against a manifest carrying it
	digest := uibundle.DigestOf(idx)
	pm.UIDigest = digest
	if vfs := uibundle.Verify(tmp, pm); len(vfs) > 0 {
		for i := range vfs {
			vfs[i].Message = "builder bug: " + vfs[i].Message
		}
		return nil, vfs, nil
	}
	if err := replaceDir(tmp, o.Out); err != nil {
		return nil, nil, err
	}
	if err := writeDigest(o.Manifest, digest); err != nil {
		return nil, nil, err
	}
	all := append(names, uibundle.ManifestPath)
	sort.Strings(all)
	return &Result{Digest: digest, Files: all}, nil, nil
}
