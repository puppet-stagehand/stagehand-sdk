# stagehand-sdk

The contract for building **Expansion Packs** — installable apps for the
Stagehand console. **Tracer slice toward v0.1.0-rc.1** (2026-09-14): the machine-readable
contract (manifest and index JSON Schemas, facet and worker protobufs, slot
types), a `pack-check` validator with tests, an example pack, and the brief
a code assistant needs to author a manifest. No worker runtime, UI toolkit
or console host yet — see the roadmap.

```
go test ./...
go run ./cmd/pack-check examples/hello/manifest.json
go run ./cmd/pack-check --format json manifest/testdata/everything-wrong.json
```

| Path | What |
|---|---|
| `CLAUDE.md` / `AGENTS.md` | Brief for code assistants |
| `llms.txt` | Reading order for context stuffing |
| `manifest/schema.json` | Pack manifest (contract_version 1) |
| `schema/json/index.schema.json` | App Center index (a pack forge = OCI registry + this, signed) |
| `schema/json/settings-ui.schema.json` | Renderable subset of JSON Schema for pack settings |
| `schema/proto/stagehand/host/v1/host.proto` | Facets the worker calls; permission strings in comments |
| `schema/proto/stagehand/host/v1/worker.proto` | Services the worker implements; extension points |
| `schema/ts/slots.d.ts` | UI slot kinds and props |
| `cmd/pack-check` | Validator: stable finding codes, each with a `fix` line; `--format json` |
| `examples/hello` | A valid pack manifest (node-detail tab + job) |
| `examples/control-repo-authoring` | Worked proof that the Code facet, the registry search, the suggestion step and the approval gate compose into one workflow; author-only, never deploys |
| `docs/control-repo-authoring.md` | End-to-end ELI10 authoring guide for that example |
| `docs/pack-author-guide.md` | ELI10 guide |
| `docs/AGENT-WALKTHROUGH-uptime.md` | What a good assistant run looks like today |

## Changing the contract (maintainers)

`schema/proto/` is a generated, verbatim copy of `proto/`. Never edit it by
hand.

1. Edit `proto/` only.
2. Run `buf lint && buf generate` (refreshes `gen/`).
3. Run `go generate ./schema` (copies `proto/` to `schema/proto/`).

CI fails on `gen/` drift and on `schema/proto/` drift
(`TestSchemaProtoMatchesProto` in `go test ./... -race`). Pack authors keep
reading `schema/proto/`.

Design record: `puppet-console/docs/adr/0010-capability-packs.md`,
`docs/design/capability-packs.md`, `docs/design/stagehand-sdk-ai-friendly.md`.

## Roadmap
This repo now tracks the approved v0.1.0-rc.1 tracer slice design
(`puppet-console/docs/superpowers/specs/2026-09-14-stagehand-sdk-v0.1-design.md`):
four facets (Documents, Settings, Secrets, Auth), `host.Local`, and
`examples/opentofu-lite` as a facet-sufficiency proof. The full 14-facet
contract this slice trims from is documented in
`puppet-console/docs/design/capability-packs.md` §3.2 and remains in this
repo's git history (commit `b10af30`) — later slices grow back toward it
one real consumer at a time, per that design doc's own deferral list.

**Status:** v0.1.0-rc.1 tracer slice implemented (Documents, Settings,
Secrets, Auth; host.Local; examples/opentofu-lite) per
docs/superpowers/plans/2026-09-14-stagehand-sdk-v0.1-tracer-slice.md in
puppet-console. Not yet tagged.

Licence: Apache-2.0.
