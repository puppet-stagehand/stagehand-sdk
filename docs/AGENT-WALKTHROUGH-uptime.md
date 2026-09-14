# Walkthrough: authoring a pack with a code assistant (preview 0.0.1)

Prompt given to Claude Code, opened in this repository with no other context:

> Build a capability pack called "uptime" that shows each node's uptime on
> a node-detail tab. Core tier, Apache-2.0, published by "Example Org".

Expected assistant behaviour:
1. Reads CLAUDE.md, then `schema/json/manifest.schema.json` and `examples/hello/manifest.json`.
2. Writes `packs/uptime/manifest.json` with `id: uptime`, `slots: ["nodeDetailTab"]`,
   `permissions: ["documents:rw", "puppetdb:read"]` (facts carry `system_uptime`),
   a placeholder `ui_digest`, and a `refresh` job.
3. Runs `go run ./cmd/pack-check --format json packs/uptime/manifest.json`.
4. Fixes findings from their `fix` lines until `ok: true`.
5. Drafts `docs/USER-GUIDE.md` (ELI10) and `docs/TESTER-GUIDE.md` (one step for the tab).
6. Asks the user only product questions (e.g. "should the tab show days or hours?").

What it cannot do yet: build the image, run the worker, or render the tab —
those need `pack-build`, the worker SDK and the console host (next phases).
