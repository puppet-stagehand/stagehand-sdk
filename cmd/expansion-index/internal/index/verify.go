package index

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Signature errors. They mirror the console's keyring: a registry outage is
// never reported as "unsigned".
var (
	ErrUnsigned        = errors.New("unsigned: no cosign signature artifact for this digest")
	ErrUntrustedSigner = errors.New("untrusted signer: no signature on this digest verifies against the key")
)

const (
	cosignSignatureAnnotation = "dev.cosignproject.cosign/signature"
	maxSigLayers              = 16
	maxSigPayloadBytes        = 64 << 10
)

type simpleSigning struct {
	Critical struct {
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
	} `json:"critical"`
}

// verifySignature checks the cosign legacy signature of repo@digest the way
// the console's keyring does: the sha256-<hex>.sig artifact, each layer's
// annotation signature over sha256(payload) with a P-256 key, and a payload
// that names this digest (a signature for another image never transfers).
func verifySignature(ctx context.Context, repo name.Repository, digest string, keys []*ecdsa.PublicKey, ropts []remote.Option) error {
	if !strings.HasPrefix(digest, "sha256:") {
		return fmt.Errorf("digest %q uses an unsupported algorithm: only sha256 is verified", digest)
	}
	if len(keys) == 0 {
		return ErrUntrustedSigner
	}
	sigTag := repo.Tag("sha256-" + strings.TrimPrefix(digest, "sha256:") + ".sig")
	opts := append([]remote.Option{remote.WithContext(ctx)}, ropts...)
	img, err := remote.Image(sigTag, opts...)
	if err != nil {
		var terr *transport.Error
		if errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound {
			return ErrUnsigned
		}
		return fmt.Errorf("fetch the signature artifact for %s: %w", digest, err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		return fmt.Errorf("read the signature manifest for %s: %w", digest, err)
	}
	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("read the signature layers for %s: %w", digest, err)
	}
	if len(layers) == 0 || len(layers) != len(manifest.Layers) || len(layers) > maxSigLayers {
		return ErrUnsigned
	}
	for i, desc := range manifest.Layers {
		if desc.Size > maxSigPayloadBytes {
			continue
		}
		sigB64 := desc.Annotations[cosignSignatureAnnotation]
		if sigB64 == "" {
			return fmt.Errorf("signature layer %d for %s carries no %s annotation", i, digest, cosignSignatureAnnotation)
		}
		sig, err := base64.StdEncoding.DecodeString(sigB64)
		if err != nil {
			return fmt.Errorf("signature layer %d for %s: malformed base64 signature: %w", i, digest, err)
		}
		rc, err := layers[i].Uncompressed()
		if err != nil {
			return fmt.Errorf("read signature payload for %s: %w", digest, err)
		}
		payload, rerr := io.ReadAll(io.LimitReader(rc, maxSigPayloadBytes+1))
		cerr := rc.Close()
		if rerr != nil {
			return fmt.Errorf("read signature payload for %s: %w", digest, rerr)
		}
		if cerr != nil {
			return fmt.Errorf("close signature payload for %s: %w", digest, cerr)
		}
		if len(payload) > maxSigPayloadBytes {
			continue
		}
		var doc simpleSigning
		if err := json.Unmarshal(payload, &doc); err != nil {
			return fmt.Errorf("signature layer %d for %s: malformed simple-signing payload: %w", i, digest, err)
		}
		if doc.Critical.Image.DockerManifestDigest != digest {
			continue
		}
		sum := sha256.Sum256(payload)
		for _, k := range keys {
			if ecdsa.VerifyASN1(k, sum[:], sig) {
				return nil
			}
		}
	}
	return ErrUntrustedSigner
}

// VerifyOptions are the inputs of Verify.
type VerifyOptions struct {
	// Ref is oci://host/repo[:tag|@digest]; a bare repository means :latest.
	Ref string
	// KeyPEM is the trusted PEM P-256 public key.
	KeyPEM []byte
	// ExpectDigest, when set, must equal the resolved index digest.
	ExpectDigest string
	// Images also verifies each listed pack image's own signature.
	Images bool
	// Remote carries the registry options (credentials, pull scope).
	Remote []remote.Option
}

