// pack-check validates a capability pack manifest against contract_version 1
// and prints findings a human or a code assistant can act on.
//
//	pack-check [--format json] path/to/manifest.json
//
// Exit 0 when valid, 1 on findings, 2 on usage error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/puppet-stagehand/stagehand-sdk/internal/manifest"
)

func main() {
	format := flag.String("format", "text", "text | json")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: pack-check [--format json] manifest.json")
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
