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
in order: `step 16a: bumping puppetlabs-stdlib to 9.6.0 is refused as requiring
approval; canary still carries 9.4.1`, then `step 16b: proposed the bump as
"bump-stdlib-1" (status pending)`, then `step 16c: a pending proposal does not
unlock the write; still refused as requiring approval`. The rejected run prints
`rejected overwrite "bump-rejected-1" ("not this sprint") did not apply; a
second decision was refused; the module stayed at 9.4.1`. The replay run prints
`approval "bump-replay-1" applied once, repeated idempotently, then refused
after the target moved to 9.8.0; the module stayed at 9.8.0`.

**What this proves:** an overwrite is refused until it is proposed and approved,
a pending proposal does not unlock it, and one approval covers one application
only.

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

5. Nothing the example depends on has changed.

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