// VerifiedImage is one pack image listed by the index.
type VerifiedImage struct {
	Pack    string `json:"pack"`
	Version string `json:"version"`
	Image   string `json:"image"`
	// Verified is set only when --images asked for the check.
	Verified *bool `json:"verified,omitempty"`
}

// VerifyResult is a successful (or partially successful) verification.
type VerifyResult struct {
	Digest      string          `json:"digest"`
	GeneratedAt string          `json:"generated_at"`
	Images      []VerifiedImage `json:"images"`
}

// Verify resolves ref, checks the index's signature exactly as a console will,
// fetches it by digest and strictly decodes it.
func Verify(ctx context.Context, opts VerifyOptions) (*VerifyResult, []Finding) {
	key, err := ParsePublicKey(opts.KeyPEM)
	if err != nil {
		return nil, []Finding{{Code: "key_invalid", Path: "--key", Message: err.Error(), Fix: "Pass --key as a PEM file holding an ECDSA P-256 public key (cosign.pub)."}}
	}
	ref, err := ParseReference(opts.Ref)
	if err != nil {
		return nil, []Finding{{Code: "ref_invalid", Path: "--ref", Message: err.Error(), Fix: "Pass --ref as oci://<registry>/<repository>[:tag|@sha256:...]."}}
	}
	ropts := append([]remote.Option{remote.WithContext(ctx)}, opts.Remote...)
	var digestRef name.Digest
	if d, ok := ref.(name.Digest); ok {
		digestRef = d
	} else {
		desc, err := remote.Head(ref, ropts...)
		if err != nil {
			return nil, []Finding{registryFinding("--ref", "resolve "+ref.String(), err)}
		}
		digestRef = ref.Context().Digest(desc.Digest.String())
	}
	digest := digestRef.DigestStr()
	if opts.ExpectDigest != "" && opts.ExpectDigest != digest {
		return nil, []Finding{{Code: "digest_mismatch", Path: "--expect-digest", Message: fmt.Sprintf("the index resolves to %s, not %s", digest, opts.ExpectDigest),
			Fix: "Verify the digest that `expansion-index push` printed, or publish the index you meant."}}
	}
	if err := verifySignature(ctx, digestRef.Context(), digest, []*ecdsa.PublicKey{key}, opts.Remote); err != nil {
		return nil, []Finding{signatureFinding("--ref", "the index "+digest, err)}
	}
	idx, fs := fetchIndex(ctx, digestRef, opts.Remote)
	if len(fs) > 0 {
		return nil, fs
	}
	res := &VerifyResult{Digest: digest, GeneratedAt: idx.GeneratedAt, Images: []VerifiedImage{}}
	var findings []Finding
	for _, p := range idx.Packs {
		for _, v := range p.Versions {
			vi := VerifiedImage{Pack: p.ID, Version: v.Version, Image: v.Image}
			if opts.Images {
				ok := false
				imgRef, perr := name.NewDigest(v.Image, name.StrictValidation)
				if perr == nil {
					serr := verifySignature(ctx, imgRef.Context(), imgRef.DigestStr(), []*ecdsa.PublicKey{key}, opts.Remote)
					ok = serr == nil
					if serr != nil {
						f := signatureFinding(fmt.Sprintf("packs[%s].versions[%s]", p.ID, v.Version), fmt.Sprintf("pack %s version %s image %s", p.ID, v.Version, v.Image), serr)
						f.Code = "image_" + strings.TrimPrefix(f.Code, "index_")
						findings = append(findings, f)
					}
				} else {
					findings = append(findings, Finding{Code: "image_invalid", Path: fmt.Sprintf("packs[%s].versions[%s]", p.ID, v.Version), Message: perr.Error(), Fix: "Rebuild the index."})
				}
				vi.Verified = &ok
			}
			res.Images = append(res.Images, vi)
		}
	}
	return res, findings
}

