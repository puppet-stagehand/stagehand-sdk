# Replacing existing code content: the overwrite gate (ELI10)

This guide is for a pack author who is new to Puppet and new to this SDK.
It explains one rule of the Code facet and how to work with it:

> Adding new content is free. Replacing content that is already there needs
> a second person to say yes first.

The rule is not a warning and not a setting. The SDK refuses the write. There
is no flag that turns the gate off, and no version of the Code facet that
skips it. If a write is refused, the fix is to follow the loop below, not to
look for a way around it. The gate guards against accidental in-place
replacement through a Put; section 6 says plainly what it does not stop.

## Some words first

- **Control repo** - the one repository that holds all of a site's Puppet
  code. The Code facet stores a working copy of it as structured data.
- **Environment** - one named copy of the code, such as `production` or
  `dev`. Nodes are told which environment to use.
- **Puppetfile** - a list of the modules an environment needs, with the
  version of each.
- **Hiera** - Puppet's lookup system for settings that differ per node or per
  group. A **hierarchy** is an ordered list of **levels**; each level says
  which data file to look in. A **data key** is one named value in a data file.
- **`environment.conf`** - a small settings file each environment can have.
  The Code facet calls its contents the environment **settings**.
- **Proposal** - a written request that says exactly what you want to
  replace and what you want to replace it with.
- **Approval scope** - a label on a short-lived token. The scope
  `code:approve` says the holder may say yes or no to overwrite proposals.

## 1. What is gated and what is not

The governing decisions are named so you can find them in the phase notes:
D-01 (deletes stay ungated), D-02 (gate a write per item, only when it
replaces something), D-03 (settings are gated on every call).

There are six gated paths. Five are the content writes in the table below. The
sixth is **import**: adopting an existing control repo, where the whole import
is one proposal that covers every selected branch. It has its own three RPCs
(`InspectImport`, `ProposeImport`, `ApplyImport`) and its own loop, with a
report step at the front, so it is described in
[`docs/code-import.md`](code-import.md) rather than here.

| Write RPC | When it is refused | Apply RPC that finishes it | Decision |
|---|---|---|---|
| `CreateEnvironment` | never | none | new content |
| `RenameEnvironment` | never (a name clash still gives a plain already-exists error) | none | not one of the six gated paths |
| `DeleteEnvironment` | never | none | D-01 |
| `DuplicateEnvironment` | only when the target name already exists | `ApplyEnvironmentDuplicate` | D-02 |
| `PutEnvironmentSettings` | on every call once the environment exists, including the first write | `ApplyEnvironmentSettings` | D-03 |
| `PutPuppetfileModule` | only when the same module is already listed (see "What counts as the same module") | `ApplyPuppetfileModuleOverwrite` | D-02 |
| `RemovePuppetfileModule` | never (it finds the module by either spelling) | none | D-01 |
| `SetModuledir` | never | none | not one of the six gated paths |
| `PutHieraLevel` | only when a level of that name already exists | `ApplyHieraLevelOverwrite` | D-02 |
| `RemoveHieraLevel`, `ReorderHieraLevels` | never | none | D-01 / not gated |
| `PutHieraDataKey` | only when that key already exists in that file | `ApplyHieraDataKeyOverwrite` | D-02 |
| `RemoveHieraDataKey`, `DeleteHieraDataFile` | never | none | D-01 |

Two details worth knowing:

- Settings are gated on every call because `environment.conf` has no single
  "new" item to add; a settings write always replaces the record.
- A write that would put back exactly what is already stored is still an
  overwrite as far as the gate is concerned. The gate looks at whether
  something exists, not at whether the new text differs.

## What counts as the same module

The gate asks "does this module already exist?" before it decides whether a
Puppetfile write is new content or a replacement. That question has a precise
answer, and it is the same one the Forge facet uses when it marks a module
`already_in_puppetfile`.

