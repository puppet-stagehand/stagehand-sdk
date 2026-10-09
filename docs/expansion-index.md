# expansion-index: running a signed feed of Expansion Packs

`expansion-index` is the tool a feed operator uses to publish a list of
Expansion Packs that Stagehand consoles can trust. This guide is for anyone who
runs their own feed (a company with private packs, a community with shared
ones) and for the people who run Perforce's own feed. You do not need to know
Puppet, cosign or OCI registries beforehand.

## What it does, for a ten year old

Imagine a school that wants to hand out approved games.

- Each **pack** is a game in a sealed box (a container image).
- The **index** is the school's list on the notice board: "these games, these
  versions, and each box has this exact fingerprint". The fingerprint is a
  digest: if anyone changes a single byte in the box, the fingerprint changes.
- The **signature** is the head teacher's stamp on the list, made with a secret
  stamp (the private key). Every console owns a picture of the real stamp (the
  public key) and refuses any list that does not carry it.
- **Copying** moves a reviewed box from the staging cupboard to the public
  shelf without opening it, so the fingerprint stays the same.
- The **listing** is a poster for the school website made from the stamped
  list, so people can read what is available. Posters for paid games only say
  "exists, costs money"; they never say where the box is kept.

`expansion-index` does everything except the stamping itself (that is the
`cosign` program, run by your publishing job). It never holds the private key.

## The commands

| Command | What it does |
|---------|--------------|
| `build` | Reads `catalog.yaml` and every candidate image, checks them, writes `index.json`. Writes nothing if anything is wrong. |
| `copy` | Copies a reviewed candidate image into the feed's image repository by digest. |
| `push` | Publishes `index.json` as an artifact under an immutable tag. |
| `promote` | Moves the `latest` tag to an index digest, but only after checking its signature. |
| `verify` | Checks an index's signature the way a console does; with `--images` also each pack image's signature. |
| `listing` | Verifies the published index again, then writes the Marquee page data (`marquee.json`). |

Every command exits `0` (ok), `1` (it found problems and printed each with a
`fix` line) or `2` (bad usage or a missing file). `--format json` prints a
machine-readable result. Registry credentials are only ever read from
environment variables whose **names** you pass (`--previous-username-env
MY_USER`); the values are never printed and are never taken from flags.

## catalog.yaml

`catalog.yaml` is the reviewed list of what is published. It is the only thing
people edit. It deliberately does **not** repeat a pack's name, publisher,
tier, licence, summary or permissions: `build` reads those from the
`stagehand/manifest.json` inside each candidate image, so the list can never
disagree with the box.

```yaml
format: 1
feeds:
  official:                                  # the name you pass as --feed
    index: ghcr.io/example-org/catalog       # where the signed index lives
    images: ghcr.io/example-org/packs        # packs are published at <images>/<pack id>
    visibility: public                       # public or private
  paid:
    index: harbor.example.com/paid/catalog
    images: harbor.example.com/paid/packs
    visibility: private
packs:
  - id: hello_world                          # 2 to 32 chars: a-z, 0-9, _
    feed: official
    docs_url: https://example.com/hello      # optional, https only
    versions:
      - version: 1.2.0
        candidate: registry.example.com/staging/hello_world@sha256:<64 hex>
        released_at: 2026-10-09              # YYYY-MM-DD
        notes_url: https://example.com/hello/1.2.0   # optional
  - id: big_addon
    feed: paid
    listing:                                 # required for packs on a private feed
      name: Big Add-on
      summary: What it does, in one sentence.
      tier: ent                              # core, ent or adv
    versions:
      - version: 2.1.0
        candidate: registry.example.com/staging/big_addon@sha256:<64 hex>
        released_at: 2026-09-15
```

Rules `build --check` enforces without touching any registry: `format: 1`; at
least one feed; every pack on a declared feed; a candidate is always pinned by
digest (a tag is never accepted, because a tag can move after review); a pack
and a (pack, version) appear once; dates and URLs are well formed; unknown keys
are an error. Run `expansion-index build --check --catalog catalog.yaml --feed
official` in your pull-request job.

## The publish order

Order matters because each step relies on the one before it. Your publishing
job does these in sequence and stops at the first failure.

