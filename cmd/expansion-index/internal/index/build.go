package index

import (
	"archive/tar"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	semver "github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/catalogfile"
	"github.com/puppet-stagehand/stagehand-sdk/manifest"
)

// ImageManifestPath is where a pack image keeps its manifest. The console reads
// the same path (backend/internal/expansions/resolve.go ImageManifestPath).
const ImageManifestPath = "stagehand/manifest.json"

// Bounds on what build reads out of a candidate image (the console's own
// bounds: MaxImageManifestBytes and MaxImageScanBytes).
const (
	maxManifestBytes = 1 << 20
	maxScanBytes     = 256 << 20
)

// BuildOptions are the inputs of Build.
type BuildOptions struct {
	// Catalog is the decoded, validated catalog.yaml.
	Catalog *catalogfile.File
	// Feed names the feed (a key of Catalog.Feeds) to build.
	Feed string
	// Now stamps generated_at; it is truncated to whole seconds in UTC.
	Now time.Time
	// KeyPEM, when set, is listed (informationally) in publisher_keys.
	KeyPEM []byte
	// CandidateRemote carries the registry options (credentials) used to read
	// the unsigned candidate images in private staging.
	CandidateRemote []remote.Option
}

// ImageResult is one candidate and where it will be published.
type ImageResult struct {
	Pack        string `json:"pack"`
	Version     string `json:"version"`
	Candidate   string `json:"candidate"`
	Destination string `json:"destination"`
	// AlreadyListed is true when the previous index already lists Destination
	// (the same repository at the same digest).
	AlreadyListed bool `json:"already_listed"`
}

// BuildResult is a successful build.
type BuildResult struct {
	Index *Index
	// Raw is the exact index document to publish.
	Raw []byte
	// Tag is the immutable tag the index is pushed under.
	Tag string
	// Previous is PreviousFound, PreviousNotFound or "" when no previous read
	// was requested.
	Previous string
	Images   []ImageResult
}

type candidate struct {
	pack     catalogfile.Pack
	ver      catalogfile.Version
	manifest *manifest.Manifest
	digest   string
	dest     string
}