- **Every spelling of one Forge module is one module.** `puppetlabs/stdlib`,
  `puppetlabs-stdlib` and `PuppetLabs/stdlib` all name the same module. The
  Forge writes it with a slash and a Puppetfile often writes it with a hyphen;
  the owner's capital letters do not matter. The SDK folds them all to the
  lowercase hyphen form, `puppetlabs-stdlib`, using `code.CanonicalModuleName`.
  That one function is the only place the rule lives.
- **A Git module's name is never folded.** A module you pull from a git
  repository has a plain name such as `my-module`, and the hyphen is simply part
  of the name. Folding it would change an author's own identifier, so it is
  compared exactly as written. A Git module and a Forge module also never match
  each other, even if their names look alike after folding.
- **A Forge module is stored under the lowercase hyphen form.** When
  `PutPuppetfileModule` or `ApplyPuppetfileModuleOverwrite` writes a Forge
  module, the stored Puppetfile (and the module those calls return) uses
  `puppetlabs-stdlib`, whichever spelling you sent. Order never changes: an
  applied overwrite replaces the module at its original place in the file.
- **An imported Puppetfile keeps the names it came with.** Import does not
  rewrite module names, so an imported file may hold the slash spelling. The gate
  still compares by identity, so a later write of either spelling against an
  imported module is refused exactly as it would be for any other existing one.
- **You can remove a module by either spelling.** `RemovePuppetfileModule`
  removes a Forge module whichever way you name it. A Git module can be removed
  only by its exact name. If a Puppetfile holds a Forge module and a Git module
  that both fold to the same name, the first one in the file is removed.

The proposal records the folded name as its target, so a proposal written for the
slash spelling unlocks the hyphen spelling of the same module and the other way
round. The proposal still keeps the module exactly as the approver saw it; only
the target name is folded.

Two honest limits:

- **Re-propose old proposals.** An overwrite proposal created before this rule
  recorded the module name as it was typed. It will no longer match a write, so
  it must be re-proposed and approved again. This is the accepted cost of
  changing the form the target is recorded in, and no shim rewrites old
  proposals, because a shim that rewrote stored approvals would itself be a
  write on the approval path.
- **A Forge module and a Git module can share one target name.** If both fold to
  the same text, an approval for one gives the "approved, call the Apply RPC"
  refusal when you try the other. Nothing is applied without its own approval:
  the apply step still picks the module by its source, so the wrong one is never
  written. The only effect is a less precise refusal message.

## 2. Why

Replacing something someone else wrote is a different act from adding
something new. It can quietly change what every node in an environment
receives. So it needs a second person's yes, and the SDK enforces that by
refusing the write, not by printing a warning.

The rule for keeping proposing and approving apart (the person or code that
asks must not be able to approve for itself) is the same rule every approval
in this SDK follows. It is written out in full in
[`docs/approval-pattern.md`](approval-pattern.md); read that first if you
have not, and do not rely on this page for it.

## 3. The loop, step by step

1. **Build the proposal body.** Use the builder that matches the thing you
   want to replace. Each one freezes exactly what you want written:
   - `code.OverwriteBodyForPuppetfileModule(env, module)`
   - `code.OverwriteBodyForSettings(settings)`
   - `code.OverwriteBodyForEnvironmentDuplicate(sourceName, targetName)`
   - `code.OverwriteBodyForHieraLevel(env, level, index, insert)`
   - `code.OverwriteBodyForHieraDataKey(env, path, key, value)`
2. **Create the proposal.** Pass the body to `approval.ProposeBody` with a
   kind built from the two constants the `code` package defines:

   ```go
   kind := approval.Kind{
       Collection:   code.OverwriteCollection,   // "code-overwrites"
       ApproveScope: code.OverwriteApproveScope, // "code:approve"
   }
   _, err = approval.ProposeBody(ctx, h, kind, "my-proposal-id", body)
   ```

   Pick a proposal id you can remember; you need it again in step 4. The
   kind must be written in your code, never built from a request or from a
   proposal body.
3. **Get it approved.** Someone holding a token issued for the `code:approve`
   scope calls `approval.Approve` (or `approval.Reject`). That must happen in
   code your proposing path cannot reach.
