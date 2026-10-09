// Package index builds, publishes, verifies and promotes a Stagehand v1 feed
// index (the document a console reads to list the packs of a feed).
//
// The types and rules in this file mirror the console's strict index decoder
// (stagehand-console backend/internal/expansions/catalog.go: Index, IndexPack,
// IndexVersion, IndexPublisherKey, indexPackIDPattern, indexImagePattern and
// ParseIndex). They must stay byte-for-byte compatible with it: an index this
// tool produces and a console refuses is invisible until a real console tries
// to read the feed. A change here must ship with a console-side fixture test
// (Phase 57.1 adds the producer/consumer drift test, ROADMAP 57.1).
package index

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/catalogfile"
)

// IndexArtifactMediaType is the layer media type of an OCI index artifact.
const IndexArtifactMediaType = "application/vnd.stagehand.expansion-index.v1+json"

// IndexConfigMediaType is the config media type of an OCI index artifact.
const IndexConfigMediaType = "application/vnd.stagehand.expansion-index.config.v1+json"

// MaxIndexBytes caps an index document (the console refuses anything larger).
const MaxIndexBytes int64 = 4 << 20

// Finding is one problem this tool refuses to proceed past. Every finding
// carries a fix line, like pack-check's.
type Finding = catalogfile.Finding

// Index is the published v1 index document.
type Index struct {
	ForgeVersion int    `json:"forge_version"`
	Name         string `json:"name"`
	GeneratedAt  string `json:"generated_at,omitempty"`
	// PublisherKeys is informational only; a console never trusts it.
	PublisherKeys []IndexPublisherKey `json:"publisher_keys"`
	Packs         []IndexPack         `json:"packs"`
}

// IndexPublisherKey is an index-listed publisher key. Informational only.
type IndexPublisherKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

// IndexPack is one pack in an index.
type IndexPack struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Publisher   string         `json:"publisher"`
	Tier        string         `json:"tier"`
	Licence     string         `json:"licence"`
	Entitlement string         `json:"entitlement"`
	Summary     string         `json:"summary,omitempty"`
	DocsURL     string         `json:"docs_url,omitempty"`
	Versions    []IndexVersion `json:"versions"`
}

// IndexVersion is one released version of a pack.
type IndexVersion struct {
	Version         string        `json:"version"`
	ContractVersion int           `json:"contract_version"`
	Image           string        `json:"image"`
	Content         *IndexContent `json:"content,omitempty"`
	Permissions     []string      `json:"permissions"`
	ReleasedAt      string        `json:"released_at"`
	NotesURL        string        `json:"notes_url,omitempty"`
}

// IndexContent names the forge content a version ships with.
type IndexContent struct {
	ForgeSlug string `json:"forge_slug,omitempty"`
	Version   string `json:"version,omitempty"`
}

var (
	// PackIDPattern is the console's indexPackIDPattern.
	PackIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	// ImagePattern is the console's indexImagePattern (the SDK schema's
	// digest-pinned image regex): lowercase host, path and a sha256 digest.
	ImagePattern = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+@sha256:[a-f0-9]{64}$`)
)

// DecodeStrict decodes raw under the console's ParseIndex rules: unknown
// fields and trailing data are refused, then every field rule below. It
// performs no signature check.
func DecodeStrict(raw []byte) (*Index, error) {
	if int64(len(raw)) > MaxIndexBytes {
		return nil, fmt.Errorf("index is larger than %d bytes", MaxIndexBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var idx Index
	if err := dec.Decode(&idx); err != nil {
		return nil, fmt.Errorf("index does not decode: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the index document")
	}
	if idx.ForgeVersion != 1 {
		return nil, fmt.Errorf("forge_version %d is not supported (want 1)", idx.ForgeVersion)
	}
	if idx.Name == "" {
		return nil, errors.New("name is required")
	}
	if idx.GeneratedAt != "" {
		if _, err := time.Parse(time.RFC3339, idx.GeneratedAt); err != nil {
			return nil, errors.New("generated_at is not an RFC 3339 date-time")
		}
	}
	if idx.PublisherKeys == nil {
		return nil, errors.New("publisher_keys is required")
	}
	if idx.Packs == nil {
		return nil, errors.New("packs is required")
	}
	seenPacks := map[string]bool{}
	for _, p := range idx.Packs {
		if !PackIDPattern.MatchString(p.ID) {
			return nil, fmt.Errorf("pack id %q is not a valid pack id", p.ID)
		}
		if seenPacks[p.ID] {
			return nil, fmt.Errorf("duplicate pack id %q", p.ID)
		}
		seenPacks[p.ID] = true
		if p.Name == "" || p.Publisher == "" {
			return nil, fmt.Errorf("pack %q needs a name and a publisher", p.ID)
		}
		switch p.Tier {
		case "core", "ent", "adv":
		default:
			return nil, fmt.Errorf("pack %q has unknown tier %q", p.ID, p.Tier)
		}
		switch p.Entitlement {
		case "none", "licence_key", "marketplace":
		default:
			return nil, fmt.Errorf("pack %q has unknown entitlement %q", p.ID, p.Entitlement)
		}
		if err := catalogfile.CheckWebURL(p.DocsURL); err != nil {
			return nil, fmt.Errorf("pack %q docs_url: %v", p.ID, err)
		}
		if len(p.Versions) == 0 {
			return nil, fmt.Errorf("pack %q has no versions", p.ID)
		}
		seenVersions := map[string]bool{}
		for _, v := range p.Versions {
			if v.Version == "" {
				return nil, fmt.Errorf("pack %q has a version with no version string", p.ID)
			}
			if seenVersions[v.Version] {
				return nil, fmt.Errorf("pack %q lists version %q twice", p.ID, v.Version)
			}
			seenVersions[v.Version] = true
			if v.ContractVersion < 1 {
				return nil, fmt.Errorf("pack %q version %q has no contract_version", p.ID, v.Version)
			}
			if !ImagePattern.MatchString(v.Image) {
				return nil, fmt.Errorf("pack %q version %q image is not a digest-pinned reference", p.ID, v.Version)
			}
			if v.Permissions == nil {
				return nil, fmt.Errorf("pack %q version %q: permissions is required", p.ID, v.Version)
			}
			if _, err := time.Parse("2006-01-02", v.ReleasedAt); err != nil {
				return nil, fmt.Errorf("pack %q version %q released_at is not a date", p.ID, v.Version)
			}
			if err := catalogfile.CheckWebURL(v.NotesURL); err != nil {
				return nil, fmt.Errorf("pack %q version %q notes_url: %v", p.ID, v.Version, err)
			}
		}
	}
	return &idx, nil
}