1. **Build and validate every candidate**, reading the previous index with the
   credentials that will push the new one:

   ```bash
   expansion-index build --catalog catalog.yaml --feed official \
     --previous oci://ghcr.io/example-org/catalog \
     --previous-username-env REGISTRY_USER --previous-password-env REGISTRY_TOKEN \
     --candidate-username-env STAGING_USER --candidate-password-env STAGING_TOKEN \
     --key cosign.pub --out index.json --format json
   ```

   The JSON lists each image with `already_listed`. The first time ever a feed
   is published, add `--allow-missing-previous` (see below).
2. **Copy each image whose `already_listed` is false**, by digest:

   ```bash
   expansion-index copy \
     --from registry.example.com/staging/hello_world@sha256:<hex> \
     --to ghcr.io/example-org/packs/hello_world:1.2.0 \
     --from-username-env STAGING_USER --from-password-env STAGING_TOKEN \
     --to-username-env REGISTRY_USER --to-password-env REGISTRY_TOKEN
   ```

   Source and destination take separate credentials, so both may be `ghcr.io`.
   The source is read by digest; the destination is checked and written with
   push permission; if the destination already holds that digest nothing is
   written (`already_present: true`); afterwards the destination digest must
   equal the source digest or the command exits `1`. A `401` or `403` on either
   side is always fatal.
3. **Sign each copied image's digest** with cosign (flags below).
4. **Push the index** under an immutable tag: `expansion-index push --index
   index.json --ref oci://ghcr.io/example-org/catalog` (the tag is derived
   from `generated_at`; `build` printed it).
5. **Sign the index digest** that `push` printed, the same way.
6. **Verify anonymously**, from a machine with no credentials (an empty
   `DOCKER_CONFIG`), exactly as a console would: `expansion-index verify --ref
   oci://ghcr.io/example-org/catalog@sha256:<index digest> --key cosign.pub
   --expect-digest sha256:<index digest> --images`.
7. **Promote `latest`**: `expansion-index promote --ref
   oci://ghcr.io/example-org/catalog --digest sha256:<index digest> --key
   cosign.pub`. Consoles read `latest`, so this is the moment the world sees it.
8. **Write the listing** (next section).

Run `copy` before any authenticated read of a pack repository: a package that
has never been created cannot be read with pull-only access on GHCR, so `verify
--images` needs the copy to have created it first.

### Signing with cosign

Signing is not part of `expansion-index`; your job runs the `cosign` program,
pinned to **v3.1.3**. Consoles check the older "legacy" signature format (a
`sha256-<hex>.sig` tag next to the image), so these exact flags are required:

```bash
cosign sign --key env://COSIGN_PRIVATE_KEY \
  --new-bundle-format=false --use-signing-config=false \
  --tlog-upload=false --registry-referrers-mode=legacy --yes \
  ghcr.io/example-org/packs/hello_world@sha256:<digest>
```

`COSIGN_PRIVATE_KEY` and `COSIGN_PASSWORD` come from your CI's protected
secrets. Pin cosign; a newer one that drops the legacy flags would produce
signatures consoles refuse, and the `verify` step in the job is what catches
that before `latest` moves. Keep the private key out of the repository.

## Why tags look like 20261009T143012Z, and why generated_at must go up

Each index records `generated_at`. Consoles remember the newest they have seen
and refuse anything older, so that nobody can replay an old list to bring back a
pack you removed. Two rules follow:

- `generated_at` must **strictly** increase on every publish. `build` reads the
  published index and refuses to emit one that is not later (finding
  `generated_at_not_increasing`). Wait a second, or fix the clock.
- The immutable tag is the same moment written as `YYYYMMDDTHHMMSSZ`, because a
  colon is not allowed in an image tag. `push` refuses to overwrite an existing
  tag, so a published list can never change under its name.

Run publishing jobs one at a time (a concurrency group that does not cancel a
running job).

## How build reads the previous index (three outcomes)

`build --previous` reads the currently published index so it can enforce the
rule above and mark `already_listed`. It has exactly three outcomes:

| Outcome | Meaning | What happens |
|---------|---------|--------------|
| found | The index was read. | Build continues; `previous` is `found` in the JSON. |
| not found | The registry says the repository or tag does not exist. | Tolerated **only** with `--allow-missing-previous` (the first publish); `previous` is `not_found`. |
| denied | The registry answered `401` or `403`. | Always fatal (`previous_denied`). No flag relaxes it. |

