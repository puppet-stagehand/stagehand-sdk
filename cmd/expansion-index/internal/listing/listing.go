// Package listing writes the Marquee page data: the static JSON document the
// stagehand-docs site renders (stagehand-docs src/data/schema/marquee.schema.json,
// schema_version 1).
//
// The document is built from two sources and nothing else:
//
//   - the free packs come from the feed's index, after the index's cosign
//     signature has been re-verified against the trusted key (the same
//     index.Verify the console-side check is modelled on), so the page can only
//     ever show what was signed;
//   - the paid listings come from the `listing` blocks of catalog.yaml packs on
//     feeds whose visibility is private. They carry a name, a summary, a tier
//     and release dates, and never an image reference, a digest or a registry
//     host. The private feed is never contacted: the only registry this package
//     dials is the one --index-ref names.
package listing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	semver "github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/catalogfile"
	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/index"
)

// SchemaVersion is the Marquee document version this package writes.
const SchemaVersion = 1

// PaidLabel is the only label a paid listing may carry (the schema's const).
const PaidLabel = "Perforce add-on, licence required"

// Finding is one problem that stops the listing, with a fix line.
type Finding = catalogfile.Finding

// Document is the Marquee listing document.
type Document struct {
	SchemaVersion int `json:"schema_version"`
	// GeneratedAt is the verified index's generated_at; null when the index
	// carries none.
	GeneratedAt  *string       `json:"generated_at"`
	Feed         string        `json:"feed"`
	IndexDigest  string        `json:"index_digest"`
	PublisherKey PublisherKey  `json:"publisher_key"`
	Packs        []Pack        `json:"packs"`
	PaidListings []PaidListing `json:"paid_listings"`
}

// PublisherKey is the trusted key the index was verified against.
type PublisherKey struct {
	// Fingerprint is "sha256:" + hex of the sha256 of the key's PKIX DER.
	Fingerprint string `json:"fingerprint"`
	PEM         string `json:"pem"`
}

// Pack is a free (or otherwise publicly installable) pack from the index.
type Pack struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Summary     string        `json:"summary"`
	Publisher   string        `json:"publisher"`
	Tier        string        `json:"tier"`
	Entitlement string        `json:"entitlement"`
	Licence     string        `json:"licence"`
	Versions    []PackVersion `json:"versions"`
}

// PackVersion is one released version of a pack, as signed in the index.
type PackVersion struct {
	Version    string `json:"version"`
	ReleasedAt string `json:"released_at"`
	Image      string `json:"image"`
	Digest     string `json:"digest"`
}

// PaidListing is the public stub for a pack on a private feed. It has no image,
// digest or host by construction: the type has no field for them.
type PaidListing struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Summary  string        `json:"summary"`
	Tier     string        `json:"tier"`
	Label    string        `json:"label"`
	Versions []PaidVersion `json:"versions"`
}

// PaidVersion is a released version of a paid add-on.
type PaidVersion struct {
	Version    string `json:"version"`
	ReleasedAt string `json:"released_at"`
}

// Options are the inputs of Build.
type Options struct {
	// IndexRef is oci://host/repo[@sha256:...|:tag], the feed's index.
	IndexRef string
	// KeyPEM is the trusted PEM P-256 public key the index must be signed by.
	KeyPEM []byte
	// Catalog is the decoded catalog.yaml.
	Catalog *catalogfile.File
	// Feed names the public feed whose index IndexRef is.
	Feed string
	// Remote carries registry options for reading the index (none: the default
	// keychain, which is anonymous for a public feed).
	Remote []remote.Option
}

