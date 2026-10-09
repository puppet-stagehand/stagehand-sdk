package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/testreg"
)

// publishSigned runs build, push and (when sign is set) a test signature, and
// returns the catalog path, key path and the oci:// reference of the pushed
// index at its digest.
func publishSigned(t *testing.T, sign bool) (catalog, keyPath, indexRef string, signer *testreg.Signer) {
	t.Helper()
	isolateDocker(t)
	reg := testreg.New(t)
	candidate := testreg.PushCandidate(t, reg.Host+"/staging/hello", helloManifest(t))
	dir := t.TempDir()
	catalog = filepath.Join(dir, "catalog.yaml")
	writeFile(t, catalog, `format: 1
feeds:
  official:
    index: `+reg.Host+`/catalog
    images: `+reg.Host+`/packs
    visibility: public
  paid:
    index: harbor.private.invalid:5000/paid/catalog
    images: harbor.private.invalid:5000/paid/packs
    visibility: private
packs:
  - id: hello
    feed: official
    versions:
      - version: 0.1.0
        candidate: `+candidate+`
        released_at: 2026-10-09
  - id: puppet_paid
    feed: paid
    listing:
      name: Puppet Paid Add-on
      summary: A paid add-on.
      tier: ent
    versions:
      - version: 2.1.0
        candidate: harbor.private.invalid:5000/staging/paid@sha256:`+strings.Repeat("c", 64)+`
        released_at: 2026-09-15
`)
	signer = testreg.NewSigner(t)
	keyPath = testreg.WriteKey(t, dir, "cosign.pub", signer.PublicPEM)
	indexPath := filepath.Join(dir, "index.json")
	code, out, errs := runCmd(t, "build", "--catalog", catalog, "--feed", "official", "--now", "2026-10-09T14:30:12Z", "--key", keyPath,
		"--previous", "oci://"+reg.Host+"/catalog", "--allow-missing-previous", "--out", indexPath)
	if code != 0 {
		t.Fatalf("build exit %d\n%s\n%s", code, out, errs)
	}
	code, out, errs = runCmd(t, "push", "--index", indexPath, "--ref", "oci://"+reg.Host+"/catalog")
	if code != 0 {
		t.Fatalf("push exit %d\n%s\n%s", code, out, errs)
	}
	digest := strings.TrimSpace(out)
	if sign {
		signer.Sign(t, reg.Host+"/catalog@"+digest)
	}
	return catalog, keyPath, "oci://" + reg.Host + "/catalog@" + digest, signer
}

