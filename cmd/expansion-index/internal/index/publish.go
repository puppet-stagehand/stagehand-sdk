package index

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// TagFor is the immutable tag an index generated at t is pushed under:
// YYYYMMDDTHHMMSSZ in UTC. It contains no colon, so it is a valid OCI tag.
func TagFor(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// ParseRepository parses oci://host/repo, host/repo, with an optional :tag or
// @digest, into a reference.
func ParseReference(ref string) (name.Reference, error) {
	return name.ParseReference(strings.TrimPrefix(ref, "oci://"))
}

// PushOptions are the inputs of Push.
type PushOptions struct {
	// Index is the exact document to publish (build's output).
	Index []byte
	// Repo is oci://host/repo, with no tag or digest.
	Repo string
	// Tag is the immutable tag; empty means TagFor(generated_at).
	Tag string
}

// Push writes the index as a one-layer OCI artifact under an immutable tag and
// returns its digest.
func Push(ctx context.Context, opts PushOptions) (string, []Finding) {
	idx, err := DecodeStrict(opts.Index)
	if err != nil {
		return "", []Finding{{Code: "index_invalid", Path: "--index", Message: "refusing to publish an index a console would refuse: " + err.Error(),
			Fix: "Rebuild the index with `expansion-index build`; never edit index.json by hand."}}
	}
	gen, err := time.Parse(time.RFC3339, idx.GeneratedAt)
	if idx.GeneratedAt == "" || err != nil {
		return "", []Finding{{Code: "index_unstamped", Path: "--index", Message: "the index has no valid generated_at",
			Fix: "Rebuild the index with `expansion-index build`, which always stamps it."}}
	}
	want := TagFor(gen)
	tag := opts.Tag
	if tag == "" {
		tag = want
	}
	if tag != want {
		return "", []Finding{{Code: "tag_mismatch", Path: "--tag", Message: fmt.Sprintf("tag %q does not match the index's generated_at (%s)", tag, want),
			Fix: "Use the tag `expansion-index build` printed, or omit --tag."}}
	}
	repo, err := name.NewRepository(strings.TrimPrefix(opts.Repo, "oci://"))
	if err != nil {
		return "", []Finding{{Code: "ref_invalid", Path: "--ref", Message: err.Error(), Fix: "Pass --ref as oci://<registry>/<repository> with no tag."}}
	}
	img, err := mutate.Append(empty.Image, mutate.Addendum{Layer: static.NewLayer(opts.Index, types.MediaType(IndexArtifactMediaType))})
	if err != nil {
		return "", []Finding{{Code: "internal_error", Path: "/", Message: err.Error(), Fix: "Report this as a bug."}}
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.MediaType(IndexConfigMediaType))
	d, err := img.Digest()
	if err != nil {
		return "", []Finding{{Code: "internal_error", Path: "/", Message: err.Error(), Fix: "Report this as a bug."}}
	}
	if err := remote.Write(repo.Tag(tag), img, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain)); err != nil {
		return "", []Finding{{Code: "push_failed", Path: "--ref", Message: briefErr(err),
			Fix: "Log in to the registry with credentials that may push this repository (docker login), then retry."}}
	}
	return d.String(), nil
}