Why: on GHCR (checked 2026-10-09), a package that does not exist answers an
anonymous read exactly like a private one, with `403 DENIED`. So an anonymous
read cannot tell "never published" from "private", and treating a denial as
"nothing there" would let a failed read reset the rollback guard. The fix is to
read the previous index **with the credentials that will push it**
(`--previous-username-env/--previous-password-env`): a registry that will accept
the push grants that scope even for a package nobody has created yet, and then
answers "not found" honestly. `push` and `copy` check existence the same way.

## First publish on GHCR

GHCR creates packages **private**, and a package can only be made public from
the web page. So the first run of a new feed is expected to stop part way:

1. Run the job with `--allow-missing-previous`. It creates the index package and
   each pack package (private), signs them, and stops at the anonymous verify,
   because an anonymous pull of a private package is refused. `latest` is not
   moved. The build step shows `previous: not_found`.
2. For the index package and each pack package: package settings, change
   visibility to **public** (this cannot be undone). Under **Manage Actions
   access**, give the publishing repository **Write**, or later runs will be
   refused on packages that are not linked to it.
3. Re-run the job. The previous read still says not found (there is no `latest`
   yet); `copy` copies nothing new (each digest is already present); signing the
   same digest again adds a second signature layer by the same key, which
   consoles accept; verify and promote now succeed.
4. From the first promoted index on, `build` finds the previous index, and
   images already listed are not copied or signed again.

**Every new pack id repeats steps 1 to 3 once**, because a new pack is a new
package.

## The listing for the Marquee page

`expansion-index listing` produces the data for a public catalog web page:

```bash
expansion-index listing \
  --index-ref oci://ghcr.io/example-org/catalog@sha256:<index digest> \
  --key cosign.pub --catalog catalog.yaml --feed official --out marquee.json
```

It first verifies the index signature again, and writes nothing if that fails.
Then:

- **Free packs** are copied from the verified index (name, summary, tier,
  licence, versions with release date, image and digest). Nothing about them is
  taken from `catalog.yaml`, so the page shows exactly what was signed.
- **Paid listings** come only from the `listing` blocks in `catalog.yaml` of
  packs on **private** feeds. Each carries a name, summary, tier, release dates
  and the fixed label "Perforce add-on, licence required"; never an image, a
  digest or a registry host. The private feed is never contacted: the only
  registry `listing` connects to is the one in `--index-ref`.
- `--feed` must name a public feed, and `--index-ref` must be that feed's own
  index repository.
- The publisher key shown on the page is re-encoded from the parsed key. The
  `--key` file must hold exactly one `PUBLIC KEY` block and nothing else; a
  file with anything more is refused (`key_invalid`), so a stray private key or
  note can never reach the page.

So a pack on a private feed reaches the public page **only** through its
`listing` block. If you want it advertised, write that block; if you do not,
leave the feed private and it is invisible.

## Taking a bad version down

To stop offering a version, remove it from `catalog.yaml`, review and merge,
and publish again. `build` writes a new index that lacks that version (and
nothing else changes), signs it and promotes it. This stops **new** installs
of it. It does not uninstall it from consoles that already have it, and the
image itself stays in the registry; delete the image separately if it must not
be pullable. There is no way to edit a published index: each publish is a new
immutable tag.

## Troubleshooting

| You see | It means | Do this |
|---------|----------|---------|
| `previous_denied` | The credentials used to read the previous index cannot read it. | Pass `--previous-username-env/--previous-password-env` with the credentials that push; check the token may write packages. |
| `previous_missing` | No index published yet. | If it really is the first publish, add `--allow-missing-previous`. |
| `generated_at_not_increasing` | The clock is not later than the published index. | Wait a second; fix the runner clock or `--now`. |
| `tag_exists` | That immutable tag was already pushed. | Rebuild with a later `generated_at`. |
| `index_untrusted_signer` | The signature is not by the key you passed. | Verify with the matching public key, or re-sign with the right private key. |
| `index_unsigned` | No signature exists for that digest. | Run the cosign step for the digest `push` printed. |
| `destination_denied` | The destination registry refused the push. | Use credentials that can create and push the repository; on GHCR check Manage Actions access. |
| `digest_mismatch` | The destination holds different content than the source. | Do not sign or list it; copy to a registry that preserves manifests. |

## Where the paid feed fits

A private feed (a credentialed registry such as Harbor) uses exactly these same
commands; it is just another `feeds:` entry with `visibility: private`. Nothing
in `copy`, `build` or `listing` is specific to one registry vendor or to
Perforce's own feed, so adding a paid feed is a `catalog.yaml` change, not a
code change.
