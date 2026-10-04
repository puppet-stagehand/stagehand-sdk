# Testing the control-repo authoring example by hand

This document lets a person confirm by hand that the composed authoring
workflow behaves the way [`docs/control-repo-authoring.md`](control-repo-authoring.md)
says it does. You will run commands from the repository root and read what they
print. Nothing here changes the repository, needs a network, needs a registry
key or needs a git host: the registry, the language model and the git remote in
the example are all in-memory fixtures.

One honest caveat first. The example is a proof, not a running pack, so nothing
listens on the four routes its `manifest.json` declares. You cannot call a route
with `curl`, and there is no port to find. Each route step below is exercised
through the persona method that stands behind the route, using a `go test` name
filter that runs the test which calls it.

If a step does not match what it says to expect, stop and report the step number
and what you saw.

Every command assumes you are in the repository root. The `-count=1` flag stops
Go reusing a cached result, so you always see a fresh run, and `-v` makes the
tests print the narration lines the steps ask you to read. Each narration line
begins with the test file name and a line number. Match on the text after that
prefix, because the line number changes whenever the test file is edited.

## The four declared routes

### Step 1: `proposeImport`

The manifest declares `POST imports/proposals`, with no access scope. The
persona method that stands behind it is `ProposerBackend.ProposeImport`, which
files one pending import proposal.

Run the whole end-to-end story and read the import lines:

```
go test ./examples/control-repo-authoring/ -run 'TestControlRepoAuthoring_EndToEnd$' -v -count=1
```

Then run the test that proposes an import and leaves it undecided:

```
go test ./examples/control-repo-authoring/ -run 'TestControlRepoAuthoring_PendingImportDoesNotApply$' -v -count=1
```

**Expected:** the first run ends with `PASS` and includes a line containing
`step 12: proposed import "import-adopt-1" over branches canary and qa_two,
pinned to the commits the report showed`, followed by a line containing `step
13: apply refused while pending (FailedPrecondition); environments still
[authored canary], canary still has no module`. The second run ends with `PASS`
and prints `pending import "import-pending-1" refused with FailedPrecondition;
no environment was created`.

**What this proves:** proposing an import files a request and writes nothing. An
import nobody has approved changes no environment at all.

### Step 2: `proposeOverwrite`

The manifest declares `POST overwrites/proposals`, with no access scope. The
persona method behind it is `ProposerBackend.ProposeModuleOverwrite`, which
files one pending request to replace a module that already exists.

```
go test ./examples/control-repo-authoring/ -run 'TestControlRepoAuthoring_EndToEnd$' -v -count=1
go test ./examples/control-repo-authoring/ -run 'TestControlRepoAuthoring_RejectedOverwriteDoesNotApply$' -v -count=1
go test ./examples/control-repo-authoring/ -run 'TestControlRepoAuthoring_ReplayRefused$' -v -count=1
```

**Expected:** each run ends with `PASS`. The end-to-end run shows the bump ladder
in order: `step 16a: bumping to 9.6.0 is refused as requiring approval under
every spelling (puppetlabs-stdlib, puppetlabs/stdlib, PuppetLabs/stdlib); canary
still carries one module at 9.4.1`, then `step 16b: proposed the bump as
"bump-stdlib-1" (status pending)`, then `step 16c: a pending proposal does not
unlock the write; still refused as requiring approval`. The rejected run prints
`rejected overwrite "bump-rejected-1" ("not this sprint") did not apply; a
second decision was refused; the module stayed at 9.4.1`. The replay run prints
`approval "bump-replay-1" applied once, repeated idempotently, then refused
after the target moved to 9.8.0; the module stayed at 9.8.0`.

**What this proves:** an overwrite is refused until it is proposed and approved,
every spelling of the module is gated (the hyphen form, the slash form and a
mixed-case owner all name one module), a pending proposal does not unlock it,
and one approval covers one application only.

### Step 3: `approveProposal`

The manifest declares `POST proposals/{proposal_id}/approve`, with the access
scope `code:approve`. The persona method behind it is
`ApproverBackend.Approve`, which presents a token somebody else obtained.

```
go test ./examples/control-repo-authoring/ -run 'TestControlRepoAuthoring_EndToEnd$' -v -count=1
go test ./examples/control-repo-authoring/ -run 'TestControlRepoAuthoring_RejectedOverwriteDoesNotApply$' -v -count=1
```