4. **Apply it.** Call the matching Apply RPC with only the proposal id:

   | You wanted to replace | Apply RPC |
   |---|---|
   | a Puppetfile module | `ApplyPuppetfileModuleOverwrite` |
   | environment settings | `ApplyEnvironmentSettings` |
   | an environment, by copying another over it | `ApplyEnvironmentDuplicate` |
   | a Hiera level | `ApplyHieraLevelOverwrite` |
   | a Hiera data key | `ApplyHieraDataKeyOverwrite` |

   The Apply RPC takes no content. It writes what was frozen into the
   proposal, so an approved proposal can never be used to write something
   the approver did not see. Each one is matched to its own kind of proposal:
   giving it a proposal for a different kind of thing is refused.

A proposal covers one exact target: the environment, the kind of thing, its
name, its data file path and its source. An approval for a key in
`common.yaml` does not cover the same key in another file. An approval to copy
`production` over `staging` does not cover copying `dev` over `staging`.

## 4. Reading the refusal

A refused write returns the gRPC code `FailedPrecondition` and an
`ErrorDetail`. Branch on the detail code, not on the gRPC code alone, because
a malformed request can also produce `FailedPrecondition`. Use the
predicates in `host/local`.

| Detail code | Predicate | What it means | What to do next |
|---|---|---|---|
| `code_overwrite_requires_approval` | `local.IsCodeOverwriteRequiresApproval` | The write would replace something and no approved proposal covers it. | Follow the `Fix` text (below), then call the Apply RPC it names. |
| `code_overwrite_apply_pending` | `local.IsCodeOverwriteApplyPending` | An approved proposal already covers this write, but a Put never applies one. | Call the Apply RPC named in the `Fix` text with the proposal id it gives. |

The `Fix` text for the first one reads: propose the overwrite into the
`code-overwrites` collection, have an operator holding the `code:approve`
scope approve it, then call the named Apply RPC with the proposal id.

The `Fix` text for the second one reads: call the named Apply RPC with proposal
id (the id is filled in) instead of the Put RPC.

A third refusal has no detail code: calling an Apply RPC on a proposal that is
still pending or was rejected returns `FailedPrecondition` saying the proposal
is not approved. An unknown proposal id returns the store's `NotFound`.

## 5. Permissions and scopes

- `code:rw` is the standing permission every Code RPC needs, and that includes
  all five content Apply RPCs. The five content paths need `code:rw` alone. It
  is listed in the manifest's `permissions`.
- The three import RPCs need `code:rw` **plus** `code:import`. `code:import` is
  a second permission, also listed in `permissions`, because import reaches a
  git host outside the console. A pack with `code:rw` but not `code:import` is
  refused on all three. See [`docs/code-import.md`](code-import.md).
- `code:approve` is **not a permission**. It must never appear in a
  manifest's `permissions` list. It appears only as a route's `access.scope`,
  on the route your operator uses to approve. A route that names a scope also
  needs `tokens:issue` in `permissions`.

The reason: a permission is granted once at install and held for the pack's
whole life, while an approval token is presented per decision. If the two
were the same thing, a pack that can ask for an overwrite would hold the power
to approve it forever. `TestCodeApproveScopeIsNotAManifestPermission` in
`manifest/validate_test.go` is the enforcement: it fails if the manifest
checker, the permission list or the manifest schema ever accept the scope as a
permission.

## 6. What the gate does not do

- **An approval covers one application.** The SDK records each applied
  proposal in a marker of its own (the proposal document is never touched, so
  its status stays `approved`). For the four content writes (module, settings,
  Hiera level, Hiera data key) each Apply writes the content frozen at propose
  time. Applying the same proposal again is allowed only when it would change
  nothing, for example a retry after a lost reply. If a later approved
  proposal has changed the same item in the meantime, replaying the old
  proposal is refused with `FailedPrecondition` ("already applied") instead of
  quietly reverting the newer change. An applied proposal is also no longer
  offered by the `code_overwrite_apply_pending` refusal: a fresh Put over that
  item asks for a new proposal.
