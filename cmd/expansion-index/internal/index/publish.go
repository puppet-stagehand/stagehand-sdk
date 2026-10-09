package index

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"regexp"
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
	// The immutable-tag check and the write share one push-scoped transport, so
	// a repository nobody has created yet answers "absent", never "denied".
	// (OCI has no conditional put: a tag created between the check and the
	// write by another publisher is not detectable here.)
	auth, err := authn.DefaultKeychain.Resolve(repo.Registry)
	if err != nil {
		return "", []Finding{{Code: "push_failed", Path: "--ref", Message: "reading the docker credentials failed: " + briefErr(err), Fix: "Check DOCKER_CONFIG / ~/.docker/config.json."}}
	}
	rt, err := pushScopedTransport(ctx, repo, auth, remote.DefaultTransport)
	if err != nil {
		return "", []Finding{pushFinding(repo, "authenticate for pushing", err)}
	}
	tagRef := repo.Tag(tag)
	existing, err := remote.Head(tagRef, remote.WithContext(ctx), remote.WithTransport(rt))
	switch {
	case err == nil:
		msg := fmt.Sprintf("the tag %s already exists in %s (it points at %s); published tags are immutable", tag, repo.String(), existing.Digest)
		if existing.Digest == d {
			msg = fmt.Sprintf("the tag %s already exists in %s with identical content (%s); published tags are immutable", tag, repo.String(), d)
		}
		return "", []Finding{{Code: "tag_exists", Path: "--tag", Message: msg,
			Fix: "Rebuild with a later generated_at (a new immutable tag); an existing tag is never overwritten."}}
	case classifyRegistryError(err) != NotFound:
		return "", []Finding{pushFinding(repo, "check whether the tag "+tag+" exists", err)}
	}
	if err := remote.Write(tagRef, img, remote.WithContext(ctx), remote.WithTransport(rt)); err != nil {
		return "", []Finding{pushFinding(repo, "write the index", err)}
	}
	return d.String(), nil
}

func pushFinding(repo name.Repository, doing string, err error) Finding {
	if classifyRegistryError(err) == Denied {
		return Finding{Code: "push_denied", Path: "--ref", Message: fmt.Sprintf("the registry denied the push to %s while trying to %s (%s)", repo.String(), doing, briefErr(err)),
			Fix: "Log in (docker login) with credentials that may create and push this repository, then retry."}
	}
	return Finding{Code: "push_failed", Path: "--ref", Message: fmt.Sprintf("failed to %s in %s: %s", doing, repo.String(), briefErr(err)),
		Fix: "Check the registry is reachable and retry."}
}

var (
	digestPattern       = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	immutableTagPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`)
)

// PromoteOptions are the inputs of Promote.
type PromoteOptions struct {
	// Repo is oci://host/repo, with no tag or digest.
	Repo string
	// Digest is the already-pushed index artifact to point the tag at.
	Digest string
	// Tag is the mutable tag to move; empty means latest.
	Tag string
	// KeyPEM is the PEM P-256 public key the digest must be signed by.
	KeyPEM []byte
}

// Promote moves a mutable tag (latest) to an already-pushed index digest, only
// after verifying that digest's cosign legacy signature against the key and
// that the artifact decodes under the console's rules. A digest that is
// unsigned, signed by another key or not an index leaves the tag where it was.
func Promote(ctx context.Context, o PromoteOptions) (string, []Finding) {
	key, err := ParsePublicKey(o.KeyPEM)
	if err != nil {
		return "", []Finding{{Code: "key_invalid", Path: "--key", Message: err.Error(), Fix: "Pass --key as a PEM file holding an ECDSA P-256 public key (cosign.pub)."}}
	}
	if !digestPattern.MatchString(o.Digest) {
		return "", []Finding{{Code: "digest_invalid", Path: "--digest", Message: fmt.Sprintf("%q is not sha256:<64 hex>", o.Digest), Fix: "Pass the digest `expansion-index push` printed."}}
	}
	tag := o.Tag
	if tag == "" {
		tag = "latest"
	}
	if immutableTagPattern.MatchString(tag) {
		return "", []Finding{{Code: "promote_immutable_tag", Path: "--tag", Message: fmt.Sprintf("%q has the form of an immutable index tag; promote moves mutable tags only", tag),
			Fix: "Promote latest (or another mutable tag); immutable tags are written once by push."}}
	}
	repo, err := name.NewRepository(strings.TrimPrefix(o.Repo, "oci://"))
	if err != nil {
		return "", []Finding{{Code: "ref_invalid", Path: "--ref", Message: err.Error(), Fix: "Pass --ref as oci://<registry>/<repository> with no tag."}}
	}
	auth, err := authn.DefaultKeychain.Resolve(repo.Registry)
	if err != nil {
		return "", []Finding{{Code: "promote_failed", Path: "--ref", Message: "reading the docker credentials failed: " + briefErr(err), Fix: "Check DOCKER_CONFIG / ~/.docker/config.json."}}
	}
	rt, err := pushScopedTransport(ctx, repo, auth, remote.DefaultTransport)
	if err != nil {
		return "", []Finding{pushFinding(repo, "authenticate for promoting", err)}
	}
	ropts := []remote.Option{remote.WithContext(ctx), remote.WithTransport(rt)}
	if err := verifySignature(ctx, repo, o.Digest, []*ecdsa.PublicKey{key}, ropts); err != nil {
		return "", []Finding{signatureFinding("--digest", "the index "+o.Digest, err)}
	}
	if _, fs := fetchIndex(ctx, repo.Digest(o.Digest), ropts); len(fs) > 0 {
		return "", fs
	}
	desc, err := remote.Get(repo.Digest(o.Digest), ropts...)
	if err != nil {
		return "", []Finding{pushFinding(repo, "read "+o.Digest, err)}
	}
	if err := remote.Tag(repo.Tag(tag), desc, ropts...); err != nil {
		return "", []Finding{pushFinding(repo, "move the tag "+tag, err)}
	}
	return o.Digest, nil
}
