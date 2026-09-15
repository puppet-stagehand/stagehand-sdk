# The `approval` package

`approval` is a small, reusable propose/approve/reject governance pattern.
It composes only two of `*host.Host`'s existing fields — `Documents` and
`Auth` — and nothing else, so any facet that needs "a human must say yes
before this takes effect" can use it unchanged. Inventory's node onboarding
is the first consumer, not a special case baked into the package.

## What this package guarantees

- A proposal is created once, through `approval.Propose`. A second
  `Propose` call for the same node id is refused; it cannot overwrite the
  first.
- A decision — `approval.Approve` or `approval.Reject` — requires a token
  the Auth facet issued for the approval scope, and nothing else will do.
  A token minted for any other scope, or no token at all, is refused
  before the package ever looks at the proposal.
- Every decision records who made it and when.
- A rejection records why.
- When several callers try to decide the same proposal at once, exactly
  one of them does. The rest are told so in a form they can branch on,
  not left to guess from a raw conflict code.

`approval.Get` reads a proposal's current state — status, reason,
decider, decision time — for any caller; reading is not itself a governed
action.

## The hard constraint

**The token that authorizes a decision must be obtained and held by the
person deciding, and the code path that creates proposals must not be
able to reach the code that obtains it.**

Here is why, in full, because a rule without its reason attached gets
optimized away the first time someone notices it looks avoidable. The
Auth facet will mint a token for any scope a caller asks for, gated only
on the pack holding the `tokens:issue` permission — it has no concept of
"only a human may request this scope." A pack whose proposing code can
also obtain an approval-scoped token has defeated the gate entirely. Not
by an attacker bypassing a check: by its own intended code calling an RPC
it is already allowed to call, with every check still passing. The
"human approval" premise would be gone, and nothing in this SDK's wire
contract would have noticed.

This package refuses to make that convenient, and it cannot do more than
refuse to make it convenient. `Propose` and `Approve`/`Reject` are
separate entry points. Nothing in this package obtains a token on a
caller's behalf. There is no combined "propose and decide" helper.

What a pack author must do, on top of what the package already does:

- Obtain the approval token in a separate entry point — a distinct
  operator persona, a separate handler, a separate process — that the
  request-handling path for proposals cannot call into.
- Treat any refactor that puts proposing and deciding in one call graph
  as a security change, not a convenience cleanup, and route it through
  the same scrutiny a permission change would get.

Name the temptation for what it is: the approve step looks like it could
be one call shorter if the pack just minted the token itself, right there,
and skipped waiting on an operator. Shortening it that way is the
failure this constraint exists to prevent, not an optimization worth
taking.

## The state machine

A proposal has exactly three statuses: `pending`, `approved`, `rejected`.
`Propose` creates a proposal at `pending`. `Approve` and `Reject` are the
only two edges out of `pending`, and both are terminal — there is no
`rejected -> pending` edge and no `approved -> rejected` edge. To retry a
denied onboarding, the proposer creates a new proposal with a new node
id; the old one stays exactly as it was decided.

A pending proposal waits indefinitely. There is no expiry, no sweep, and
no fourth "expired" status. Nothing in this SDK has a scheduling
mechanism today, and inventing one here — for a package whose entire job
is composing two existing facets — would be new, unproven machinery
introduced to solve a problem nobody has asked this package to solve yet.

## Using it

A real pack calls these in the order a real proposal actually moves
through them: propose, then — from the separate approver path — decide,
then act.

```go
// Proposing path (the pack's normal request handling).
node := &hostv1.Node{Id: "web-01", DisplayName: "web-01", Environment: "production"}
proposal, err := approval.Propose(ctx, h, node)

// Deciding path — structurally separate from the code above. The
// operator obtained secret out of band; nothing in the proposing path
// can reach it.
decided, err := approval.Approve(ctx, h, approval.ApproveRequest{
	ProposalID:  node.Id,
	TokenSecret: secret,
})
if err != nil {
	if approval.IsAlreadyDecided(err) {
		// someone else already decided this proposal
	}
	return err
}

// Only now, after decide returned success, perform the governed side
// effect.
_, err = h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: node.Id})
```

State the sequencing rule as a rule, not a suggestion: **perform the
governed side effect only after the decide call returns success, never
off the read that preceded it.** Two callers can both read a pending
proposal before either one commits its decision — that read tells you
nothing about who is about to win. A side effect placed before the write
happens twice, once per reader, while only one write ever survives. This
package deliberately performs no side effect of its own; that is what
keeps the ordering in the caller's hands, and visible in the caller's own
code, rather than hidden inside a helper the caller has to trust.

Errors worth branching on: `approval.ErrAlreadyProposed` (a second
`Propose` for a node id already in flight), `approval.IsAlreadyDecided`
(the predicate — use it, don't compare `codes.FailedPrecondition`
directly, since a malformed proposal can also produce that code), and
`approval.ErrReasonRequired` (`Reject` called with an empty reason).

Two manifest notes: the pack's manifest needs the `tokens:issue`
permission for the decide calls to be allowed at all, and the approval
scope belongs on a route's `access.scope` — it must never appear in the
pack's `permissions` list.

## What this pattern does not do

- **No field-level enforcement on the proposals collection.** The
  Documents facet is a generic namespaced JSON document store; it has no
  concept of collections, field names, or state machines. Any code
  holding the host can write a proposal document directly, with no
  token presented at all. This package's integrity guarantee is that it
  is the only sanctioned writer past creation — a discipline, not an
  enforcement — and a real durable host would need to defend this
  boundary differently.
- **No transaction spanning the decision and the act.** The decision is
  recorded in the Documents facet; the governed action is carried out by
  the caller, in a different facet entirely. A caller that dies between
  the two leaves a decided proposal with nothing done about it. A
  durable host must answer that gap with a real cross-facet transaction
  or a reconciliation pass — this pattern does not assume the gap is
  already closed.
- **The recorded decider is a token label, not a verified identity.**
  `decided_by` is whatever label the token was issued with.
  `host.Local`'s Auth facet has no requester-identity concept beyond
  that label; do not read `decided_by` as a stronger guarantee than it
  is.
- **Fact values must be byte-safe.** Fact values travel through a JSON
  structure (`structpb.Struct`), which cannot hold invalid UTF-8. Any
  value carrying raw bytes must be base64-encoded before it is proposed,
  and decoded symmetrically on read.

## Reusing it for another governed action

This package depends on nothing but the host's `Documents` and `Auth`
fields, which is exactly what lets a second governed action reuse it
unchanged rather than forking it. A second action needs its own
Documents collection (a distinct name from `inventory-proposals`) and its
own approval scope.

Today the required scope is one fixed, code-defined constant. That is
honest about the current state, not a design goal: the first genuine
second consumer is what turns it into a real kind-to-scope mapping, and
making that change means auditing every existing caller of this package
to confirm none of them was relying on the single fixed value.
