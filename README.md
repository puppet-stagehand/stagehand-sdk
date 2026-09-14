# stagehand-sdk

The contract for building **capability packs** — installable apps for the
Stagehand console. **Preview 0.0.1** (2026-09-14): the machine-readable
contract (manifest and index JSON Schemas, facet and worker protobufs, slot
types), a `pack-check` validator with tests, an example pack, and the brief
a code assistant needs to author a manifest. No worker runtime, UI toolkit
or console host yet — see the roadmap.

```
go test ./...
go run ./cmd/pack-check examples/hello/manifest.json
go run ./cmd/pack-check --format json internal/manifest/testdata/everything-wrong.json
```

| Path | What |
|---|---|
| `CLAUDE.md` / `AGENTS.md` | Brief for code assistants |
| `llms.txt` | Reading order for context stuffing |
| `schema/json/manifest.schema.json` | Pack manifest (contract_version 1) |
| `schema/json/index.schema.json` | App Center index (a pack forge = OCI registry + this, signed) |
| `schema/json/settings-ui.schema.json` | Renderable subset of JSON Schema for pack settings |
| `schema/proto/stagehand/host/v1/host.proto` | Facets the worker calls; permission strings in comments |
| `schema/proto/stagehand/host/v1/worker.proto` | Services the worker implements; extension points |
| `schema/ts/slots.d.ts` | UI slot kinds and props |
| `cmd/pack-check` | Validator: stable finding codes, each with a `fix` line; `--format json` |
| `examples/hello` | A valid pack manifest (node-detail tab + job) |
| `docs/pack-author-guide.md` | ELI10 guide |
| `docs/AGENT-WALKTHROUGH-uptime.md` | What a good assistant run looks like today |

Design record: `puppet-console/docs/adr/0010-capability-packs.md`,
`docs/design/capability-packs.md`, `docs/design/stagehand-sdk-ai-friendly.md`.

## Roadmap (from the design)
S1 contract (this preview) → S2 worker SDK + `host.Local` + conformance →
S3 example pack as a runnable container → S4 `@stagehand/sdk-ui` + `pack-build`
+ `pack-index` → S5 v0.1.0 → S6 AI-friendly surface (`pack-build mcp`, skills,
templates, cookbook, docs linter).

Licence: Apache-2.0.