- **`ApplyEnvironmentDuplicate` is different, and is single-use.** It freezes
  no copy of the source. The approver approved the operation "replace the
  target with a copy of the source", so the source is read when you apply. If
  the source changed after approval but before the first apply, applying copies
  the source as it is now. Once the proposal has been applied, applying it
  again succeeds only if the target already equals a copy of the source (a
  retry after a lost reply). If the source or the target has changed since,
  the call is refused with `FailedPrecondition` ("already applied") and you
  need a new proposal and a new approval. Without this rule one approval would
  be a standing permission to copy whatever the source later became.
- **The Code RPCs cannot be told to skip the gate.** There is no option,
  permission or environment variable that lets a Put through without an
  approved proposal. The trust boundary is the Code facet, not the Documents
  facet: an approval is only honoured when the proposal records that it was
  decided under the `code:approve` scope (`approved_scope`) by a named
  principal (`decided_by`), and `approval.Approve` writes both. The Documents
  facet has no access control, so a pack that can call `Documents.Put`
  directly can write a complete approval record into `code-overwrites` (or
  write the Code collections themselves) and bypass the gate on `host.Local`.
  This holds for all six gated paths, import included: `code:import` is not a
  stronger lock, and the import proposal sits in the same `code-overwrites`
  collection as the others.
  `TestCodeOverwriteApprovalProvenance` pins that. The same goes for the
  `code-overwrite-applied` marker collection that makes an approval
  single-use: a caller with `Documents` access can delete a marker to allow a
  replay, or write one to make an Apply refuse.
- **Creating and deleting are not gated, so the gate can be walked around by
  a caller who means to.** Only an in-place replace is refused. Removing a
  module, level or data key is ungated (D-01), and putting it back afterwards
  is then an ungated create, so any holder of `code:rw` can replace an item
  without approval by calling the remove and then the put. `ReorderHieraLevels`
  and `SetModuledir` also change behaviour with no gate. The gate protects
  against accidental in-place replacement and gives a reviewable path for
  deliberate ones; it is not a control against a `code:rw` holder acting
  maliciously. If you need that, gate the removes as well in the pack or the
  host that embeds this SDK. `TestCodeOverwriteRemoveThenPutBypassesTheGate`
  pins the remove-then-put path so this stays documented rather than
  rediscovered.
