package catalogfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

const good = `format: 1
feeds:
  official:
    index: ghcr.io/org/expansion-index
    images: ghcr.io/org/packs
    visibility: public
  paid:
    index: harbor.example.com/paid/index
    images: harbor.example.com/paid/packs
    visibility: private
packs:
  - id: hello
    feed: official
    docs_url: https://example.com/hello
    versions:
      - version: 0.1.0
        candidate: ghcr.io/org/staging/hello@` + digest + `
        released_at: 2026-10-09
        notes_url: https://example.com/notes
  - id: observability
    feed: paid
    listing:
      name: Observability
      summary: Dashboards.
      tier: ent
    versions:
      - version: 1.0.0
        candidate: harbor.example.com/staging/observability@` + digest + `
        released_at: 2026-10-01
`

func codes(fs []Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Code)
	}
	return strings.Join(out, ",")
}

func TestDecodeGood(t *testing.T) {
	f, fs := Decode([]byte(good))
	if len(fs) != 0 {
		t.Fatalf("findings: %v", fs)
	}
	if f.Packs[0].Versions[0].ReleasedAt != "2026-10-09" {
		t.Fatalf("an unquoted YAML date must arrive as the date string, got %q", f.Packs[0].Versions[0].ReleasedAt)
	}
}

func TestLoadMissingFileIsIOError(t *testing.T) {
	if _, _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected an I/O error")
	}
	p := filepath.Join(t.TempDir(), "catalog.yaml")
	if err := os.WriteFile(p, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, fs, err := Load(p); err != nil || len(fs) != 0 {
		t.Fatalf("Load: %v %v", err, fs)
	}
}

func TestRules(t *testing.T) {
	cases := []struct {
		name string
		edit func(string) string
		want string // a finding code that must be present
	}{
		{"missing format", func(s string) string { return strings.Replace(s, "format: 1\n", "", 1) }, "catalog_format"},
		{"format 2", func(s string) string { return strings.Replace(s, "format: 1", "format: 2", 1) }, "catalog_format"},
		{"bad pack id", func(s string) string { return strings.Replace(s, "id: hello", "id: Hello-Pack", 1) }, "pack_id_invalid"},
		{"candidate by tag", func(s string) string {
			return strings.Replace(s, "ghcr.io/org/staging/hello@"+digest, "ghcr.io/org/staging/hello:1.0", 1)
		}, "candidate_not_pinned"},
		{"candidate uppercase digest", func(s string) string { return strings.Replace(s, "hello@sha256:00", "hello@sha256:AB", 1) }, "candidate_not_pinned"},
		{"released_at with time", func(s string) string {
			return strings.Replace(s, "released_at: 2026-10-09", `released_at: "2026-10-09T00:00:00Z"`, 1)
		}, "released_at_invalid"},
		{"released_at free text", func(s string) string {
			return strings.Replace(s, "released_at: 2026-10-09", "released_at: yesterday", 1)
		}, "released_at_invalid"},
		{"undeclared feed", func(s string) string { return strings.Replace(s, "feed: official", "feed: nowhere", 1) }, "pack_feed_undeclared"},
		{"duplicate (id, version)", func(s string) string {
			return strings.Replace(s, "        released_at: 2026-10-09\n", "        released_at: 2026-10-09\n      - version: 0.1.0\n        candidate: ghcr.io/org/staging/hello@"+digest+"\n        released_at: 2026-10-09\n", 1)
		}, "version_duplicate"},
		{"private feed without listing", func(s string) string {
			return strings.Replace(s, "    listing:\n      name: Observability\n      summary: Dashboards.\n      tier: ent\n", "", 1)
		}, "listing_missing"},
		{"private listing tier unknown", func(s string) string { return strings.Replace(s, "tier: ent", "tier: platinum", 1) }, "listing_tier_invalid"},
		{"unknown key", func(s string) string { return strings.Replace(s, "id: hello\n", "id: hello\n    name: Hello\n", 1) }, "catalog_unparseable"},
		{"unknown feed key", func(s string) string {
			return strings.Replace(s, "visibility: public", "visibility: public\n    colour: red", 1)
		}, "catalog_unparseable"},
		{"bad visibility", func(s string) string { return strings.Replace(s, "visibility: public", "visibility: secret", 1) }, "feed_visibility_invalid"},
		{"feed with a scheme", func(s string) string {
			return strings.Replace(s, "index: ghcr.io/org/expansion-index", "index: https://ghcr.io/org/expansion-index", 1)
		}, "feed_index_invalid"},
		{"pack with no versions", func(s string) string {
			i := strings.Index(s, "    versions:\n      - version: 0.1.0")
			j := strings.Index(s, "  - id: observability")
			return s[:i] + "    versions: []\n" + s[j:]
		}, "pack_no_versions"},
		{"javascript docs_url", func(s string) string {
			return strings.Replace(s, "https://example.com/hello", "javascript:alert(1)", 1)
		}, "docs_url_invalid"},
		{"empty file", func(string) string { return "" }, "catalog_empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, fs := Decode([]byte(c.edit(good)))
			if !strings.Contains(codes(fs), c.want) {
				t.Fatalf("findings %q lack %q", codes(fs), c.want)
			}
			for _, f := range fs {
				if f.Fix == "" || f.Message == "" {
					t.Fatalf("a finding must carry a message and a fix line: %+v", f)
				}
			}
		})
	}
}

func TestListingNotRequiredOnPublicFeed(t *testing.T) {
	if _, fs := Decode([]byte(good)); len(fs) != 0 {
		t.Fatalf("a public-feed pack needs no listing: %v", fs)
	}
}
