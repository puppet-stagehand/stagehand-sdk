# Inventory Onboarding — a worked proof, not a real pack

This is a proof example. It is not an Expansion Pack you could install, and
it does not listen on a network. What it shows is that the Inventory facet
(node identity, facts, groups) and the `approval` package (propose an
action, have a human approve it, only then act) genuinely fit together into
a real node-onboarding workflow, run against `host.Local` — the in-process
test double that stands in for the real console during development.

If you are writing a pack that needs "a human must say yes before this
happens," copy the shapes here into your own pack.

## The story it tells

Read `examples/inventory-onboarding/inventory_onboarding_test.go` for the
full run. In plain language, in order:

1. The pack notices a machine it has not onboarded yet (`Discover`).
2. It files the machine under a group (`AddToGroup`).
3. It records what it knows about the machine (`AttachFacts`).
4. It writes down that it would like to onboard the machine, and stops
   (`ProposeOnboarding`).
5. A person holding the right token says yes (`Approve`).
6. Only then does the machine actually join the inventory (`Onboard`).
7. Afterwards, it can be found by what was recorded about it
   (`FindByFact`).

The test's fixture machine is `web-07.example.test`, filed under the group
`production-web`. If you run the test with `-v`, that is the id and group
you will see in the output.

## The two halves, and why there are two

The example is split into two types, not one type with two methods:
`ProposerBackend` (built with `NewProposer`) and `ApproverBackend` (built
with `NewApprover`).

`ProposerBackend` holds every step that writes down what should happen:
`Discover`, `AddToGroup`, `AttachFacts`, `ProposeOnboarding`, `Onboard`, and
`FindByFact`. `ApproverBackend` holds only the two steps that decide what
actually happens: `Approve` and `Reject`.

The one sentence that explains the split: the code that writes down a
request must not be able to reach the code that approves it, or nobody was
actually asked. `ApproverBackend.Approve` takes the approval token's secret
as an argument, and nothing in this package can produce that secret — the
token is minted only inside the test file's `approverToken` helper, standing
in for an operator obtaining it out of band, away from any code the pack
itself runs.

`TestInventoryOnboarding_ProposerCannotSelfApprove` is the test that checks
this claim by reading this package's own source, not by trusting a code
review: it parses `inventory_onboarding.go` and fails if anything reachable
from `ProposerBackend` could ever call into approving or token-minting. If a
future change ever puts the two halves in one call graph, this test is what
catches it. See `docs/approval-pattern.md` for the rule this test enforces,
in full.

## What the manifest declares

`examples/inventory-onboarding/manifest.json` shows the two-vocabulary
split a pack author must get right:

- `inventory:rw` is a **permission** — it gates every Inventory call for as
  long as the pack is installed.
- `inventory:approve` is **not** a permission. It is a route's access
  scope — it gates one decision, and arrives as a short-lived token, not a
  standing grant.

If `inventory:approve` were written as a permission instead, a per-decision
capability would become a standing grant, and the gate would be open by
configuration rather than by code — exactly the mistake this split exists
to prevent.

## How to run it

The example's own tests:

```
go test ./examples/inventory-onboarding/... -race -v
```

You should see four tests pass:
`TestInventoryOnboarding_EndToEnd`,
`TestInventoryOnboarding_PendingProposalDoesNotOnboard`,
`TestInventoryOnboarding_RejectedProposalDoesNotOnboard`, and
`TestInventoryOnboarding_ProposerCannotSelfApprove`.

The manifest fixture:

```
go run ./cmd/pack-check --format json examples/inventory-onboarding/manifest.json
```

You should see `"ok": true`.

## What it does not do

- No network listener — this is a Go package, not a running worker.
- No OpenAPI fragment, because `pack-build` does not exist yet in Preview
  0.0.1; `manifest.json` still declares its three routes so the fixture is
  complete on its own terms.
- No real discovery source — `Discover` reads a bounded fixture set
  injected at construction. It is never a network scan.
- No `USER-GUIDE.md` or `TESTER-GUIDE.md` under `docs/`. Those are owed by
  real Expansion Packs; every behaviour this example has is covered by an
  automated test instead.
