package build

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/puppet-stagehand/stagehand-sdk/manifest"
	"github.com/puppet-stagehand/stagehand-sdk/uibundle"
)

func outFinding(msg string) []manifest.Finding {
	return []manifest.Finding{{Code: "ui_out_unsafe", Path: "--out", Message: msg,
		Fix: "Choose an --out directory that is empty, does not exist yet, or holds a previous expansion-build output (it has a ui.manifest.json), and does not contain --src or --manifest."}}
}

// checkOut refuses an --out that would delete something that is not a previous build.
func checkOut(o Options) []manifest.Finding {
	out, err := filepath.Abs(o.Out)
	if err != nil {
		return outFinding(err.Error())
	}
	for _, p := range []string{o.Src, o.Manifest} {
		abs, err := filepath.Abs(p)
		if err != nil {
			return outFinding(err.Error())
		}
		if abs == out || strings.HasPrefix(abs+string(filepath.Separator), out+string(filepath.Separator)) {
			return outFinding("--out " + o.Out + " contains " + p)
		}
	}
	if cwd, err := os.Getwd(); err == nil && cwd == out {
		return outFinding("--out is the current directory")
	}
	ents, err := os.ReadDir(out)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return outFinding(err.Error())
	}
	if len(ents) == 0 {
		return nil
	}
	if _, err := os.Stat(filepath.Join(out, uibundle.ManifestPath)); err != nil {
		return outFinding("--out " + o.Out + " exists, is not empty and is not a previous build")
	}
	return nil
}

// replaceDir swaps tmp into out, removing whatever was there.
func replaceDir(tmp, out string) error {
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, out)
}
