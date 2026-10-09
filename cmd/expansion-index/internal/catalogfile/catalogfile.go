// Package catalogfile reads catalog.yaml, the reviewed source of truth for a
// feed: which packs exist, which candidate image is each released version, and
// where the feed's index and pack images are published.
//
// catalog.yaml never carries manifest-owned fields (name, publisher, tier,
// licence, entitlement, summary, permissions, contract_version): those are read
// from each candidate image's stagehand/manifest.json by `expansion-index
// build`, so a catalog cannot disagree with the image it lists.
package catalogfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

// Finding is one problem with catalog.yaml (or, via the index package, with a
// build). Code is stable; Fix tells a human or a code assistant what to change.
type Finding struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
}

func (f Finding) String() string {
	return fmt.Sprintf("%s at %s: %s — fix: %s", f.Code, f.Path, f.Message, f.Fix)
}

// File is a decoded catalog.yaml.
type File struct {
	Format int             `yaml:"format"`
	Feeds  map[string]Feed `yaml:"feeds"`
	Packs  []Pack          `yaml:"packs"`
}

// Feed is one place packs are published. Index and Images are registry
// repositories without a scheme or tag: Index is where the signed index
// artifact lives, Images is the prefix under which each pack image is copied
// (<images>/<pack id>@<digest>). Visibility is public or private.
type Feed struct {
	Index      string `yaml:"index"`
	Images     string `yaml:"images"`
	Visibility string `yaml:"visibility"`
}

// Pack is one pack in the catalog.
type Pack struct {
	ID       string    `yaml:"id"`
	Feed     string    `yaml:"feed"`
	DocsURL  string    `yaml:"docs_url"`
	Listing  *Listing  `yaml:"listing"`
	Versions []Version `yaml:"versions"`
}

// Listing is the public marketing stub for a pack on a private feed.
type Listing struct {
	Name    string `yaml:"name"`
	Summary string `yaml:"summary"`
	Tier    string `yaml:"tier"`
}

// Version is one released version of a pack. Candidate is the unsigned staging
// image, digest-pinned; ReleasedAt is a YYYY-MM-DD date.
type Version struct {
	Version    string `yaml:"version"`
	Candidate  string `yaml:"candidate"`
	ReleasedAt string `yaml:"released_at"`
	NotesURL   string `yaml:"notes_url"`
}

// Load reads and decodes the catalog at path.
func Load(path string) (*File, []Finding, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	f, findings := Decode(raw)
	return f, findings, nil
}

// Decode decodes raw catalog.yaml bytes. An unknown key anywhere is a finding.
func Decode(raw []byte) (*File, []Finding) {
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, []Finding{{Code: "catalog_empty", Path: "/", Message: "catalog.yaml is empty",
				Fix: "Start from `format: 1` and add feeds and packs."}}
		}
		return nil, []Finding{{Code: "catalog_unparseable", Path: "/", Message: err.Error(),
			Fix: "Make catalog.yaml valid YAML that uses only the keys documented for format 1."}}
	}
	return &f, nil
}