// Build verifies the index and assembles the listing. Any finding means there
// is no document.
func Build(ctx context.Context, opts Options) (*Document, []Finding) {
	if opts.Catalog == nil {
		return nil, []Finding{{Code: "catalog_missing", Path: "--catalog", Message: "no catalog.yaml was given", Fix: "Pass --catalog with the reviewed catalog.yaml."}}
	}
	if fs := catalogfile.Validate(opts.Catalog); len(fs) > 0 {
		return nil, fs
	}
	feed, ok := opts.Catalog.Feeds[opts.Feed]
	if !ok {
		return nil, []Finding{{Code: "unknown_feed", Path: "--feed", Message: fmt.Sprintf("catalog.yaml declares no feed %q", opts.Feed),
			Fix: "Pass --feed with one of the names under `feeds:` in catalog.yaml."}}
	}
	if feed.Visibility != "public" {
		return nil, []Finding{{Code: "listing_feed_private", Path: "--feed", Message: fmt.Sprintf("feed %q is private; the Marquee page lists a public feed only", opts.Feed),
			Fix: "Pass the public feed's name. Packs on private feeds reach the page through their `listing` blocks, never by reading the private feed."}}
	}

	pub, err := index.ParsePublicKey(opts.KeyPEM)
	if err != nil {
		return nil, []Finding{{Code: "key_invalid", Path: "--key", Message: err.Error(), Fix: "Pass --key as a PEM file holding an ECDSA P-256 public key (cosign.pub)."}}
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, []Finding{{Code: "key_invalid", Path: "--key", Message: err.Error(), Fix: "Pass --key as a PEM file holding an ECDSA P-256 public key (cosign.pub)."}}
	}
	sum := sha256.Sum256(der)
	// The page shows this text, so it is re-encoded from the parsed key and
	// never copied from the file: the fingerprint and the PEM derive from the
	// same bytes.
	canonicalPEM, err := index.CanonicalPEM(pub)
	if err != nil {
		return nil, []Finding{{Code: "key_invalid", Path: "--key", Message: err.Error(), Fix: "Pass --key as a PEM file holding an ECDSA P-256 public key (cosign.pub)."}}
	}

	ref, err := index.ParseReference(opts.IndexRef)
	if err != nil {
		return nil, []Finding{{Code: "ref_invalid", Path: "--index-ref", Message: err.Error(), Fix: "Pass --index-ref as oci://<registry>/<repository>@sha256:<hex>."}}
	}
	feedRepo, err := name.NewRepository(feed.Index)
	if err != nil {
		return nil, []Finding{{Code: "feed_index_invalid", Path: "feeds." + opts.Feed + ".index", Message: err.Error(), Fix: "Use <registry>/<path> with no scheme, tag or digest."}}
	}
	if ref.Context().Name() != feedRepo.Name() {
		return nil, []Finding{{Code: "index_ref_mismatch", Path: "--index-ref",
			Message: fmt.Sprintf("--index-ref names %s but feed %q publishes its index at %s", ref.Context().Name(), opts.Feed, feedRepo.Name()),
			Fix:     "Pass the index of the feed named by --feed, or the --feed whose `index:` is this repository."}}
	}

	// Re-verify the signature before anything is written; this is the only
	// registry contact.
	res, fs := index.Verify(ctx, index.VerifyOptions{Ref: opts.IndexRef, KeyPEM: opts.KeyPEM, Remote: opts.Remote})
	if len(fs) > 0 {
		return nil, fs
	}
	if res == nil || res.Index == nil {
		return nil, []Finding{{Code: "internal_error", Path: "/", Message: "verification returned no index", Fix: "Report this as a bug."}}
	}

	doc := &Document{
		SchemaVersion: SchemaVersion,
		Feed:          "oci://" + feedRepo.Name(),
		IndexDigest:   res.Digest,
		PublisherKey:  PublisherKey{Fingerprint: "sha256:" + hex.EncodeToString(sum[:]), PEM: canonicalPEM},
		Packs:         []Pack{},
		PaidListings:  []PaidListing{},
	}
	if res.Index.GeneratedAt != "" {
		g := res.Index.GeneratedAt
		doc.GeneratedAt = &g
	}

	var findings []Finding
	taken := map[string]bool{}
	for _, p := range res.Index.Packs {
		taken[p.ID] = true
		if p.Summary == "" {
			findings = append(findings, Finding{Code: "pack_summary_missing", Path: "packs[" + p.ID + "]",
				Message: fmt.Sprintf("pack %s has no summary in the signed index, and the page needs one", p.ID),
				Fix:     "Give the pack a summary in its manifest.json, rebuild its image and republish the index."})
			continue
		}
		pk := Pack{ID: p.ID, Name: p.Name, Summary: p.Summary, Publisher: p.Publisher, Tier: p.Tier, Entitlement: p.Entitlement, Licence: p.Licence}
		for _, v := range p.Versions {
			// The strict index decode guarantees a digest-pinned image
			// (<repository>@sha256:<hex>); the split is still checked, so a
			// reference without "@" can never yield a made-up digest.
			_, digest, pinned := strings.Cut(v.Image, "@")
			if !pinned || !index.DigestPattern.MatchString(digest) {
				findings = append(findings, Finding{Code: "image_not_pinned", Path: "packs[" + p.ID + "].versions[" + v.Version + "]",
					Message: fmt.Sprintf("pack %s version %s lists an image that is not digest-pinned", p.ID, v.Version),
					Fix:     "Republish the index with `expansion-index build`; a console refuses this index too."})
				continue
			}
			pk.Versions = append(pk.Versions, PackVersion{Version: v.Version, ReleasedAt: v.ReleasedAt, Image: v.Image, Digest: digest})
		}
		doc.Packs = append(doc.Packs, pk)
	}

	paid, pfs := paidListings(opts.Catalog, taken)
	findings = append(findings, pfs...)
	if len(findings) > 0 {
		return nil, findings
	}
	doc.PaidListings = paid
	return doc, nil
}