- **Text injection is no longer one of these routes.** Until Phase 12.1 a
  holder of `code:rw` could put a module whose name or version contained a
  quote mark and a line break, and have it render as extra `mod` lines in the
  Puppetfile: a second module, or a replacement of one already there, with no
  approved proposal and without the gate ever seeing it. The facet now refuses
  any value it could not write back faithfully, and the text it renders is
  read back and compared against the model before it is stored, so the
  renderer and the file can no longer disagree. Be precise about the boundary:
  the sentence above that the gate is not a control against a `code:rw` holder
  acting maliciously stays true, because remove-then-put and the ungated
  `SetModuledir` and `ReorderHieraLevels` are still there. What changed is
  that the text layer is no longer a way around the gate.
  `TestCode_PuppetfileRejectsInjectedModuleText` (the audit's reproduction),
  `TestCode_ModuledirRejectsInjectedText` and
  `TestCodeOverwriteApplyRefusesHostilePayload` pin the refusals through the
  real RPCs, and `FuzzRenderParseRoundTrip` searches for any value that makes the rendered
  text break its shape (an extra `mod` line, or text outside a quoted value)
  or read back differently.
- **An environment whose Puppetfile already holds injected text must be re-authored.** This is the one operator action Phase 12.1 requires. The
  facet reads the stored Puppetfile on every call, and nothing repairs a
  file written before this change automatically. Do not wait for an error to
  tell you a file is affected, because a read does not always refuse it. An
  injected line that is not valid Puppetfile syntax, or a Forge version that
  carries one, is refused when it is read (`FailedPrecondition`). An injected
  line that is itself a well-formed `mod` statement is not: a Git module name
  such as `zz'` followed by a new line and `mod 'puppetlabs-stdlib', '99.0.0'`
  was written as three ordinary-looking entries, and a read lists all three,
  possibly including a second `puppetlabs-stdlib`. Only the next write fails
  (`InvalidArgument`), because the forged entries do not pass module
  validation. So inspect the module list of every environment created before
  Phase 12.1 and look for modules you did not add. There is
  deliberately no migration: this is a 0.x preview with no released data, and
  a file in that state is exactly the problem that was fixed, so quietly
  rewriting it would be guessing at what its author meant. The remedy is to
  delete the environment (`DeleteEnvironment` removes its Puppetfile without
  reading it, so it works even on a poisoned one) and then create it again, or
  re-import the branch it came from.
- **A pack that can reach both proposing and approving code has no gate.**
  Keep the two apart as described in
  [`docs/approval-pattern.md`](approval-pattern.md).

## Manual verification

For the text-injection checks, see Steps 14 to 18 of
[`docs/control-repo-authoring-testing.md`](control-repo-authoring-testing.md);
they are not repeated here.

This section lets a person confirm the gate by hand with `host.Local`,
without reading the Go source. You will run a small scratch test and read the
results. Nothing here changes the repository; delete the scratch directory at
the end.

1. **Set up.** From the repository root, create a scratch directory and one
   file. It builds a host with `code:rw` and `tokens:issue`, plus helpers to
   propose and approve.

   ```
   mkdir manualcheck
   ```

   Save this as `manualcheck/gate_test.go`:

   ```go
   package manualcheck

   import (
       "context"
       "testing"

       "google.golang.org/grpc/codes"
       "google.golang.org/grpc/status"
       "google.golang.org/protobuf/types/known/structpb"

       "github.com/puppet-stagehand/stagehand-sdk/approval"
       "github.com/puppet-stagehand/stagehand-sdk/code"
       hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
       "github.com/puppet-stagehand/stagehand-sdk/host"
       "github.com/puppet-stagehand/stagehand-sdk/host/local"
   )

   var ctx = context.Background()

   var kind = approval.Kind{Collection: code.OverwriteCollection, ApproveScope: code.OverwriteApproveScope}

   func newHost() *host.Host {
       return local.New([]string{"code:rw", "tokens:issue"}, "manual-check")
   }

   func str(s string) *string { return &s }

   func module(v string) *hostv1.PuppetfileModule {
       return &hostv1.PuppetfileModule{Name: "puppetlabs/ntp",
           Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: v}}}
   }

   func value(t *testing.T, v string) *hostv1.Json {
       s, err := structpb.NewStruct(map[string]any{"v": v})
       if err != nil {
           t.Fatal(err)
       }
       return &hostv1.Json{Value: s}
   }

   // propose creates a pending proposal from a builder's result.
   func propose(t *testing.T, h *host.Host, id string, body map[string]any, err error) {
       t.Helper()
       if err != nil {
           t.Fatal(err)
       }
       if _, err := approval.ProposeBody(ctx, h, kind, id, body); err != nil {
           t.Fatal(err)
       }
   }

   // approve plays the operator: it mints a code:approve token and decides.
   func approve(t *testing.T, h *host.Host, id string) {
       t.Helper()
       tok, err := h.Auth.IssueToken(ctx, &hostv1.IssueTokenRequest{
           Scope: code.OverwriteApproveScope, Label: "operator", TtlSeconds: 300})
       if err != nil {
           t.Fatal(err)
       }
       if _, err := approval.Approve(ctx, h, approval.ApproveRequest{
           Kind: kind, ProposalID: id, TokenSecret: tok.Secret}); err != nil {
           t.Fatal(err)
       }
   }

   func wantRefusal(t *testing.T, err error) {
       t.Helper()
       if !local.IsCodeOverwriteRequiresApproval(err) || status.Code(err) != codes.FailedPrecondition {
           t.Fatalf("want FailedPrecondition code_overwrite_requires_approval, got %v", err)
       }
       t.Log(err)
   }

   func TestManual(t *testing.T) {
       h := newHost()
       // Add the numbered steps below here, in order.
   }
   ```

   Run `go test ./manualcheck -run TestManual -v` after adding each group of
   steps. Every step below shows what to add and what to expect.

2. **Make two environments.** Add
   `h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: n})`
   for `n` in `"prod"` and `"staging"`. Expect success for both. Creating an
   environment is not gated.

3. **Create a new Puppetfile module (ungated).** Add
   `h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{Environment: "prod", Module: module("9.0.0")})`.
   Expect success. The module did not exist, so this is new content.

4. **Create a new Hiera level (ungated).** Add
   `h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{Environment: "prod", Level: &hostv1.HieraLevel{Name: "common", Path: "common.yaml"}, Index: 0, Insert: true})`.
   Expect success.

5. **Create a new Hiera data key (ungated).** Add
   `h.Code.PutHieraDataKey(ctx, &hostv1.PutHieraDataKeyRequest{Environment: "prod", Path: "common.yaml", Key: "ntp::servers", Value: value(t, "a")})`.
   Expect success. The file and key did not exist.

6. **Duplicate onto an unused name (ungated).** Add
   `h.Code.DuplicateEnvironment(ctx, &hostv1.DuplicateEnvironmentRequest{SourceName: "prod", TargetName: "dev"})`.
   Expect success. Nothing was named `dev`.

7. **Puppetfile overwrite is refused.** Add
   `_, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{Environment: "prod", Module: module("9.1.0")})`
   then `wantRefusal(t, err)`. Expect `FailedPrecondition` with detail code
   `code_overwrite_requires_approval`, and a log line saying environment
   `prod` already has puppetfile_module `puppetlabs-ntp` (the lowercase
   hyphen form). Then call
   `h.Code.ListPuppetfileModules` for `prod` and confirm the module is still
   version `9.0.0`. Nothing was written.

8. **Settings overwrite is refused, even the first time.** Add
   `h.Code.PutEnvironmentSettings(ctx, &hostv1.PutEnvironmentSettingsRequest{Settings: &hostv1.EnvironmentSettings{Environment: "prod", ConfigVersion: str("v1")}})`
   and `wantRefusal`. Expect the refusal saying `prod` already has a settings
   record. Then `h.Code.GetEnvironmentSettings` for `prod` and confirm
   `config_version` is still absent.

9. **Duplicate onto a name in use is refused.** Add
   `h.Code.DuplicateEnvironment(ctx, &hostv1.DuplicateEnvironmentRequest{SourceName: "prod", TargetName: "staging"})`
   and `wantRefusal`. Expect the refusal saying `staging` already exists and
   would be replaced by a copy of `prod`. Then `h.Code.GetEnvironment` for
   `staging` and confirm its `updated_at` did not move.

10. **Hiera level overwrite is refused.** Add
    `h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{Environment: "prod", Level: &hostv1.HieraLevel{Name: "common", Path: "global.yaml"}})`
    and `wantRefusal`. Expect the refusal saying `prod` already has hiera_level
    `common`. Then `h.Code.GetHieraHierarchy` for `prod` and confirm the level's
    path is still `common.yaml`.

11. **Hiera data key overwrite is refused.** Add
    `h.Code.PutHieraDataKey(ctx, &hostv1.PutHieraDataKeyRequest{Environment: "prod", Path: "common.yaml", Key: "ntp::servers", Value: value(t, "b")})`
    and `wantRefusal`. Expect the refusal naming the key and `common.yaml`.
    Then `h.Code.GetHieraDataFile` and confirm the value is still `a`.

12. **Apply before approval is refused.** Build and create a proposal:
    `body, err := code.OverwriteBodyForPuppetfileModule("prod", module("9.1.0"))`
    then `propose(t, h, "p1", body, err)`. Do not approve. Call
    `h.Code.ApplyPuppetfileModuleOverwrite(ctx, &hostv1.ApplyPuppetfileModuleOverwriteRequest{ProposalId: "p1"})`.
    Expect `FailedPrecondition` saying proposal `p1` is not approved, and no
    `ErrorDetail` code. Confirm the module is still `9.0.0`.

13. **Approved but not applied: the Put says so.** Add `approve(t, h, "p1")`.
    Repeat the Put from step 7 and check
    `local.IsCodeOverwriteApplyPending(err)` is true. Expect detail code
    `code_overwrite_apply_pending`, with `Fix` text telling you to call
    `ApplyPuppetfileModuleOverwrite` with proposal id `p1`. Confirm the module
    is still `9.0.0`: a Put never applies an approved proposal.

14. **`ApplyPuppetfileModuleOverwrite` applies it.** Call
    `h.Code.ApplyPuppetfileModuleOverwrite` with `p1`. Expect success and a
    module at version `9.1.0`. Call it a second time; expect the same result.
    List the modules and confirm there is still exactly one module, named
    `puppetlabs-ntp`. As a further check, repeat step 7 using the name
    `puppetlabs-ntp`: it is refused in just the same way, because both spellings
    are one module.

15. **`ApplyEnvironmentSettings` applies a settings overwrite.** Build
    `code.OverwriteBodyForSettings(&hostv1.EnvironmentSettings{Environment: "prod", ConfigVersion: str("v1")})`,
    `propose` it as `p2`, `approve` it, then call
    `h.Code.ApplyEnvironmentSettings` with `p2`. Expect `config_version` of
    `v1` and no other field set. Then `GetEnvironmentSettings` to confirm.

16. **`ApplyEnvironmentDuplicate` applies a duplicate.** Build
    `code.OverwriteBodyForEnvironmentDuplicate("prod", "staging")`, `propose`
    it as `p3`, `approve` it, then call `h.Code.ApplyEnvironmentDuplicate`
    with `p3`. Expect the environment `staging`. Then list the modules of
    `staging` and confirm it now has the `prod` module at `9.1.0`. Remember
    this copies `prod` as it is when you apply, not as it was when approved.

17. **`ApplyHieraLevelOverwrite` applies a level overwrite.** Build
    `code.OverwriteBodyForHieraLevel("prod", &hostv1.HieraLevel{Name: "common", Path: "global.yaml"}, 0, false)`,
    `propose` it as `p4`, `approve` it, then call
    `h.Code.ApplyHieraLevelOverwrite` with `p4`. Expect a hierarchy whose
    `common` level path is `global.yaml`, still in first position.

18. **`ApplyHieraDataKeyOverwrite` applies a data key overwrite.** Build
    `code.OverwriteBodyForHieraDataKey("prod", "common.yaml", "ntp::servers", value(t, "b"))`,
    `propose` it as `p5`, `approve` it, then call
    `h.Code.ApplyHieraDataKeyOverwrite` with `p5`. Expect the file
    `common.yaml` to hold `ntp::servers` with the value `b`.

19. **One approval does not cover a different target.** Proposal `p1` was
    approved for the `puppetlabs-ntp` module in `prod`. After step 16 the
    `staging` environment also has that module. Add
    `h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{Environment: "staging", Module: module("9.2.0")})`
    and `wantRefusal`. Expect `code_overwrite_requires_approval` (not
    `code_overwrite_apply_pending`), because `p1` names `prod` and not
    `staging`. Confirm the `staging` module is still `9.1.0`.

20. **Deletes are ungated.** Add
    `h.Code.RemovePuppetfileModule(ctx, &hostv1.RemovePuppetfileModuleRequest{Environment: "prod", Name: "puppetlabs/ntp"})`
    (either spelling finds the module)
    and
    `h.Code.DeleteEnvironment(ctx, &hostv1.DeleteEnvironmentRequest{Name: "dev"})`.
    Expect both to succeed with no proposal.

21. **The scope is not a permission.** Run
    `go test ./manifest -run IsNotAManifestPermission -v`. Expect a pass line
    for both `TestApprovalScopeIsNotAManifestPermission` and
    `TestCodeApproveScopeIsNotAManifestPermission`.

22. **Clean up.** Delete the scratch directory with `rm -r manualcheck` and
    confirm `git status` shows nothing new.

If any expected refusal in steps 7 to 11 instead succeeds, the gate is broken:
stop and report it, because an overwrite went through without an approved
proposal.
