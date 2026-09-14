# Pack author guide (ELI10)

## What you are making
The Stagehand console is a stage. A **capability pack** is a new act you
add to it: a few pages, a worker that does the act's work, and a card that
says what the act needs. The console reads the card, gives the worker only
the tools on the card, and puts the pages where the card says.

## The card: `manifest.json`
Copy `examples/hello/manifest.json` and change:
- `id` — short lowercase name, e.g. `uptime`.
- `name`, `summary`, `publisher`, `licence`.
- `permissions` — the tools your worker needs. Start small. Examples:
  - `documents:rw` — remember things (always allowed).
  - `puppetdb:read` — read facts and reports.
  - `bolt:run` — run a task on nodes through Bolt.
  - `secrets:rw` — keep a password safely.
- `slots` — where your pages go. `nodeDetailTab` puts a tab on every
  node's page; `page` gives you a whole page (then add `nav`).
- `settings_schema` — the knobs an operator can turn, as a small JSON Schema.

Run the checker. It tells you exactly what to fix and how:
```
go run ./cmd/pack-check examples/hello/manifest.json
```

## The worker (coming in the next SDK phase)
A small program in a container. It phones the console and asks for things
using the tools on the card. It answers the console's questions: "serve
this page request", "are you healthy", "run your hourly job now".
The exact messages are in `schema/proto/stagehand/host/v1/`.

## The pages (coming in the next SDK phase)
React components built from the console's own parts, so they look like
the rest of Stagehand. They receive `ctx.api` to call your worker's routes.

## Two documents you owe
- `docs/USER-GUIDE.md` — explain your pack to someone who has never used
  Puppet. Short sentences. A glossary.
- `docs/TESTER-GUIDE.md` — for a human tester with a running console: one
  numbered step per route and per slot, and what they should see.