**Expected:** both runs end with `PASS`. The end-to-end run prints three
approvals, each by a named operator: `step 8: operator-ada approved
"settings-authored-1"`, `step 14: operator-grace approved "import-adopt-1" and
the apply wrote 2 environments`, and `step 16d: operator-grace approved
"bump-stdlib-1"`. The rejected run passes only because, inside it, approving a
proposal that was already rejected is refused as already decided.

**What this proves:** a decision is made by a person holding a token, each
approval names who made it, and a decided proposal cannot be decided again.

### Step 4: `rejectProposal`

The manifest declares `POST proposals/{proposal_id}/reject`, with the access
scope `code:approve`. The persona method behind it is `ApproverBackend.Reject`.

```
go test ./examples/control-repo-authoring/ -run 'TestControlRepoAuthoring_RejectedOverwriteDoesNotApply$' -v -count=1
```

**Expected:** the run ends with `PASS` and prints `rejected overwrite
"bump-rejected-1" ("not this sprint") did not apply`. The test only reaches that
line if three things held first: a rejection with an empty reason was refused,
the rejection with the reason `not this sprint` was stored, and applying the
rejected proposal was refused. A failure in any of them prints a message that
names which one.

**What this proves:** a rejection must say why, a rejected proposal never
applies, and the module it would have changed stays at 9.4.1.

## What only a person can check

### Step 5: read the whole story, in order

```
go test ./examples/control-repo-authoring/ -v -count=1
```

**Expected:** nine tests pass and the last line is `ok`. Read the lines the
end-to-end test printed from top to bottom. They must arrive in this order: two
blank environments (step 1), a suggestion (step 2), the check (steps 3 and 4),
the module writes (step 5), Hiera (step 6), the settings approval (steps 7 to
10), the import report and its approval (steps 11 to 15) and then the module
bump (steps 16a to 16f). Every approval must be preceded by a refusal or a
pending state: settings are proposed before step 8, the import is refused while
pending at step 13 before step 14, and the bump is refused at step 16a, and again
at 16c, before the approval at 16d.

**What this proves:** the transcript tells the same story the guide tells, and
no step writes replaced content before a person said yes.

### Step 6: read the example README

Open `examples/control-repo-authoring/README.md`.

**Expected:** its last section says, in plain words, that the workflow **does
not deploy** and **does not write back** to a real control repo. Read it as if
you had never used Puppet. You should be able to follow what the example is for
and what it leaves out.

**What this proves:** the scope boundary is stated where a newcomer will read
it, in words a newcomer can follow.

### Step 7: read the ELI10 guide

Open `docs/control-repo-authoring.md`.

**Expected:** it reads plainly for someone new to Puppet. Every term in the
"Some words first" section is explained before it is used. The "What this does
not do" section says the workflow does not deploy, does not write back to the
real repository and does not merge an import item by item. It also says nothing
listens on the example's routes.

**What this proves:** the guide a pack author reads says what the software does
and nothing more.

### Step 8: run the manifest validator

```
go run ./cmd/pack-check --format json examples/control-repo-authoring/manifest.json
```

**Expected:** the output contains `"ok": true` and `"findings": null`.

**What this proves:** the example's manifest is valid for contract version 1,
with the six permissions it declares and the four routes this document walked
through.

### Step 9: run the repository-wide checks

Run each command from the repository root and compare with what is written
under it.

1. The build and the vet.

   ```
   go build ./... && go vet ./...
   ```

   Expect no output and an exit status of 0.

2. The whole test suite with the race detector.

   ```
   go test ./... -count=1 -race
   ```

   Expect every package to print `ok`.

3. The protocol linter, then regenerating the protocol output.

   ```
   buf lint
   buf generate && git diff --exit-code -- gen/
   ```

   Expect no output from either command and an exit status of 0. This work
   edits no protocol file, so regenerating must leave no difference.

4. The command directory still holds only the validator.

   ```
   git ls-files cmd/
   ```

   Expect exactly one line, `cmd/pack-check/main.go`.

