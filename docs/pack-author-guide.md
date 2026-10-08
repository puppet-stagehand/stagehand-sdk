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
  - `code:rw` — read and write environments, Puppetfile modules and Hiera
    data in the Code facet.
  - `code:import` — read an existing control repo from a git address and
    propose importing it. It is separate from `code:rw` because it reaches a
    git host outside the console, and you need both to import. `code:import`
    is only honoured together with `code:rw`: if you declare `code:import`
    without `code:rw`, pack-check refuses the manifest with the finding
    `code_import_requires_code_rw`. The fix is to add `"code:rw"`.
  - `forge:rw` — search the Puppet Forge for modules and look at what a
    module depends on.
  - `forge:recommend` — ask for ranked module suggestions from a plain
    sentence. It is separate from `forge:rw` because it sends your text to a
    third-party language model, which can cost money.
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
with a checklist a human can follow to confirm it works. Importing an existing
control repo from a git address is a sixth gated path: it needs `code:import`
as well as `code:rw`, and `docs/code-import.md` walks through it, including
what it cannot bring in.

## Suggesting Puppet modules
The Forge facet can turn a plain sentence ("I need to manage security
settings on my Windows servers") into ranked Puppet module suggestions.
Every suggestion comes from a real search of a real registry; a language
model you configure only proposes an order for the results and says why, and
the host moves deprecated modules to the end. It is
advice only and never edits a Puppetfile. It needs the `forge:recommend`
permission and a provider whose API key is kept with `secrets:rw`.
`docs/forge-recommend.md` walks through it from the manifest to the answer,
lists every limit and error, and ends with a checklist a human with their own
API key can follow.

## Authoring a control repo end to end
The Code facet, the registry search, the suggestion step and the ask-a-human
gate compose into one workflow: describe a need, check the suggested module,
write it with its Hiera data, get a person to approve the settings, adopt an
existing repository, and change a module that is already there, each time with
a person saying yes before anything is replaced. There is a worked, runnable
proof of the whole thing in `examples/control-repo-authoring`, with
`examples/control-repo-authoring/README.md` as the short tour.
`docs/control-repo-authoring.md` walks through it from the first blank
environment to the final read-back, and `docs/control-repo-authoring-testing.md`
is the checklist a human follows by hand. This milestone is author-only: it
does not deploy and does not write back to a real control repo.

## Names you cannot use for your own documents
Your pack can keep notes of its own in the Documents facet, in collections you
name yourself (for example `state` or `locks`). A few names are not yours,
because a facet already uses them for its own bookkeeping:

- anything that starts with `code-`, `deploy-`, `bolt-` or `inventory-`
- the exact names `forge-sources` and `llm-providers`

Think of them as labelled drawers in the console's filing cabinet: you may
look inside, but only the facet that owns a drawer may put things in or take
things out. If your pack tries to write or delete one, the write is refused
with `PERMISSION_DENIED`, an error detail code of `collection_reserved`, and a
fix line that says which facet owns it. Reading, listing and querying those
collections still works. Upper and lower case do not matter, so `CODE-notes`
is just as reserved as `code-notes`. The fix is to pick another name for your
own state. A name that only looks similar, such as `codex-notes`,
`deployments` or `bolts`, is fine.

Proposals are the one exception that looks like a clash: `approval.ProposeBody`
and `approval.Propose` still create pending proposals in `code-overwrites` and
`inventory-proposals`, because that is how asking a person works.

Tests and examples that run on `host.Local` sometimes need to put something in
one of those drawers, for example a language-model provider for the Forge
facet. They do it with `local.SeedDocument`, which stands in for the operator.
Real pack code never calls it. `docs/forge-recommend.md` shows it in use.

## Two documents you owe
- `docs/USER-GUIDE.md` — explain your pack to someone who has never used
  Puppet. Short sentences. A glossary.
- `docs/TESTER-GUIDE.md` — for a human tester with a running console: one
  numbered step per route and per slot, and what they should see.