// Build turns the catalog and its candidate images into a v1 index. It reads
// and validates every candidate before returning anything; any finding means
// there is no result and nothing may be signed.
func Build(ctx context.Context, opts BuildOptions) (*BuildResult, []Finding) {
	feed, ok := opts.Catalog.Feeds[opts.Feed]
	if !ok {
		return nil, []Finding{{Code: "unknown_feed", Path: "feed", Message: fmt.Sprintf("catalog.yaml declares no feed %q", opts.Feed),
			Fix: "Pass --feed with one of the names under `feeds:` in catalog.yaml."}}
	}
	now := opts.Now.UTC().Truncate(time.Second)

	var findings []Finding
	var cands []candidate
	packs := append([]catalogfile.Pack(nil), opts.Catalog.Packs...)
	sort.SliceStable(packs, func(i, j int) bool { return packs[i].ID < packs[j].ID })
	for _, p := range packs {
		if p.Feed != opts.Feed {
			continue
		}
		for _, v := range p.Versions {
			c, fs := readCandidate(ctx, feed, p, v, opts.CandidateRemote)
			findings = append(findings, fs...)
			if c != nil {
				cands = append(cands, *c)
			}
		}
	}

	res := &BuildResult{}
	var prev *Index
	if len(findings) > 0 {
		return nil, findings
	}

	idx := &Index{ForgeVersion: 1, Name: opts.Feed, GeneratedAt: now.Format(time.RFC3339), PublisherKeys: []IndexPublisherKey{}, Packs: []IndexPack{}}
	if len(opts.KeyPEM) > 0 {
		k, err := publisherKey(opts.KeyPEM, opts.Feed)
		if err != nil {
			return nil, []Finding{{Code: "key_invalid", Path: "--key", Message: err.Error(),
				Fix: "Pass --key as a PEM file holding an ECDSA P-256 public key (cosign.pub)."}}
		}
		idx.PublisherKeys = append(idx.PublisherKeys, k)
	}

	listed := listedImages(prev)
	byPack := map[string][]candidate{}
	var ids []string
	for _, c := range cands {
		if _, seen := byPack[c.pack.ID]; !seen {
			ids = append(ids, c.pack.ID)
		}
		byPack[c.pack.ID] = append(byPack[c.pack.ID], c)
	}
	sort.Strings(ids)
	for _, id := range ids {
		group := byPack[id]
		sort.SliceStable(group, func(i, j int) bool { return newer(group[i], group[j]) })
		newest := group[0]
		m := newest.manifest
		ip := IndexPack{
			ID: id, Name: m.Name, Publisher: m.Publisher, Tier: m.Tier, Licence: m.Licence,
			Entitlement: m.Entitlement, Summary: m.Summary, DocsURL: newest.pack.DocsURL,
		}
		if ip.DocsURL == "" {
			ip.DocsURL = m.DocsURL
		}
		for _, c := range group {
			perms := c.manifest.Permissions
			if perms == nil {
				perms = []string{}
			}
			iv := IndexVersion{
				Version: c.ver.Version, ContractVersion: c.manifest.ContractVersion, Image: c.dest,
				Permissions: perms, ReleasedAt: c.ver.ReleasedAt, NotesURL: c.ver.NotesURL,
			}
			if c.manifest.Content != nil {
				iv.Content = &IndexContent{ForgeSlug: c.manifest.Content.ForgeSlug, Version: c.manifest.Content.Version}
			}
			ip.Versions = append(ip.Versions, iv)
			res.Images = append(res.Images, ImageResult{
				Pack: id, Version: c.ver.Version, Candidate: c.ver.Candidate, Destination: c.dest, AlreadyListed: listed[c.dest],
			})
		}
		idx.Packs = append(idx.Packs, ip)
	}

	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return nil, []Finding{{Code: "internal_error", Path: "/", Message: err.Error(), Fix: "Report this as a bug."}}
	}
	raw = append(raw, '\n')
	// Self-check: the document must decode under the console's strict rules.
	if _, err := DecodeStrict(raw); err != nil {
		return nil, []Finding{{Code: "index_invalid", Path: "/", Message: "the assembled index would be refused by a console: " + err.Error(),
			Fix: "Fix the catalog.yaml value or the candidate manifest the message names, then rebuild."}}
	}
	res.Index, res.Raw, res.Tag = idx, raw, TagFor(now)
	return res, nil
}

func listedImages(prev *Index) map[string]bool {
	out := map[string]bool{}
	if prev == nil {
		return out
	}
	for _, p := range prev.Packs {
		for _, v := range p.Versions {
			out[v.Image] = true
		}
	}
	return out
}

// newer reports whether a sorts before b: later released_at first, then the
// higher version.
func newer(a, b candidate) bool {
	if a.ver.ReleasedAt != b.ver.ReleasedAt {
		return a.ver.ReleasedAt > b.ver.ReleasedAt
	}
	av, aerr := semver.NewVersion(a.ver.Version)
	bv, berr := semver.NewVersion(b.ver.Version)
	if aerr == nil && berr == nil {
		return av.GreaterThan(bv)
	}
	return a.ver.Version > b.ver.Version
}

func publisherKey(pemBytes []byte, feedName string) (IndexPublisherKey, error) {
	pub, err := ParsePublicKey(pemBytes)
	if err != nil {
		return IndexPublisherKey{}, err
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return IndexPublisherKey{}, err
	}
	sum := sha256.Sum256(der)
	return IndexPublisherKey{ID: "sha256:" + hex.EncodeToString(sum[:]), Name: feedName, PublicKey: string(pemBytes)}, nil
}