5. Nothing the example depends on has changed. This check and the next one
   compare against the commit that introduced the example, so they hold only at
   that commit. Later phases changed `host`, `approval`, `code` and `manifest`
   on purpose; on a later branch expect differences there and skip checks 5 and
   6.

   ```
   git diff --exit-code 4702293 -- examples/inventory-onboarding examples/opentofu-lite host approval code manifest proto gen
   ```

   Expect no output and an exit status of 0. `4702293` is the last commit before
   this example's work began. If your checkout does not have that commit, compare
   against the commit your branch was created from instead.

6. Nothing outside the new example and the documents has changed.

   ```
   git diff --name-only 4702293 -- . ':!.planning' ':!.gsd'
   ```

   Expect only files under `examples/control-repo-authoring/`, the two new
   documents `docs/control-repo-authoring.md` and
   `docs/control-repo-authoring-testing.md`, and the two files that point at
   them, `docs/pack-author-guide.md` and `README.md`.

7. The module files are unchanged.

   ```
   git diff --exit-code 4702293 -- go.mod go.sum
   ```

   Expect no output and an exit status of 0. This proves no dependency was added.

**What this proves:** the example builds, passes under the race detector, adds
no command, no protocol change and no dependency, and touches nothing outside
its own directory and its documents.

## What changed in Phase 12

Phase 12 changed four behaviours. Each step below is a way to see the new
behaviour with your own eyes. Steps 12 and 13 deliberately break a manifest, so
work on a copy in a scratch directory and never commit the copy.

### Step 10: every spelling of a module is gated

A Forge module can be written `puppetlabs-stdlib`, `puppetlabs/stdlib` or
`PuppetLabs/stdlib`. All three are one module, so none of them can slip past the
overwrite gate.

```
go test ./examples/control-repo-authoring/ -run 'TestControlRepoAuthoring_EndToEnd$' -v -count=1 | grep 'step 16a'
```

**Expected:** exactly one line, containing `step 16a: bumping to 9.6.0 is
refused as requiring approval under every spelling (puppetlabs-stdlib,
puppetlabs/stdlib, PuppetLabs/stdlib); canary still carries one module at
9.4.1`.

**What this proves:** the destructive-overwrite gate decides by the module's
identity and not by how it is typed, so a different spelling cannot add a
second, ungated copy.

### Step 11: a hand-written approval is refused by Inventory onboarding

Onboarding a node needs a proposal that a person really approved. A proposal
whose body merely says `approved`, with no approving scope and no named
decider, is refused.

```
go test ./host/local/ -run 'TestInventory_OnboardNodeRequiresApprovedProposal' -v -count=1
```

**Expected:** the run ends with `PASS`, and the subtests
`approved_with_no_approved_scope`, `approved_under_a_different_scope` and
`approved_with_empty_decided_by` each pass. Each passes only because
`OnboardNode` returned a `FailedPrecondition` error for a proposal that carried
no approving scope, the wrong one or no decider (the shared
`approval.RequireApproved` check).

**What this proves:** the Inventory gate is as strict as the Code gate. Only a
proposal that went through `Approve` can onboard a node.

### Step 12: the removed Forge permission is refused by pack-check

`forge:read` no longer exists. Searching and resolving need `forge:rw`, and
recommendations need `forge:recommend`.

```
SCRATCH=$(mktemp -d)
sed 's/"forge:rw", /"forge:rw", "forge:read", /' examples/control-repo-authoring/manifest.json > "$SCRATCH/manifest.json"
go run ./cmd/pack-check --format json "$SCRATCH/manifest.json"
```

**Expected:** `"ok": false` and exit status 1, with one finding whose `code` is
`permission_unknown`, whose `message` is `unknown permission forge:read`, and
whose `fix` line lists the live permissions, including `forge:rw` and
`forge:recommend` and no `forge:read`.

**What this proves:** a manifest that still asks for the removed permission is
refused with a fix line that points at the two real Forge permissions. Delete
`$SCRATCH` afterwards; do not copy the file back into the repository.

### Step 13: `code:import` without `code:rw` is refused by pack-check

An import writes through the Code facet, so a pack that declares `code:import`
must also declare `code:rw`.

```
SCRATCH=$(mktemp -d)
sed 's/"code:rw", //' examples/control-repo-authoring/manifest.json > "$SCRATCH/manifest.json"
go run ./cmd/pack-check --format json "$SCRATCH/manifest.json"
```

