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
	"net/url"
	"os"
	"regexp"
	"sort"
	"time"

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

// Patterns catalog.yaml values must match. packIDPattern and candidatePattern
// are the console's indexPackIDPattern and indexImagePattern (the latter for a
// whole digest-pinned reference); repoPattern is the repository part of it.
var (
	packIDPattern    = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	repoPattern      = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+$`)
	candidatePattern = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+@sha256:[a-f0-9]{64}$`)
)

// Load reads, decodes and validates the catalog at path. The error is an I/O
// error (exit 2); findings are problems with the content (exit 1).
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
	if fs := Validate(&f); len(fs) > 0 {
		return &f, fs
	}
	return &f, nil
}

// CheckWebURL accepts an empty string or an http(s) URL with a host, no
// userinfo and no control characters or spaces. It is the console's
// checkWebURL: index and catalog links end up in the UI.
func CheckWebURL(raw string) error {
	if raw == "" {
		return nil
	}
	for _, r := range raw {
		if r <= 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return errors.New("must not contain control characters or spaces")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Hostname() == "" {
		return errors.New("must be an http(s) URL")
	}
	if u.User != nil {
		return errors.New("must not contain credentials")
	}
	return nil
}

// Validate applies the catalog.yaml format 1 rules. It needs no network.
func Validate(f *File) []Finding {
	var out []Finding
	add := func(code, path, msg, fix string) {
		out = append(out, Finding{Code: code, Path: path, Message: msg, Fix: fix})
	}

	if f.Format != 1 {
		add("catalog_format", "format", fmt.Sprintf("format is %d, want 1", f.Format), "Start catalog.yaml with `format: 1`.")
	}
	if len(f.Feeds) == 0 {
		add("catalog_no_feeds", "feeds", "no feed is declared", "Declare at least one feed with index, images and visibility.")
	}
	feedNames := make([]string, 0, len(f.Feeds))
	for n := range f.Feeds {
		feedNames = append(feedNames, n)
	}
	sort.Strings(feedNames)
	for _, n := range feedNames {
		fd := f.Feeds[n]
		p := "feeds." + n
		if !repoPattern.MatchString(fd.Index) {
			add("feed_index_invalid", p+".index", fmt.Sprintf("index %q is not a registry repository", fd.Index),
				"Use <registry>/<path> with lowercase characters and no scheme, tag or digest.")
		}
		if !repoPattern.MatchString(fd.Images) {
			add("feed_images_invalid", p+".images", fmt.Sprintf("images %q is not a registry repository prefix", fd.Images),
				"Use <registry>/<path> with lowercase characters and no scheme, tag or digest; each pack is published at <images>/<pack id>.")
		}
		if fd.Visibility != "public" && fd.Visibility != "private" {
			add("feed_visibility_invalid", p+".visibility", fmt.Sprintf("visibility %q is not public or private", fd.Visibility),
				"Set visibility to public or private.")
		}
	}

	seenPack := map[string]bool{}
	for i, pk := range f.Packs {
		p := fmt.Sprintf("packs[%d]", i)
		if packIDPattern.MatchString(pk.ID) {
			p = "packs[" + pk.ID + "]"
		} else {
			add("pack_id_invalid", p+".id", fmt.Sprintf("pack id %q is not valid", pk.ID), "Use 2-32 characters: a lowercase letter then lowercase letters, digits or underscores.")
		}
		if pk.ID != "" && seenPack[pk.ID] {
			add("pack_duplicate", p, fmt.Sprintf("pack %q is listed twice", pk.ID), "List each pack once and put all its versions under it.")
		}
		seenPack[pk.ID] = true
		fd, declared := f.Feeds[pk.Feed]
		if !declared {
			add("pack_feed_undeclared", p+".feed", fmt.Sprintf("pack %q is on feed %q, which is not declared", pk.ID, pk.Feed), "Declare the feed under `feeds:` or correct the pack's feed.")
		}
		if err := CheckWebURL(pk.DocsURL); err != nil {
			add("docs_url_invalid", p+".docs_url", "docs_url "+err.Error(), "Use an https:// link or remove docs_url.")
		}
		if declared && fd.Visibility == "private" {
			switch {
			case pk.Listing == nil:
				add("listing_missing", p+".listing", fmt.Sprintf("pack %q is on a private feed and has no listing", pk.ID),
					"Add `listing: {name, summary, tier}`: the public stub shown for a pack whose index is private.")
			case pk.Listing.Name == "":
				add("listing_name_missing", p+".listing.name", "listing has no name", "Set listing.name.")
			}
		}
		if pk.Listing != nil {
			switch pk.Listing.Tier {
			case "core", "ent", "adv":
			default:
				add("listing_tier_invalid", p+".listing.tier", fmt.Sprintf("listing tier %q is not core, ent or adv", pk.Listing.Tier), "Set listing.tier to core, ent or adv.")
			}
		}
		if len(pk.Versions) == 0 {
			add("pack_no_versions", p+".versions", fmt.Sprintf("pack %q lists no versions", pk.ID), "List at least one version, or remove the pack from catalog.yaml.")
		}
		seenVer := map[string]bool{}
		for j, v := range pk.Versions {
			vp := fmt.Sprintf("%s.versions[%d]", p, j)
			if v.Version != "" {
				vp = fmt.Sprintf("%s.versions[%s]", p, v.Version)
			}
			if v.Version == "" {
				add("version_missing", vp+".version", "version is empty", "Set the version to the pack's semantic version.")
			} else if seenVer[v.Version] {
				add("version_duplicate", vp, fmt.Sprintf("pack %q lists version %q twice", pk.ID, v.Version), "List each (pack, version) once.")
			}
			seenVer[v.Version] = true
			if !candidatePattern.MatchString(v.Candidate) {
				add("candidate_not_pinned", vp+".candidate", fmt.Sprintf("candidate %q is not <repository>@sha256:<64 hex>", v.Candidate),
					"Pin the candidate image by digest (lowercase); a tag is never accepted.")
			}
			if _, err := time.Parse("2006-01-02", v.ReleasedAt); err != nil {
				add("released_at_invalid", vp+".released_at", fmt.Sprintf("released_at %q is not a YYYY-MM-DD date", v.ReleasedAt), "Use a date such as 2026-10-09.")
			}
			if err := CheckWebURL(v.NotesURL); err != nil {
				add("notes_url_invalid", vp+".notes_url", "notes_url "+err.Error(), "Use an https:// link or remove notes_url.")
			}
		}
	}
	return out
}
