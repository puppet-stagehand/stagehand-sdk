// expansion-build builds an Expansion Pack. Slice 1 has one subcommand:
//
//	expansion-build ui [--src ui/src] [--out dist/ui] [--manifest manifest.json] [--format json]
//
// Exit 0 ok, 1 findings, 2 usage or environment error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-build/internal/build"
	"github.com/puppet-stagehand/stagehand-sdk/uibundle"
)

const usage = "usage: expansion-build ui [--src ui/src] [--out dist/ui] [--manifest manifest.json] [--format json]\n"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "ui":
		return runUI(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown subcommand %q\n%s", args[0], usage)
	return 2
}

func runUI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	src := fs.String("src", "ui/src", "UI source directory")
	out := fs.String("out", "dist/ui", "output directory (the pack image's /stagehand/ui/)")
	mf := fs.String("manifest", "manifest.json", "pack manifest; only ui_digest is rewritten")
	format := fs.String("format", "text", "text | json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	res, findings, err := build.Run(build.Options{Src: *src, Out: *out, Manifest: *mf})
	if err != nil {
		fmt.Fprintln(stderr, "expansion-build:", err)
		return 2
	}
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		doc := map[string]any{"ok": len(findings) == 0, "contract_version": uibundle.ContractVersion, "findings": findings}
		if res != nil {
			doc["ui_digest"], doc["files"] = res.Digest, res.Files
		}
		_ = enc.Encode(doc)
	} else if len(findings) == 0 && res != nil {
		fmt.Fprintf(stdout, "ok: built %d file(s) into %s\nui_digest %s\n", len(res.Files), *out, res.Digest)
	} else {
		for _, f := range findings {
			fmt.Fprintln(stdout, f)
		}
		fmt.Fprintf(stdout, "%d finding(s)\n", len(findings))
	}
	if len(findings) > 0 {
		return 1
	}
	return 0
}