**Expected:** `"ok": false` and exit status 1, with one finding whose `code` is
`code_import_requires_code_rw`, at path `/permissions`, with the fix line `Add
"code:rw" to permissions.`

**What this proves:** a manifest that passes pack-check cannot be refused by the
host at install for this reason. Delete `$SCRATCH` afterwards; do not commit the
copy.

## What changed in Phase 12.1

Phase 12.1 closed a hole in how the Code facet writes a Puppetfile, and it
changed six behaviours in doing so. Each step below is a way to see one of them
with your own eyes. Unlike Steps 12 and 13, none of these steps edits a file:
each is a read-only test run, so there is no scratch directory to make and
nothing to clean up afterwards.

### Step 14: a module name cannot smuggle in a second module

A Puppetfile is a Ruby file. Until this change, the Code facet dropped a
module's name straight into that file between two quote marks. A name that
contained a quote mark and a line break could therefore close the quote early
and add whole extra lines: a second module, at a version nobody approved. The
Code facet now checks every value it writes and refuses anything that could do
that. The answer is always refusal and never quoting-around-it, because a value
that had been quoted around would read back as something different from what
was written.

```
go test ./host/local/ -run 'TestCode_PuppetfileRejectsInjectedModuleText' -v -count=1
```

**Expected:** the run ends with `PASS`, and the verbose output shows a `--- PASS`
line for each of three sub-tests: `git_name_injects_mod_lines`,
`forge_version_injects_statement` and
`control_well-formed_git_module_is_appended`. The first two are the hostile
writes. The third is the control, which writes an ordinary, well-formed module
and must succeed.

**What this proves:** three things. The hostile write was refused with
`InvalidArgument`. The stored Puppetfile text is byte-for-byte what it was
before the attempt, so a refusal is never a partial write. The module that was
already there is still pinned at its original version, so the attempt did not
quietly replace it either. This is the exact scenario the v0.3.0-rc.1 milestone
audit reproduced as finding NEW-2. The control sub-test is there so that a
mistake which made every write fail could not be mistaken for a pass.

### Step 15: the module directory setting cannot forge a module line

One setting, the module directory, is written into the Puppetfile but has no
approval gate on it at all: changing it is treated as a plain setting change.
That made it the easiest place to inject text, so it is now checked by the same
rule as every other value. Two kinds of value are deliberately still allowed: a
path that starts with `/` and a path that contains `..`. The Code facet has no
opinion about where you keep your modules (SD-6). It only cares whether the
value can be written back into the file faithfully.

```
go test ./host/local/ -run 'TestCode_ModuledirRejectsInjectedText' -v -count=1
```

**Expected:** the run ends with `PASS`, with a `--- PASS` line for each of four
refusal sub-tests (`refuses_injects_mod_line`, `refuses_trailing_backslash`,
`refuses_nul_byte`, `refuses_paragraph_separator`) and for each of four
accepted-value sub-tests (`accepts_thirdparty`, `accepts_/srv/modules`,
`accepts_../up`, and `accepts_`, which clears the setting).

**What this proves:** an injected module-directory value is refused with
`InvalidArgument`, the stored Puppetfile text and its module list are unchanged
after each refusal, and the ordinary values still work, including an absolute
path and a `..` path.

### Step 16: an approval does not make a bad value good

The overwrite gate exists so that a person reviews a replacement before it
happens. A reviewer reads the payload a pack proposed, and a name with an
invisible line break in it is exactly the kind of thing that survives a skim. So
the Code facet checks the payload again at the moment it applies it, after the
approval.

```
go test ./host/local/ -run 'TestCodeOverwriteApplyRefusesHostilePayload' -v -count=1
```

**Expected:** the run ends with `PASS`, with a `--- PASS` line for the two
hostile sub-tests, `git_name_injects_mod_lines` and
`forge_version_injects_statement`, and for the control sub-test,
`control_well-formed_payload_is_applied`.

**What this proves:** the apply was refused with `InvalidArgument` even though
the proposal was genuinely approved. The stored file is unchanged. The approval
was not used up, so the record still shows that the proposal was never applied.
The control sub-test shows that a well-formed approved payload is still applied
normally.

### Step 17: the two readers agree, and an import cannot bring one module in twice

