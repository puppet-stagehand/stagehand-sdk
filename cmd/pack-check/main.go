// pack-check validates a capability pack manifest against contract_version 1
// and prints findings a human or a code assistant can act on.
//
//	pack-check [--format json] [--ui path/to/ui] path/to/manifest.json
//
// With --ui, the built UI directory (the pack image's /stagehand/ui/) is also
// checked: ui.manifest.json is valid, manifest.ui_digest is its digest, every
// listed file matches its size and sha256, and nothing unlisted is present.
//
// Exit 0 when valid, 1 on findings, 2 on usage error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/puppet-stagehand/stagehand-sdk/manifest"
	"github.com/puppet-stagehand/stagehand-sdk/uibundle"
)

func main() {
	format := flag.String("format", "text", "text | json")
	uiDir := flag.String("ui", "", "also verify the built UI directory (ui.manifest.json and its files)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: pack-check [--format json] [--ui dir] manifest.json")
		os.Exit(2)
	}
	raw, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	m, fs := manifest.Parse(raw)
	if m != nil {
		fs = append(fs, manifest.Validate(m)...)
	}
	if *uiDir != "" {
		fs = append(fs, uibundle.Verify(*uiDir, m)...)
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"ok": len(fs) == 0, "contract_version": manifest.ContractVersion, "findings": fs})
	} else if len(fs) == 0 {
		fmt.Printf("ok: %s %s (contract_version %d)\n", m.ID, m.Version, manifest.ContractVersion)
	} else {
		for _, f := range fs {
			fmt.Println(f)
		}
		fmt.Printf("%d finding(s)\n", len(fs))
	}
	if len(fs) > 0 {
		os.Exit(1)
	}
}