// ParsePublicKey parses a PEM PKIX ECDSA P-256 public key, the form cosign
// writes as cosign.pub and the console's keyring accepts.
func ParsePublicKey(pemBytes []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("not a PEM document")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("not a PKIX public key: %v", err)
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok || pub.Curve.Params().Name != "P-256" {
		return nil, errors.New("not an ECDSA P-256 public key")
	}
	return pub, nil
}

// readCandidate reads one candidate image's manifest and cross-checks it
// against the catalog entry.
func readCandidate(ctx context.Context, feed catalogfile.Feed, p catalogfile.Pack, v catalogfile.Version, ropts []remote.Option) (*candidate, []Finding) {
	where := fmt.Sprintf("packs[%s].versions[%s]", p.ID, v.Version)
	fail := func(code, msg, fix string) (*candidate, []Finding) {
		return nil, []Finding{{Code: code, Path: where, Message: fmt.Sprintf("pack %s version %s: %s", p.ID, v.Version, msg), Fix: fix}}
	}
	ref, err := name.NewDigest(v.Candidate, name.StrictValidation)
	if err != nil {
		return fail("candidate_not_pinned", fmt.Sprintf("candidate %q is not a digest-pinned reference", v.Candidate),
			"Set candidate to <repository>@sha256:<64 hex> (a tag is never accepted).")
	}
	raw, err := extractManifest(ctx, ref, ropts)
	if err != nil {
		if errors.Is(err, errNoManifest) {
			return fail("candidate_no_manifest", "the candidate image has no "+ImageManifestPath,
				"Build the pack image with expansion-build so its top layer carries stagehand/manifest.json.")
		}
		if isDenied(err) {
			return fail("candidate_denied", "the registry denied reading the candidate image ("+briefErr(err)+")",
				"Pass --candidate-username-env/--candidate-password-env naming environment variables that hold credentials with read access to the staging repository.")
		}
		return fail("candidate_unreadable", "reading the candidate image failed: "+briefErr(err),
			"Check the candidate reference exists and the registry is reachable, then rebuild.")
	}
	m, fs := manifest.Parse(raw)
	if m != nil {
		fs = append(fs, manifest.Validate(m)...)
	}
	if len(fs) > 0 {
		out := make([]Finding, 0, len(fs))
		for _, f := range fs {
			out = append(out, Finding{Code: "candidate_manifest_invalid", Path: where + f.Path,
				Message: fmt.Sprintf("pack %s version %s: manifest %s: %s", p.ID, v.Version, f.Code, f.Message),
				Fix:     "In the pack source: " + f.Fix})
		}
		return nil, out
	}
	if m.ID != p.ID {
		return fail("manifest_id_mismatch", fmt.Sprintf("catalog.yaml lists the id %q but the image's manifest says %q", p.ID, m.ID),
			"Correct the pack id in catalog.yaml, or point the candidate at the right image.")
	}
	if m.Version != v.Version {
		return fail("manifest_version_mismatch", fmt.Sprintf("catalog.yaml lists version %q but the image's manifest says %q", v.Version, m.Version),
			"Correct the version in catalog.yaml, or point the candidate at the image built for that version.")
	}
	digest := ref.DigestStr()
	return &candidate{pack: p, ver: v, manifest: m, digest: digest, dest: feed.Images + "/" + p.ID + "@" + digest}, nil
}

var errNoManifest = errors.New("no " + ImageManifestPath + " in the image")