func TestListingCLIWritesTheDocument(t *testing.T) {
	catalog, keyPath, ref, _ := publishSigned(t, true)
	outPath := filepath.Join(t.TempDir(), "marquee.json")
	code, out, errs := runCmd(t, "listing", "--index-ref", ref, "--key", keyPath, "--catalog", catalog, "--feed", "official", "--out", outPath, "--format", "json")
	if code != 0 {
		t.Fatalf("listing exit %d\n%s\n%s", code, out, errs)
	}
	var res struct {
		OK          bool   `json:"ok"`
		IndexDigest string `json:"index_digest"`
		Packs       int    `json:"packs"`
		Paid        int    `json:"paid_listings"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || !res.OK || res.Packs != 1 || res.Paid != 1 || !strings.HasPrefix(res.IndexDigest, "sha256:") {
		t.Fatalf("json envelope %q: %v %+v", out, err, res)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || doc["schema_version"] != float64(1) {
		t.Fatalf("written document: %v\n%s", err, raw)
	}
	if strings.Contains(string(raw), "harbor.private.invalid") {
		t.Fatalf("the private feed's host reached the page data:\n%s", raw)
	}
}

func TestListingCLIWritesNothingWhenTheIndexDoesNotVerify(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sign     bool
		wrongKey bool
		code     string
	}{
		{"unsigned", false, false, "index_unsigned"},
		{"wrong key", true, true, "index_untrusted_signer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, keyPath, ref, _ := publishSigned(t, tc.sign)
			if tc.wrongKey {
				keyPath = testreg.WriteKey(t, t.TempDir(), "other.pub", testreg.NewSigner(t).PublicPEM)
			}
			outPath := filepath.Join(t.TempDir(), "marquee.json")
			code, out, errs := runCmd(t, "listing", "--index-ref", ref, "--key", keyPath, "--catalog", catalog, "--feed", "official", "--out", outPath, "--format", "json")
			if code != 1 || !strings.Contains(out, tc.code) {
				t.Fatalf("exit %d, want 1 with %s\n%s\n%s", code, tc.code, out, errs)
			}
			if _, err := os.Stat(outPath); !os.IsNotExist(err) {
				t.Fatalf("nothing may be written when verification fails (stat err %v)", err)
			}
		})
	}
}

func TestListingCLIUsageErrors(t *testing.T) {
	if code, _, _ := runCmd(t, "listing"); code != 2 {
		t.Fatalf("no flags: exit %d, want 2", code)
	}
	catalog, keyPath, ref, _ := publishSigned(t, true)
	if code, _, errs := runCmd(t, "listing", "--index-ref", ref, "--key", filepath.Join(t.TempDir(), "missing.pub"), "--catalog", catalog, "--feed", "official", "--out", filepath.Join(t.TempDir(), "o.json")); code != 2 {
		t.Fatalf("unreadable key: exit %d, want 2\n%s", code, errs)
	}
	if code, _, errs := runCmd(t, "listing", "--index-ref", ref, "--key", keyPath, "--catalog", filepath.Join(t.TempDir(), "missing.yaml"), "--feed", "official", "--out", filepath.Join(t.TempDir(), "o.json")); code != 2 {
		t.Fatalf("unreadable catalog: exit %d, want 2\n%s", code, errs)
	}
}

func TestCopyThroughTheCLI(t *testing.T) {
	isolateDocker(t)
	src := testreg.NewBasicAuth(t, "srcuser", "src-secret-value")
	dst := testreg.NewBasicAuth(t, "dstuser", "dst-secret-value")
	t.Setenv("SRC_USER", "srcuser")
	t.Setenv("SRC_PASS", "src-secret-value")
	t.Setenv("DST_USER", "dstuser")
	t.Setenv("DST_PASS", "dst-secret-value")
	candidate := testreg.PushCandidate(t, src.Host+"/staging/hello", helloManifest(t), remote.WithAuth(&authn.Basic{Username: "srcuser", Password: "src-secret-value"}))
	digest := candidate[strings.Index(candidate, "@")+1:]
	args := []string{"copy", "--from", candidate, "--to", dst.Host + "/packs/hello:0.1.0",
		"--from-username-env", "SRC_USER", "--from-password-env", "SRC_PASS", "--to-username-env", "DST_USER", "--to-password-env", "DST_PASS", "--format", "json"}

	for i, wantPresent := range []bool{false, true} {
		code, out, errs := runCmd(t, args...)
		if code != 0 {
			t.Fatalf("run %d: exit %d\n%s\n%s", i, code, out, errs)
		}
		var res struct {
			OK             bool   `json:"ok"`
			Digest         string `json:"digest"`
			AlreadyPresent bool   `json:"already_present"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil || !res.OK || res.Digest != digest || res.AlreadyPresent != wantPresent {
			t.Fatalf("run %d: envelope %q (%v) %+v, want digest %s already_present %v", i, out, err, res, digest, wantPresent)
		}
		for _, secret := range []string{"src-secret-value", "dst-secret-value"} {
			if strings.Contains(out+errs, secret) {
				t.Fatalf("a credential was printed:\n%s\n%s", out, errs)
			}
		}
	}

	code, out, _ := runCmd(t, "copy", "--from", candidate, "--to", dst.Host+"/packs/hello:0.1.0", "--from-username-env", "SRC_USER", "--from-password-env", "SRC_PASS",
		"--to-username-env", "DST_USER", "--to-password-env", "SRC_PASS", "--format", "json")
	if code != 1 || !strings.Contains(out, "destination_denied") {
		t.Fatalf("wrong destination password: exit %d\n%s", code, out)
	}
	if code, _, _ := runCmd(t, "copy", "--from", candidate); code != 2 {
		t.Fatalf("missing --to: exit %d, want 2", code)
	}
	if code, _, errs := runCmd(t, "copy", "--from", candidate, "--to", dst.Host+"/packs/hello:0.1.0", "--to-username-env", "NOT_SET_ANYWHERE", "--to-password-env", "DST_PASS"); code != 2 {
		t.Fatalf("unset username variable: exit %d, want 2\n%s", code, errs)
	}
	code, out, _ = runCmd(t, "copy", "--from", src.Host+"/staging/hello:candidate", "--to", dst.Host+"/packs/hello:0.1.0", "--format", "json")
	if code != 1 || !strings.Contains(out, "source_not_pinned") {
		t.Fatalf("tag-pinned source: exit %d\n%s", code, out)
	}
}