The Code facet has two Puppetfile readers: a strict one for files it wrote
itself, and a forgiving one for files it is importing from somebody else's
repository. If the forgiving one accepts something the strict one would read
differently, the file means one thing and the model says another. Two new
refusals close that gap. A `mod` line whose value is a Ruby expression, or that
carries a leftover word, is skipped whole rather than imported partially. A line
that sets the same attribute twice (`:git` or `:default_branch`) is skipped,
because Ruby would use the last value while a person reading the file sees the
first.

There is one more change. The same Forge module can be written
`puppetlabs/stdlib` or `puppetlabs-stdlib`, and an imported file holding both
now comes in as one module with a warning instead of two entries. This is fixed
**at import time only**. An environment that already holds two entries keeps
working exactly as before (SD-5), because strict reading, adding, removing and
applying all still tolerate a stored duplicate.

```
go test ./code/ -run 'TestPuppetfile_ParseRejectsLeftoverText|TestPuppetfile_ParseRejectsRepeatedGitAttribute|TestParsePuppetfileLenient_Catalog|TestLenientAgreesWithStrict|TestParsePuppetfile_StrictToleratesCrossSpellingDuplicate' -v -count=1
```

**Expected:** the run ends with `PASS`. Among the many catalog rows, look for
`git_attr_value_is_an_expression`, `git_attr_trailing_garbage`,
`git_attr_trailing_token_after_ref`, `repeated_git_attribute`,
`repeated_default_branch_attribute`, the `moduledir_value_*` rows,
`duplicate_forge_module_other_spelling`,
`duplicate_forge_module_hyphen_spelling_first`,
`duplicate_forge_module_mixed_case_owner` and the two `..._do_not_collide`
rows. Also look for `TestLenientAgreesWithStrict` with its two sub-tests, and
`TestParsePuppetfile_StrictToleratesCrossSpellingDuplicate`.

**What this proves:** strict reading refuses each shape; the import reader
reports each one as a warning and skips the statement; the two readers are
checked against each other on a shared set of texts; a Forge module never
swallows a Git module of a similar name; and strict reading still accepts a
stored cross-spelling duplicate, which is why nothing already stored breaks.

### Step 18: anything the facet writes reads back as the same thing

This is the real rule behind all of the above, in one sentence: whatever the
Code facet writes into a Puppetfile has to read back as exactly the modules it
thought it was writing. This step asks the computer to spend thirty seconds
trying to find a value where that is not true.

```
go test ./code/ -run '^$' -fuzz FuzzRenderParseRoundTrip -fuzztime 30s
```

**Expected:** the run prints a line every few seconds, each with an elapsed
time, an execution count and a `new interesting` count, and it ends with `PASS`.
The recorded run made about 660,000 executions; yours will differ with the
speed of your machine. `new interesting` lines during the run are normal and
are not failures. If the run ever reports a failing input, it writes a file
under `code/testdata/fuzz/`. That file should be reported, not committed.

**What this proves:** that the known attacks are blocked (all ten of the
audit's payloads are also pinned as ordinary tests that run in every plain
`go test`), and that a search for new values that break the *shape* of the
written file found nothing in that time budget. The search checks the written
text with its own small rules, separate from the facet's code: the number of
`mod` lines equals the number of modules, and nothing sits outside a quoted
value except the fixed `mod`, `moduledir` and `:key =>` words. It also checks
that the text reads back as the same modules, but that second check is the same
comparison the facet already makes before it stores anything, so on its own it
cannot fail. Be clear about what this does not show: it cannot see a value that
the facet writes safely but that Ruby would later read differently (for example
a double-quoted `#{...}` in an imported file). Those cases are covered by the
named tests, not by the search.

### Step 19: the published contract no longer advertises a permission that does not exist

The reference copy of the wire contract under `schema/proto/` is what this
repository's own instructions point a coding assistant at. One comment there
still named a Forge permission, `forge:read`, that was deleted in Phase 12, so
an author following it would declare a permission the validator now refuses.

```
go test ./manifest/ -run 'TestSchemaProtoReferenceForgePermissionIsCurrent' -v -count=1
```

**Expected:** the run ends with `PASS`.

**What this proves:** the comment names the permission the Forge facet really
requires, `forge:rw`, and points at the live contract file; and the deleted
permission appears in neither copy of the contract. One thing was deliberately
not done: the reference copy was not regenerated from the live one, because that
would replace a published document wholesale. Whether to do that is still an
open decision (SD-4).