// extractManifest reads stagehand/manifest.json from the image, scanning
// layers topmost first (the topmost layer wins, a whiteout deletes it), as the
// console does at install time.
func extractManifest(ctx context.Context, ref name.Digest, ropts []remote.Option) ([]byte, error) {
	opts := append([]remote.Option{remote.WithContext(ctx)}, ropts...)
	img, err := remote.Image(ref, opts...)
	if err != nil {
		return nil, err
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	budget := int64(maxScanBytes)
	for i := len(layers) - 1; i >= 0; i-- {
		raw, found, whiteout, err := scanLayer(layers[i], &budget)
		if err != nil {
			return nil, err
		}
		if whiteout {
			return nil, errNoManifest
		}
		if found {
			return raw, nil
		}
	}
	return nil, errNoManifest
}

func scanLayer(layer v1.Layer, budget *int64) (raw []byte, found, whiteout bool, err error) {
	rc, err := layer.Uncompressed()
	if err != nil {
		return nil, false, false, err
	}
	defer rc.Close()
	lr := &io.LimitedReader{R: rc, N: *budget + 1}
	tr := tar.NewReader(lr)
	wh := path.Join(path.Dir(ImageManifestPath), ".wh."+path.Base(ImageManifestPath))
	for {
		hdr, herr := tr.Next()
		if errors.Is(herr, io.EOF) {
			break
		}
		if herr != nil {
			return nil, false, false, herr
		}
		switch strings.TrimPrefix(path.Clean(hdr.Name), "/") {
		case wh:
			whiteout = true
		case ImageManifestPath:
			if hdr.Typeflag != tar.TypeReg {
				return nil, false, false, fmt.Errorf("%s is not a regular file", ImageManifestPath)
			}
			if hdr.Size > maxManifestBytes {
				return nil, false, false, fmt.Errorf("%s is %d bytes, more than %d", ImageManifestPath, hdr.Size, maxManifestBytes)
			}
			b, rerr := io.ReadAll(io.LimitReader(tr, maxManifestBytes+1))
			if rerr != nil {
				return nil, false, false, rerr
			}
			raw, found = b, true
		}
	}
	// Drain to EOF so the registry's digest check on the layer runs.
	if _, err := io.Copy(io.Discard, lr); err != nil {
		return nil, false, false, err
	}
	if lr.N <= 0 {
		return nil, false, false, fmt.Errorf("the image is larger than the %d bytes build will scan", maxScanBytes)
	}
	*budget = lr.N - 1
	return raw, found, whiteout, nil
}

// briefErr is err's first line, for use in a finding message.
func briefErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// isDenied reports whether err is a registry 401 or 403.
func isDenied(err error) bool {
	var terr *transport.Error
	return errors.As(err, &terr) && (terr.StatusCode == http.StatusUnauthorized || terr.StatusCode == http.StatusForbidden)
}

// PreviousOutcome is how reading the currently published index ended.
type PreviousOutcome int

const (
	// Found: the previous index was read.
	Found PreviousOutcome = iota + 1
	// NotFound: the registry says the repository or tag does not exist.
	NotFound
	// Denied: the registry answered 401 or 403 (including a 403 DENIED from
	// its token endpoint). Never tolerated.
	Denied
	// Failed: anything else (outage, TLS, 5xx, an unknown 404 shape).
	Failed
)

// classifyRegistryError maps a registry error onto a PreviousOutcome. A 404
// whose diagnostics are all NAME_UNKNOWN or MANIFEST_UNKNOWN, or a 404 with an
// empty body (a HEAD answer carries none), is NotFound; a 401 or 403 is
// Denied; everything else is Failed.
func classifyRegistryError(err error) PreviousOutcome {
	var terr *transport.Error
	if !errors.As(err, &terr) {
		return Failed
	}
	switch terr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return Denied
	case http.StatusNotFound:
		if len(terr.Errors) == 0 {
			if emptyBody(terr) {
				return NotFound
			}
			return Failed
		}
		for _, d := range terr.Errors {
			if d.Code != transport.NameUnknownErrorCode && d.Code != transport.ManifestUnknownErrorCode {
				return Failed
			}
		}
		return NotFound
	}
	return Failed
}

// emptyBody reports whether a structured-less 404 carried no body. transport
// keeps the raw body private, but its message ends with the status text only
// when the body was empty.
func emptyBody(terr *transport.Error) bool {
	msg := terr.Error()
	status := fmt.Sprintf("unexpected status code %d %s", terr.StatusCode, http.StatusText(terr.StatusCode))
	return strings.HasSuffix(msg, status) || strings.HasSuffix(msg, status+" (HEAD responses have no body, use GET for details)")
}
