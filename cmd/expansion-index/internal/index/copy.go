package index

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// CopyOptions are the inputs of Copy.
type CopyOptions struct {
	// From is <repository>@sha256:<hex>, the reviewed candidate (optionally
	// prefixed oci://). A tag is refused.
	From string
	// To is <repository>:<tag>, where the image is published.
	To string
	// FromAuth reads the source (pull scope); nil means the default keychain
	// (docker login), which is anonymous when nothing is configured.
	FromAuth authn.Authenticator
	// ToAuth checks and writes the destination (push scope); nil means the
	// default keychain. It is deliberately separate from FromAuth: both sides
	// may be ghcr.io with different credentials.
	ToAuth authn.Authenticator
	// Transport is the base HTTP transport for both sides (nil:
	// remote.DefaultTransport). Tests use it to observe the traffic.
	Transport http.RoundTripper
}

// CopyResult is a successful copy.
type CopyResult struct {
	Digest         string `json:"digest"`
	AlreadyPresent bool   `json:"already_present"`
}

// Copy publishes the image at From into To without changing its digest
// (D-10): the manifest bytes are written as read, and the destination's digest
// is compared with the source's afterwards. When the destination repository
// already holds the digest nothing is written. The source is read with the
// pull scope; the destination is checked with the push scope it is about to
// use, so copying into a package nobody has created yet creates it instead of
// failing on a registry's anonymous-style denial. A 401 or 403 on either side
// is a finding, never tolerated.
func Copy(ctx context.Context, opts CopyOptions) (*CopyResult, []Finding) {
	srcRef, err := name.NewDigest(strings.TrimPrefix(opts.From, "oci://"), name.StrictValidation)
	if err != nil {
		return nil, []Finding{{Code: "source_not_pinned", Path: "--from",
			Message: fmt.Sprintf("--from %q is not <repository>@sha256:<64 hex>: %v", opts.From, err),
			Fix:     "Pass the candidate by digest (a tag can move): the build output's `candidate` value."}}
	}
	dstTag, err := name.NewTag(strings.TrimPrefix(opts.To, "oci://"), name.StrictValidation)
	if err != nil {
		return nil, []Finding{{Code: "destination_invalid", Path: "--to",
			Message: fmt.Sprintf("--to %q is not <repository>:<tag>: %v", opts.To, err),
			Fix:     "Pass the destination as <registry>/<repository>:<tag>, for example ghcr.io/org/packs/<pack id>:<version>."}}
	}
	base := opts.Transport
	if base == nil {
		base = remote.DefaultTransport
	}

	// Source: pull scope, by digest.
	srcOpts := []remote.Option{remote.WithContext(ctx), remote.WithTransport(base)}
	if opts.FromAuth != nil {
		srcOpts = append(srcOpts, remote.WithAuth(opts.FromAuth))
	}
	desc, err := remote.Get(srcRef, srcOpts...)
	if err != nil {
		switch classifyRegistryError(err) {
		case Denied:
			return nil, []Finding{{Code: "source_denied", Path: "--from",
				Message: fmt.Sprintf("the registry denied reading the source %s (%s)", srcRef.String(), briefErr(err)),
				Fix:     "Pass --from-username-env/--from-password-env naming environment variables that hold credentials with read access to the staging repository."}}
		case NotFound:
			return nil, []Finding{{Code: "source_not_found", Path: "--from",
				Message: fmt.Sprintf("the source %s does not exist (%s)", srcRef.String(), briefErr(err)),
				Fix:     "Check the candidate digest in catalog.yaml and that the pack repository pushed it."}}
		}
		return nil, []Finding{{Code: "source_unreadable", Path: "--from",
			Message: fmt.Sprintf("reading the source %s failed: %s", srcRef.String(), briefErr(err)),
			Fix:     "Check the registry is reachable and retry."}}
	}
	want := desc.Digest.String()
	if want != srcRef.DigestStr() {
		return nil, []Finding{{Code: "source_digest_mismatch", Path: "--from",
			Message: fmt.Sprintf("the source registry returned %s for %s", want, srcRef.String()),
			Fix:     "Do not publish from this registry: it did not return the content it was asked for."}}
	}

	// Destination: one push-scoped transport for the existence check and the write.
	repo := dstTag.Context()
	dstAuth := opts.ToAuth
	if dstAuth == nil {
		if dstAuth, err = authn.DefaultKeychain.Resolve(repo.Registry); err != nil {
			return nil, []Finding{{Code: "destination_failed", Path: "--to", Message: "reading the docker credentials failed: " + briefErr(err),
				Fix: "Check DOCKER_CONFIG / ~/.docker/config.json, or pass --to-username-env/--to-password-env."}}
		}
	}
	rt, err := pushScopedTransport(ctx, repo, dstAuth, base)
	if err != nil {
		return nil, []Finding{destinationFinding(repo, "authenticate for pushing", err)}
	}
	dstOpts := []remote.Option{remote.WithContext(ctx), remote.WithTransport(rt), remote.WithAuth(dstAuth)}

	present, err := remote.Head(repo.Digest(want), dstOpts...)
	switch {
	case err == nil:
		if present.Digest.String() != want {
			return nil, []Finding{mismatch(repo, want, present.Digest.String())}
		}
		return &CopyResult{Digest: want, AlreadyPresent: true}, nil
	case classifyRegistryError(err) != NotFound:
		return nil, []Finding{destinationFinding(repo, "check whether "+want+" is already there", err)}
	}

	if desc.MediaType.IsIndex() {
		idx, ierr := desc.ImageIndex()
		if ierr != nil {
			return nil, []Finding{{Code: "source_unreadable", Path: "--from", Message: "reading the source index failed: " + briefErr(ierr), Fix: "Check the registry is reachable and retry."}}
		}
		err = remote.WriteIndex(dstTag, idx, dstOpts...)
	} else {
		img, ierr := desc.Image()
		if ierr != nil {
			return nil, []Finding{{Code: "source_unreadable", Path: "--from", Message: "reading the source image failed: " + briefErr(ierr), Fix: "Check the registry is reachable and retry."}}
		}
		err = remote.Write(dstTag, img, dstOpts...)
	}
	if err != nil {
		return nil, []Finding{destinationFinding(repo, "write "+dstTag.String(), err)}
	}

	// Digest equality is the contract: a destination that stores or reports
	// anything else is not the image that was reviewed.
	got, err := remote.Head(dstTag, dstOpts...)
	if err != nil {
		return nil, []Finding{destinationFinding(repo, "read back "+dstTag.String(), err)}
	}
	if got.Digest.String() != want {
		return nil, []Finding{mismatch(repo, want, got.Digest.String())}
	}
	return &CopyResult{Digest: want}, nil
}

func mismatch(repo name.Repository, want, got string) Finding {
	return Finding{Code: "digest_mismatch", Path: "--to",
		Message: fmt.Sprintf("the destination %s holds %s but the source is %s", repo.String(), got, want),
		Fix:     "Do not sign or list this image. The destination registry changed the content (or reported a different digest); copy to a registry that preserves manifests."}
}

// destinationFinding turns a destination-side registry error into a finding; a
// 401 or 403 is named a denied destination.
func destinationFinding(repo name.Repository, doing string, err error) Finding {
	if classifyRegistryError(err) == Denied {
		return Finding{Code: "destination_denied", Path: "--to",
			Message: fmt.Sprintf("the registry denied the destination %s while trying to %s (%s)", repo.String(), doing, briefErr(err)),
			Fix:     "Pass --to-username-env/--to-password-env naming environment variables that hold credentials allowed to create and push this repository (on GHCR: a token with write:packages, or the workflow's GITHUB_TOKEN with the package's Actions access set to Write)."}
	}
	return Finding{Code: "destination_failed", Path: "--to",
		Message: fmt.Sprintf("failed to %s in %s: %s", doing, repo.String(), briefErr(err)),
		Fix:     "Check the registry is reachable and retry."}
}
