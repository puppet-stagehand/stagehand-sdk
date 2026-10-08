// Package bundle wraps esbuild's Go API for the two builds a pack UI needs:
// the in-process entry (shims external) and the self-contained sandbox entry.
package bundle

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/puppet-stagehand/stagehand-sdk/manifest"
	"github.com/puppet-stagehand/stagehand-sdk/uibundle"
)

// Files maps an output path relative to the ui directory (slash-separated) to its bytes.
type Files map[string][]byte

func shimPlugin() api.Plugin {
	return api.Plugin{Name: "stagehand-shims", Setup: func(b api.PluginBuild) {
		b.OnResolve(api.OnResolveOptions{Filter: `.*`}, func(a api.OnResolveArgs) (api.OnResolveResult, error) {
			if u, ok := uibundle.Shims[a.Path]; ok {
				return api.OnResolveResult{Path: u, External: true}, nil
			}
			// A subpath of a shim package would be bundled in from node_modules,
			// giving the console a second copy of React. Refuse it.
			for _, pkg := range []string{"react", "react-dom", "@stagehand/console-ui"} {
				if strings.HasPrefix(a.Path, pkg+"/") {
					return api.OnResolveResult{}, fmt.Errorf("import %q: the in-process entry may import only react, react/jsx-runtime, react-dom and @stagehand/console-ui (the console supplies them); use the sandbox entry for anything self-contained", a.Path)
				}
			}
			return api.OnResolveResult{}, nil
		})
	}}
}

// Entry bundles <src>/<entry> as native ESM: chunks allowed, the four shim
// modules external and rewritten to /runtime/v1/*.js. The output is index.js.
func Entry(src, entry string) (Files, []manifest.Finding) {
	return build(src, entry, "index", api.BuildOptions{
		Splitting:  true,
		Plugins:    []api.Plugin{shimPlugin()},
		ChunkNames: "chunks/chunk-[hash]", // never carries an author file name
	})
}

// Sandbox bundles <src>/<entry> as ONE self-contained ESM file named
// sandbox.js: no externals, no splitting.
func Sandbox(src, entry string) (Files, []manifest.Finding) {
	return build(src, entry, "sandbox", api.BuildOptions{})
}

func build(src, entry, name string, o api.BuildOptions) (Files, []manifest.Finding) {
	abs, err := filepath.Abs(src)
	if err == nil {
		// esbuild reports output paths with symlinks resolved (/var -> /private/var
		// on macOS); resolve here so the paths below stay comparable.
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err != nil {
		return nil, []manifest.Finding{buildErr(err.Error())}
	}
	o.AbsWorkingDir = abs // keeps esbuild's "// path" comments machine-independent
	o.EntryPointsAdvanced = []api.EntryPoint{{InputPath: entry, OutputPath: name}}
	o.Outdir = "out"
	o.EntryNames = "[name]"
	o.Bundle = true
	o.Write = false
	o.Format = api.FormatESModule
	o.JSX = api.JSXAutomatic
	o.Target = api.ES2022
	o.LegalComments = api.LegalCommentsNone
	o.LogLevel = api.LogLevelSilent
	r := api.Build(o)
	if len(r.Errors) > 0 {
		var fs []manifest.Finding
		for _, e := range r.Errors {
			msg := e.Text
			if e.Location != nil {
				msg = fmt.Sprintf("%s:%d: %s", e.Location.File, e.Location.Line, e.Text)
			}
			fs = append(fs, buildErr(msg))
		}
		return nil, fs
	}
	outRoot := filepath.Join(abs, "out")
	files := Files{}
	var bad []manifest.Finding
	for _, f := range r.OutputFiles {
		rel, err := filepath.Rel(outRoot, f.Path)
		if err != nil {
			return nil, []manifest.Finding{buildErr(err.Error())}
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, ".js") {
			bad = append(bad, manifest.Finding{
				Code: "ui_build_output_unsupported", Path: "/" + rel,
				Message: "the build produced " + rel + "; slice 1 of expansion-build emits .js files only",
				Fix:     "Remove the CSS or asset import (an ESM entry cannot load CSS by itself); use theme tokens and console-ui primitives instead.",
			})
			continue
		}
		files[rel] = f.Contents
	}
	if len(bad) > 0 {
		sort.Slice(bad, func(i, j int) bool { return bad[i].Path < bad[j].Path })
		return nil, bad
	}
	return files, nil
}

func buildErr(msg string) manifest.Finding {
	return manifest.Finding{
		Code: "ui_build_error", Path: "/", Message: msg,
		Fix: "Fix the import or syntax error above; if a package is missing, install it in the pack's node_modules.",
	}
}
