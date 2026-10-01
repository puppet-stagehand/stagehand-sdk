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

## Asking a human first
Some actions should not happen just because your pack decided they should
— adding a new node to the fleet is one of them. You write down what you
want to do, a person holding the right token says yes or no, and only
then does it happen. Your pack's own code must never be able to hand
itself that token — if it could, nobody was actually asked. See
`docs/approval-pattern.md` for the rule in full, and why it has to hold.
There is a worked example in the box: `examples/inventory-onboarding`, with
`examples/inventory-onboarding/README.md` as the short tour. Copying its
two-halves shape is the easiest way to get this right.

## Replacing code that is already there
The Code facet (`code:rw`) lets your pack write environments, Puppetfile
modules, Hiera levels and Hiera data keys. Adding new content is free.
Replacing content that already exists is refused until a person has
approved a proposal for exactly that change, and there is no setting that
turns this off. Note that the approval scope, `code:approve`, is not a
permission: it goes on a route's `access.scope` and never in `permissions`.
`docs/code-overwrite-gating.md` walks through the whole loop (propose,
approve, apply) for all five kinds of write, explains each refusal, and ends
with a checklist a human can follow to confirm it works.

## Two documents you owe
- `docs/USER-GUIDE.md` — explain your pack to someone who has never used
  Puppet. Short sentences. A glossary.
- `docs/TESTER-GUIDE.md` — for a human tester with a running console: one
  numbered step per route and per slot, and what they should see.