// paidListings maps the packs of private feeds to listings, from their listing
// blocks and catalog version dates only, ordered by pack id.
func paidListings(cat *catalogfile.File, takenIDs map[string]bool) ([]PaidListing, []Finding) {
	var packs []catalogfile.Pack
	for _, p := range cat.Packs {
		if f, ok := cat.Feeds[p.Feed]; ok && f.Visibility == "private" {
			packs = append(packs, p)
		}
	}
	sort.SliceStable(packs, func(i, j int) bool { return packs[i].ID < packs[j].ID })

	out := []PaidListing{}
	var findings []Finding
	for _, p := range packs {
		where := "packs[" + p.ID + "].listing"
		switch {
		case p.Listing == nil || p.Listing.Name == "":
			findings = append(findings, Finding{Code: "listing_missing", Path: where, Message: fmt.Sprintf("pack %s is on a private feed and has no listing name", p.ID),
				Fix: "Add `listing: {name, summary, tier}` to the pack in catalog.yaml."})
			continue
		case p.Listing.Summary == "":
			findings = append(findings, Finding{Code: "listing_summary_missing", Path: where + ".summary", Message: fmt.Sprintf("the listing of pack %s has no summary, and the page needs one", p.ID),
				Fix: "Set listing.summary in catalog.yaml."})
			continue
		case takenIDs[p.ID]:
			findings = append(findings, Finding{Code: "listing_id_collision", Path: where, Message: fmt.Sprintf("pack id %s is both a listed paid add-on and a pack in the public index", p.ID),
				Fix: "Pack ids are unique across the page: rename one of them."})
			continue
		}
		l := PaidListing{ID: p.ID, Name: p.Listing.Name, Summary: p.Listing.Summary, Tier: p.Listing.Tier, Label: PaidLabel}
		vs := append([]catalogfile.Version(nil), p.Versions...)
		sort.SliceStable(vs, func(i, j int) bool { return newerVersion(vs[i], vs[j]) })
		for _, v := range vs {
			l.Versions = append(l.Versions, PaidVersion{Version: v.Version, ReleasedAt: v.ReleasedAt})
		}
		out = append(out, l)
	}
	return out, findings
}

// newerVersion reports whether a sorts before b: later release date first,
// then the higher version.
func newerVersion(a, b catalogfile.Version) bool {
	if a.ReleasedAt != b.ReleasedAt {
		return a.ReleasedAt > b.ReleasedAt
	}
	av, aerr := semver.NewVersion(a.Version)
	bv, berr := semver.NewVersion(b.Version)
	if aerr == nil && berr == nil {
		return av.GreaterThan(bv)
	}
	return a.Version > b.Version
}

// Marshal renders doc with stable field order, two-space indentation and one
// trailing newline.
func Marshal(doc *Document) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
