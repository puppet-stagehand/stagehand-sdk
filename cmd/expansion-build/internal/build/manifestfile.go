package build

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var reDigestField = regexp.MustCompile(`("ui_digest"\s*:\s*")[^"]*(")`)

// writeDigest replaces only the value of "ui_digest", leaving every other byte
// of manifest.json as it was, and writes the file atomically.
func writeDigest(path, digest string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !reDigestField.Match(raw) {
		return fmt.Errorf("manifest has no ui_digest key")
	}
	out := reDigestField.ReplaceAll(raw, []byte("${1}"+digest+"${2}"))
	if string(out) == string(raw) {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".manifest-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil {
		_ = os.Chmod(tmp.Name(), st.Mode())
	}
	return os.Rename(tmp.Name(), path)
}