// signatureFinding turns a verifySignature error into a finding.
func signatureFinding(path, what string, err error) Finding {
	switch {
	case errors.Is(err, ErrUnsigned):
		return Finding{Code: "index_unsigned", Path: path, Message: what + " is unsigned: no cosign signature artifact exists for it",
			Fix: "Sign the digest with cosign (the catalog workflow does this between push and promote) before anything trusts it."}
	case errors.Is(err, ErrUntrustedSigner):
		return Finding{Code: "index_untrusted_signer", Path: path, Message: what + " has an untrusted signer: no signature verifies against --key",
			Fix: "Verify with the public key that matches the signing key, or re-sign with the right private key."}
	}
	return registryFinding(path, "check the signature of "+what, err)
}

// registryFinding turns a registry error into a finding; a 401/403 is named a
// denied read.
func registryFinding(path, doing string, err error) Finding {
	switch classifyRegistryError(err) {
	case Denied:
		return Finding{Code: "registry_denied", Path: path, Message: "the registry denied the read while trying to " + doing + " (" + briefErr(err) + ")",
			Fix: "The repository is private, missing, or the credentials lack pull access: pass --username-env/--password-env, or check the repository name."}
	case NotFound:
		return Finding{Code: "not_found", Path: path, Message: "not found while trying to " + doing + " (" + briefErr(err) + ")",
			Fix: "Check the repository and tag exist."}
	}
	return Finding{Code: "registry_error", Path: path, Message: "failed to " + doing + ": " + briefErr(err),
		Fix: "Check the registry is reachable and retry."}
}

// fetchIndex reads and strictly decodes the one-layer index artifact at ref.
func fetchIndex(ctx context.Context, ref name.Digest, ropts []remote.Option) (*Index, []Finding) {
	opts := append([]remote.Option{remote.WithContext(ctx)}, ropts...)
	img, err := remote.Image(ref, opts...)
	if err != nil {
		return nil, []Finding{registryFinding("--ref", "fetch the index "+ref.String(), err)}
	}
	idx, err := readIndexArtifact(img)
	if err != nil {
		var ie *invalidArtifactError
		if errors.As(err, &ie) {
			return nil, []Finding{{Code: "index_invalid", Path: "--ref", Message: ref.String() + ": " + ie.Error(),
				Fix: "Republish the index with `expansion-index build` and `push`; a console refuses this artifact."}}
		}
		return nil, []Finding{registryFinding("--ref", "read the index "+ref.String(), err)}
	}
	return idx, nil
}

// invalidArtifactError marks an index artifact that is readable but wrong
// (layer count, media type, size, or strict decode), as opposed to a registry
// failure.
type invalidArtifactError struct{ msg string }

func (e *invalidArtifactError) Error() string { return e.msg }

// readIndexArtifact applies the console's artifact rules: exactly one layer of
// the index media type within the size cap, then DecodeStrict.
func readIndexArtifact(img v1.Image) (*Index, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	if len(layers) != 1 {
		return nil, &invalidArtifactError{fmt.Sprintf("index artifact has %d layers, want exactly 1", len(layers))}
	}
	mt, err := layers[0].MediaType()
	if err != nil {
		return nil, err
	}
	if string(mt) != IndexArtifactMediaType {
		return nil, &invalidArtifactError{fmt.Sprintf("index artifact layer has media type %q, want %s", mt, IndexArtifactMediaType)}
	}
	if size, err := layers[0].Size(); err == nil && size > MaxIndexBytes {
		return nil, &invalidArtifactError{fmt.Sprintf("index layer is %d bytes, more than %d", size, MaxIndexBytes)}
	}
	rc, err := layers[0].Compressed()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, MaxIndexBytes+1))
	if err != nil {
		return nil, err
	}
	idx, err := DecodeStrict(raw)
	if err != nil {
		return nil, &invalidArtifactError{err.Error()}
	}
	return idx, nil
}
