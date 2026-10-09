// expansion-index builds, publishes and verifies a Stagehand feed index: the
// signed document a console reads to list the packs of a feed.
//
//	expansion-index build   --catalog catalog.yaml --feed <name> --out index.json [--previous oci://<index repo>] ...
//	expansion-index push    --index index.json --ref oci://<index repo> [--tag YYYYMMDDTHHMMSSZ]
//	expansion-index promote --ref oci://<index repo> --digest sha256:<hex> --key cosign.pub [--tag latest]
//	expansion-index verify  --ref oci://<repo>[:tag|@digest] --key cosign.pub [--expect-digest sha256:...] [--images]
//	expansion-index listing --index-ref oci://<index repo>@sha256:<hex> --key cosign.pub --catalog catalog.yaml --feed <name> --out marquee.json
//
// Signing is not in this tool: the catalog workflow runs the pinned cosign CLI
// between push and promote. Exit 0 ok, 1 findings, 2 usage or environment
// error. Registry credentials are only ever read from the environment
// variables a flag names, never from flag values, and are never printed.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/catalogfile"
	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/index"
	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/listing"
)

const usage = `usage: expansion-index <build|push|promote|verify|listing> [flags]
  build   --catalog catalog.yaml --feed NAME --out index.json [--previous oci://REPO] [--allow-missing-previous]
          [--now RFC3339] [--key cosign.pub] [--candidate-username-env N --candidate-password-env N]
          [--previous-username-env N --previous-password-env N] [--check] [--format json]
  push    --index index.json --ref oci://REPO [--tag YYYYMMDDTHHMMSSZ] [--format json]
  promote --ref oci://REPO --digest sha256:HEX --key cosign.pub [--tag latest] [--format json]
  verify  --ref oci://REPO[:TAG|@DIGEST] --key cosign.pub [--expect-digest sha256:HEX] [--images]
          [--username-env N --password-env N] [--format json]
  listing --index-ref oci://REPO@sha256:HEX --key cosign.pub --catalog catalog.yaml --feed NAME --out marquee.json
          [--format json]
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "build":
		return runBuild(args[1:], stdout, stderr)
	case "push":
		return runPush(args[1:], stdout, stderr)
	case "promote":
		return runPromote(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "listing":
		return runListing(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown subcommand %q\n%s", args[0], usage)
	return 2
}

// newFlagSet returns a flagset that reports errors on stderr and never exits.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// authFromEnv builds credentials from two environment-variable names. Both
// empty means anonymous (nil). The values are never printed.
func authFromEnv(userEnv, passEnv string) (authn.Authenticator, error) {
	if userEnv == "" && passEnv == "" {
		return nil, nil
	}
	if userEnv == "" || passEnv == "" {
		return nil, fmt.Errorf("give both the username and the password environment-variable names, or neither")
	}
	u, p := os.Getenv(userEnv), os.Getenv(passEnv)
	if u == "" {
		return nil, fmt.Errorf("environment variable %s is not set", userEnv)
	}
	if p == "" {
		return nil, fmt.Errorf("environment variable %s is not set", passEnv)
	}
	return &authn.Basic{Username: u, Password: p}, nil
}

func remoteAuth(a authn.Authenticator) []remote.Option {
	if a == nil {
		return nil
	}
	return []remote.Option{remote.WithAuth(a)}
}

// finish prints the envelope (json) or the text form and returns the exit code.
func finish(stdout io.Writer, format string, findings []index.Finding, doc map[string]any, okText string) int {
	if findings == nil {
		findings = []index.Finding{}
	}
	if format == "json" {
		doc["ok"] = len(findings) == 0
		doc["findings"] = findings
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(doc)
	} else if len(findings) == 0 {
		fmt.Fprintln(stdout, okText)
	} else {
		for _, f := range findings {
			fmt.Fprintln(stdout, f)
		}
		fmt.Fprintf(stdout, "%d finding(s)\n", len(findings))
	}
	if len(findings) > 0 {
		return 1
	}
	return 0
}

func runBuild(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("build", stderr)
	catalog := fs.String("catalog", "catalog.yaml", "the reviewed catalog.yaml")
	feed := fs.String("feed", "", "feed name (a key under feeds: in catalog.yaml)")
	now := fs.String("now", "", "RFC 3339 time stamped as generated_at (default: the UTC clock)")
	keyPath := fs.String("key", "", "PEM P-256 public key listed (informationally) in publisher_keys")
	candUser := fs.String("candidate-username-env", "", "environment variable holding the candidate registry username")
	candPass := fs.String("candidate-password-env", "", "environment variable holding the candidate registry password")
	previous := fs.String("previous", "", "oci://<index repository>: the currently published index (read for the generated_at rollback guard)")
	allowMissing := fs.Bool("allow-missing-previous", false, "tolerate a previous index that does not exist yet (the first publish); a 401 or 403 is never tolerated")
	prevUser := fs.String("previous-username-env", "", "environment variable holding the username that reads the previous index (with its password: read with the push scope)")
	prevPass := fs.String("previous-password-env", "", "environment variable holding the password that reads the previous index")
	check := fs.Bool("check", false, "validate catalog.yaml offline; read no registry and write nothing")
	out := fs.String("out", "", "where to write index.json")
	format := fs.String("format", "text", "text | json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || (!*check && (*feed == "" || *out == "" || *previous == "")) {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cat, findings, err := catalogfile.Load(*catalog)
	if err != nil {
		fmt.Fprintln(stderr, "expansion-index:", err)
		return 2
	}
	if *check && len(findings) == 0 && *feed != "" {
		if _, ok := cat.Feeds[*feed]; !ok {
			findings = append(findings, index.Finding{Code: "unknown_feed", Path: "feed", Message: fmt.Sprintf("catalog.yaml declares no feed %q", *feed),
				Fix: "Pass --feed with one of the names under `feeds:` in catalog.yaml."})
		}
	}
	if len(findings) > 0 || *check {
		return finish(stdout, *format, findings, map[string]any{}, "ok: "+*catalog+" is valid")
	}
	stamp := time.Now().UTC()
	if *now != "" {
		t, err := time.Parse(time.RFC3339, *now)
		if err != nil {
			fmt.Fprintf(stderr, "expansion-index: --now %q is not an RFC 3339 time\n", *now)
			return 2
		}
		stamp = t
	}
	var keyPEM []byte
	if *keyPath != "" {
		if keyPEM, err = os.ReadFile(*keyPath); err != nil {
			fmt.Fprintln(stderr, "expansion-index:", err)
			return 2
		}
	}
	cauth, err := authFromEnv(*candUser, *candPass)
	if err != nil {
		fmt.Fprintln(stderr, "expansion-index:", err)
		return 2
	}
	pauth, err := authFromEnv(*prevUser, *prevPass)
	if err != nil {
		fmt.Fprintln(stderr, "expansion-index:", err)
		return 2
	}
	res, bfs := index.Build(context.Background(), index.BuildOptions{
		Catalog: cat, Feed: *feed, Now: stamp, KeyPEM: keyPEM, CandidateRemote: remoteAuth(cauth),
		Previous: &index.PreviousOptions{Repo: *previous, AllowMissing: *allowMissing, Auth: pauth},
	})
	if len(bfs) > 0 {
		return finish(stdout, *format, bfs, map[string]any{"images": []index.ImageResult{}}, "")
	}
	if err := os.WriteFile(*out, res.Raw, 0o644); err != nil {
		fmt.Fprintln(stderr, "expansion-index:", err)
		return 2
	}
	doc := map[string]any{"tag": res.Tag, "generated_at": res.Index.GeneratedAt, "previous": res.Previous, "images": res.Images}
	return finish(stdout, *format, nil, doc, fmt.Sprintf("ok: wrote %s (%d pack(s)), tag %s", *out, len(res.Index.Packs), res.Tag))
}

func runPush(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("push", stderr)
	indexPath := fs.String("index", "", "index.json written by build")
	ref := fs.String("ref", "", "oci://<registry>/<index repository>")
	tag := fs.String("tag", "", "immutable tag (default: derived from generated_at)")
	format := fs.String("format", "text", "text | json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *indexPath == "" || *ref == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	raw, err := os.ReadFile(*indexPath)
	if err != nil {
		fmt.Fprintln(stderr, "expansion-index:", err)
		return 2
	}
	digest, findings := index.Push(context.Background(), index.PushOptions{Index: raw, Repo: *ref, Tag: *tag})
	if *format != "json" && len(findings) == 0 {
		fmt.Fprintln(stdout, digest)
		return 0
	}
	return finish(stdout, *format, findings, map[string]any{"digest": digest}, digest)
}

func runPromote(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("promote", stderr)
	ref := fs.String("ref", "", "oci://<registry>/<index repository>")
	digest := fs.String("digest", "", "the already-pushed, signed index digest (sha256:...)")
	tag := fs.String("tag", "latest", "the mutable tag to move")
	keyPath := fs.String("key", "", "PEM P-256 public key the digest must be signed by")
	format := fs.String("format", "text", "text | json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *ref == "" || *digest == "" || *keyPath == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	keyPEM, err := os.ReadFile(*keyPath)
	if err != nil {
		fmt.Fprintln(stderr, "expansion-index:", err)
		return 2
	}
	d, findings := index.Promote(context.Background(), index.PromoteOptions{Repo: *ref, Digest: *digest, Tag: *tag, KeyPEM: keyPEM})
	return finish(stdout, *format, findings, map[string]any{"digest": d, "tag": *tag}, fmt.Sprintf("ok: %s -> %s", *tag, d))
}

func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("verify", stderr)
	ref := fs.String("ref", "", "oci://<registry>/<repository>[:tag|@digest]")
	keyPath := fs.String("key", "", "PEM P-256 public key to trust")
	expect := fs.String("expect-digest", "", "the digest the index must resolve to")
	images := fs.Bool("images", false, "also verify each listed pack image's own signature")
	user := fs.String("username-env", "", "environment variable holding the registry username")
	pass := fs.String("password-env", "", "environment variable holding the registry password")
	format := fs.String("format", "text", "text | json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *ref == "" || *keyPath == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	keyPEM, err := os.ReadFile(*keyPath)
	if err != nil {
		fmt.Fprintln(stderr, "expansion-index:", err)
		return 2
	}
	auth, err := authFromEnv(*user, *pass)
	if err != nil {
		fmt.Fprintln(stderr, "expansion-index:", err)
		return 2
	}
	res, findings := index.Verify(context.Background(), index.VerifyOptions{
		Ref: *ref, KeyPEM: keyPEM, ExpectDigest: *expect, Images: *images, Remote: remoteAuth(auth),
	})
	doc := map[string]any{"images": []index.VerifiedImage{}}
	okText := ""
	if res != nil {
		doc["digest"], doc["generated_at"], doc["images"] = res.Digest, res.GeneratedAt, res.Images
		okText = fmt.Sprintf("ok: %s generated_at %s, %d image(s) listed", res.Digest, res.GeneratedAt, len(res.Images))
	}
	return finish(stdout, *format, findings, doc, okText)
}

// runListing verifies the published index and only then writes the Marquee
// page data. Nothing is written when verification or any check fails.
func runListing(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("listing", stderr)
	indexRef := fs.String("index-ref", "", "oci://<registry>/<index repository>@sha256:<hex>: the published, signed index")
	keyPath := fs.String("key", "", "PEM P-256 public key the index must be signed by")
	catalog := fs.String("catalog", "catalog.yaml", "the reviewed catalog.yaml (paid listings come from its listing blocks)")
	feed := fs.String("feed", "", "the public feed (a key under feeds: in catalog.yaml) whose index --index-ref is")
	out := fs.String("out", "", "where to write marquee.json")
	format := fs.String("format", "text", "text | json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *indexRef == "" || *keyPath == "" || *feed == "" || *out == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	keyPEM, err := os.ReadFile(*keyPath)
	if err != nil {
		fmt.Fprintln(stderr, "expansion-index:", err)
		return 2
	}
	cat, findings, err := catalogfile.Load(*catalog)
	if err != nil {
		fmt.Fprintln(stderr, "expansion-index:", err)
		return 2
	}
	if len(findings) > 0 {
		return finish(stdout, *format, findings, map[string]any{}, "")
	}
	doc, findings := listing.Build(context.Background(), listing.Options{IndexRef: *indexRef, KeyPEM: keyPEM, Catalog: cat, Feed: *feed})
	if len(findings) > 0 {
		return finish(stdout, *format, findings, map[string]any{}, "")
	}
	raw, err := listing.Marshal(doc)
	if err != nil {
		fmt.Fprintln(stderr, "expansion-index:", err)
		return 2
	}
	if err := os.WriteFile(*out, raw, 0o644); err != nil {
		fmt.Fprintln(stderr, "expansion-index:", err)
		return 2
	}
	res := map[string]any{"index_digest": doc.IndexDigest, "packs": len(doc.Packs), "paid_listings": len(doc.PaidListings), "out": *out}
	return finish(stdout, *format, nil, res, fmt.Sprintf("ok: wrote %s (%d pack(s), %d paid listing(s)) from the index %s", *out, len(doc.Packs), len(doc.PaidListings), doc.IndexDigest))
}
