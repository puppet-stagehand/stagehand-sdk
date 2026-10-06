# Importing an existing control repo: look first, then adopt (ELI10)

This guide is for a pack author who is new to Puppet and new to this SDK. It
explains three calls of the Code facet, `InspectImport`, `ProposeImport` and
`ApplyImport`, from nothing to a finished import.

> Import never writes anything until a person has read a report of what it
> found and said yes. The report always comes first.

A company that already uses Puppet keeps all of its Puppet code in one git
repository. Import reads that repository from its git address and turns each
branch into an environment in the Code facet, so you do not have to type it all
in again.

## Some words first

- **Control repo** - the one git repository that holds all of a site's Puppet
  code. It is what you are importing.
- **Branch** - a named line of work inside a git repository. A repository can
  have many.
- **Environment** - one named copy of the code, such as `production` or `dev`.
  Nodes are told which environment to use. Real Puppet tools usually make one
  environment out of each branch of the control repo, with the branch name
  becoming the environment name. That is the idea of
  **branch-as-environment**, and import follows it exactly: the branch `dev`
  becomes the environment `dev`.
- **Puppetfile** - a plain text file at the top of a branch that lists the
  modules the environment needs, with the version of each.
- **`hiera.yaml`** - a file at the top of a branch that tells Puppet where to
  look up settings that differ per node or per group. It lists **levels**, and
  each level names the data files to look in. Hiera is the name of that lookup
  system.
- **`data/`** - the folder of data files (written in YAML) that the hierarchy
  points at. Each file holds named settings.
- **`environment.conf`** - a small settings file a branch can have. The Code
  facet calls what it holds the environment **settings**.
- **Finding** - one note the importer writes down when it meets something in a
  branch it cannot bring in, or can only bring in partly. Findings are how
  import tells you what it left behind instead of silently dropping it.
- **Proposal** - a written request that says exactly what you want to do and
  what you want to replace. For import, one proposal covers every branch you
  selected, and it holds a frozen copy of what was read.
- **Approval scope** - a label on a short-lived token. The scope `code:approve`
  says the holder may say yes or no to proposals about code.
- **Sealed secret** - a value stored through the Secrets facet instead of in
  plain Documents. A git login lives only in one.
- **Credential** - in this guide, the name of a sealed secret that holds a git
  login. You pass the name, never the login itself.

## 1. The loop, step by step

The loop is the same shape as the one in
[`docs/code-overwrite-gating.md`](code-overwrite-gating.md), which explains
proposing, approving and applying in full. Read that first if you have not.
This page only describes what import adds: a **report step at the front**, and
the fact that **one proposal covers every selected branch at once**. Nothing is
written until after a human has approved. That ordering is the whole point.

1. **Declare the permissions.** Put both of these in `permissions` in your
   `manifest.json`:

   | Permission | What it lets you do |
   |---|---|
   | `code:rw` | Use the Code facet at all. Every Code call needs it. |
   | `code:import` | Use the three import calls, and only those. |

   Import needs **both**. `code:import` is separate from `code:rw` on purpose:
   it reaches a git host outside the console, and that is a choice an operator
   should see on its own line. A pack with only `code:rw` that calls an import
   RPC is refused. Section 6 says what granting `code:import` really reaches.
   The approval scope `code:approve` is **not** a permission and must never go
   in `permissions`; it goes only on the route your operator uses to approve.
2. **Seal a credential, if the repository is private.** For a public
   repository skip this step: leave the credential name empty and the fetch is
   anonymous. Otherwise store one secret through the Secrets facet
   (`secrets:rw`) and remember its **name**. Section 4 gives the shape.
3. **Inspect.** Call `InspectImport` with the repository address, optionally
   the credential name, and optionally a list of branch names to look at (an
   empty list means every branch):

   ```go
   report, err := h.Code.InspectImport(ctx, &hostv1.InspectImportRequest{
       Url:        "https://git.example.com/platform/control-repo.git",
       Credential: "my-git-login", // the NAME of a sealed secret, or empty
   })
   ```

   This fetches the repository shallowly, reads each branch and returns a
   report. It writes **nothing** to the Code facet and files no proposal.
4. **Read the report.** For each branch you get: the branch name, the commit
   that was read, whether it is `importable`, whether it `will_overwrite` an
   environment of the same name that already exists, the content found, and a
   list of findings. Section 2 explains the findings. Show this to a person.
5. **Propose.** Call `ProposeImport` with a proposal id you choose, the same
   address and credential name, and the branches you want (an empty list means
   every importable branch):

   ```go
   resp, err := h.Code.ProposeImport(ctx, &hostv1.ProposeImportRequest{
       ProposalId:      "adopt-control-repo",
       Url:             "https://git.example.com/platform/control-repo.git",
       Credential:      "my-git-login",
       Branches:        []string{"production", "dev"},
       ExpectedCommits: map[string]string{"production": "<commit from the report>", "dev": "<commit from the report>"},
   })
   ```

   `ProposeImport` fetches again, freezes what it read into one pending
   proposal, and returns it so you can show the approver exactly what was
   filed. `ExpectedCommits` is an optional guard: if you pass it, it must name
   every selected branch, and the call is refused if a branch has moved since
   the report your person read (section 8). It is how you stop a proposal from
   being filed against code the human never saw. A branch you name that cannot
   be imported is refused outright, never quietly dropped. If you name no
   branches, branches that cannot be imported stay in the proposal marked
   "not importable" so the approver reads the same findings the report showed;
   `ApplyImport` skips them.
6. **Get it approved.** Someone holding a token for the `code:approve` scope
   approves the proposal, in code your proposing path cannot reach. Nothing in
   the import calls can approve a proposal, and they cannot be told to.
7. **Apply.** Call `ApplyImport` with only the proposal id:

   ```go
   done, err := h.Code.ApplyImport(ctx, &hostv1.ApplyImportRequest{ProposalId: "adopt-control-repo"})
   ```

   `ApplyImport` makes **no network call and needs no credential**. It writes
   exactly what was frozen into the proposal, so an approved proposal can never
   be used to write anything the approver did not read, even if someone pushes
   to the repository afterwards. It is all-or-none across every selected
   branch. Repeating it after a lost reply is harmless. It always needs an
   approved proposal, even when every branch is brand new.
8. **Read the content back.** Use the ordinary Code read calls on the new
   environments: `ListEnvironments`, `ListPuppetfileModules`,
   `GetHieraHierarchy`, `ListHieraDataFiles`, `GetHieraDataFile` and
   `GetEnvironmentSettings`.

A branch whose environment already exists is **replaced as a whole**: its
Puppetfile, hierarchy, data files and settings are all swapped for what was
frozen. The report marks this with `will_overwrite`, so the approver sees it
being replaced. If an environment appears after the proposal was filed and the
proposal did not flag it, `ApplyImport` refuses the whole import (section 8).

## 2. Warnings and errors

Every finding has one of two severities, and they mean what they sound like:

- **Warning.** One construct was skipped and the rest of the file still came
  in. Example: a line in a Puppetfile the importer does not understand.
- **Error.** That **file** did not come in at all. Example: a data file that is
  not valid YAML.

Nothing except an **invalid branch name** stops a whole branch. A branch full
of errors is still importable; it just comes in without those files. The
errors are shown prominently in the proposal, and **the human, not the pack,
decides** whether to go ahead. A pack should show the findings to a person and
let them choose, not hide them.

Nothing in Phase 12.1 turned a warning into an error, so a branch carrying any
of the shapes in section 3 is still importable; it simply comes in without those
statements.

## 3. What import cannot represent

The Code facet models a fixed set of things. Whatever falls outside is listed as
a finding rather than lost silently. Each row names the real finding kind.

| What you have | Finding kind | Severity | What happens, and what you can do |
|---|---|---|---|
| A `forge` line at the top of a Puppetfile | `puppetfile_forge_directive` | warning | The line is skipped (the model has no field for a forge address). The rest imports. Nothing to do; the line is harmless here. |
| Ruby in a Puppetfile (a variable, an `if`, a method call, a bare symbol) | `puppetfile_unsupported_ruby` | warning | The Ruby line itself is skipped. Write the module as a plain `mod` line to bring it in. **Be careful with structure:** the warning lands on the structural line (the `if`, `else`, `end` or `=begin`), not on the modules around it. A `mod` line inside a conditional, inside a block comment (`=begin` ... `=end`), or after the end-of-source marker (`__END__`) is **still imported**, with no finding of its own. For `if`/`else` that means the first arm comes in as if it were unconditional, and the other arm is flagged as a duplicate. The report is weaker than it looks, so for a repository that uses these constructs, read the imported module list against the Puppetfile before approving. Three more shapes get this same finding and are skipped. A `mod` statement whose attribute value is an expression or that carries a leftover word is skipped whole, rather than imported without the part the model cannot hold. A statement that sets `:git` or `:default_branch` twice is skipped, because Ruby would use the last value while a person reading the file sees the first. A `moduledir` value that is not one plain folder name (a path such as `/srv/modules`, `../up` or `a/b`, a name starting with `.` or `-`, one longer than 64 characters, or one carrying a quote, a backslash, a line break or a control character) is skipped, leaving the setting unset; a later valid `moduledir` line is still used, and an invalid line never replaces an earlier valid one. A double-quoted value that Ruby would compute at run time (it contains `#{...}`, `#$name` or `#@name`, for example `:tag => "v#{VERSION}"` or `moduledir "#{DIR}"`) is skipped too, because importing it as plain text would change what gets deployed. The same text inside single quotes is ordinary text in Ruby and is imported as written. |
| A `mod` line with an attribute the facet has no field for | `puppetfile_unmodelled_attribute` | warning | The **whole** `mod` statement is skipped, because bringing in the module without the attribute would deploy something the author did not write. |
| A `mod` line naming two conflicting refs, or a duplicate module or `moduledir` | `puppetfile_conflicting_ref`, `puppetfile_duplicate_module`, `puppetfile_duplicate_moduledir` | warning | The conflicting statement is skipped; for a duplicate module the first is kept; for a duplicate `moduledir` the last is kept. Two spellings of the same Forge module (`puppetlabs/stdlib` and `puppetlabs-stdlib`, in either order and whatever the owner's letter case) count as the same module, so the second is reported as a duplicate and the first is kept with its own version. This applies to importing only: an environment that already holds both entries is untouched. |
| A module the facet would not accept | `puppetfile_invalid_module` | warning | Skipped. |
| A Hiera version 3 `hiera.yaml`, or one with no version | `hiera_version_unsupported` | **error** | The hierarchy does not come in. Only Hiera version 5 is understood. A version 3 file is refused on purpose: reading it as empty could replace a real hierarchy with nothing. |
| A `hiera.yaml` that cannot be read back | `hiera_unparseable` | **error** | The hierarchy does not come in. |
| An eyaml level, or any key the model has no field for (`lookup_key`, `options`, `data_dig`, `hiera3_backend`, `globs`, `uri`, `uris`, `default_hierarchy`, `plan_hierarchy`) | `hiera_unmodelled_key` | warning | The raw `hiera.yaml` text is kept so the key is not lost on import, but the facet cannot show or edit it, and a later `PutHieraLevel` rewrite of that level would drop it. One warning per key. |
| Unnamed or duplicate levels, or a `datadir` that cannot be resolved | `hiera_level_unnamed`, `hiera_duplicate_level`, `hiera_datadir_unresolvable` | warning | Levels are matched by name, so these are flagged; no files are imported for an unresolvable `datadir`. |
| A data file the facet cannot read back (a list or plain value at the top, an unquoted timestamp, invalid text, too deeply nested) | `data_file_unparseable` | **error** | That file does not come in. The message names the cause. Fix the file in the repository. |
| A data file that is not YAML, a symlink, a submodule pointer, over the size limit, a duplicate path, outside the data folder, or with extra YAML documents | `data_file_not_yaml`, `data_file_symlink`, `data_file_gitlink`, `data_file_too_large`, `data_file_collision`, `data_file_outside_datadir`, `data_file_extra_documents` | warning | Skipped (for extra documents, only the first document counts). A symlink is never followed. |
| An `environment.conf` line the facet does not know, a section header, a line with no `=` (or a value that cannot be stored faithfully), or a true/false setting holding something else | `envconf_unrecognized_key`, `envconf_malformed_line`, `envconf_invalid_boolean` | warning | The line is skipped; the rest of the file is kept. |
| A `Puppetfile`, `hiera.yaml` or `environment.conf` that is a symlink, a submodule pointer, over the size limit or unreadable | `branch_file_unreadable` | **error** | That file does not come in. |
| A branch with more content than the limits allow | `branch_data_cap_exceeded`, `findings_truncated` | warning | The data walk stops at the limit with a note; past 200 findings one last note says more were left out. |
| A branch whose **name** breaks the environment-name rule | `branch_name_invalid` | **error** | The branch is **not importable** and is never fetched. See below. |

### Branch names

An environment name may contain only lowercase letters, digits and the
underscore (`^[a-z0-9_]+$`). A branch called `feature/new-thing` does not match
and so cannot become an environment. This importer **never renames** a branch.

This can surprise someone who knows real r10k, the tool most sites use to
deploy a control repo. By default r10k would accept that branch and deploy it
under a corrected name, replacing each odd character with an underscore. Import
does not do that, because renaming could make two different branches land on
the same environment. The finding message says the same thing. To import such a
branch, push it under a valid name first.

## 4. Naming a credential

A private repository needs a login. Four rules keep that login safe:

1. **It is a sealed secret.** Store it with `Secrets.Store`. It never sits in
   Documents unsealed and never in your image.
2. **It is named in the request, and there is no default.** You pass the secret's
   name in `Credential`. An empty name means an anonymous fetch. The host never
   guesses a login for you, and never reads a list of them. This holds for
   `ssh` as well as `https`. With no name given, an `ssh` fetch carries no key
   and talks to no `ssh` agent. So if a repository can only be opened by the
   operator's own key, an import with no credential named is **refused**. It
   does not quietly succeed using that key.
3. **It is never in the URL, and never on a command line.** A git address with a
   password in it is refused, because the address would end up in logs and in a
   process listing. (An `ssh://` address may carry a bare login name, the part
   before the host, because that is a user name and not a secret; it may never
   carry a password. An `https` address may carry nothing.)
4. **It is never kept.** The host reveals the one secret you named, holds it for
   the one fetch, and then forgets it. It is not logged, not stored in a
   proposal, and not echoed in an error. `ApplyImport` needs none, because it
   makes no network call.

The sealed value is a small JSON object with a `kind`:

| `kind` | Fields | For |
|---|---|---|
| `https_token` | `username`, `token` | A private repository over `https` with an access token. |
| `ssh_key` | `username`, `private_key` | A repository over `ssh`. The key must be **unencrypted**. |

An example that names a credential and nothing else:

```go
// Seal once (the value comes from a protected source, never from this file):
_, err := h.Secrets.Store(ctx, &hostv1.StoreSecretRequest{
    Name:      "my-git-login",
    Plaintext: sealedJSON, // {"kind":"https_token","username":"...","token":"..."}
})
// Then only the name travels with each request:
//   InspectImportRequest{Url: "...", Credential: "my-git-login"}
```

A secret that exists but is malformed (for example an unknown `kind`) is an
error that names the credential and the field, never the value.

## 5. What the host needs

Import runs the system `git` program, so the machine running the host needs a
few things in place:

- **`git` on the `PATH`, version 2.32 or newer.** The host refuses to run an
  older one, and it also refuses if it cannot tell the version. The reason is
  safety: the host tells git to ignore the operator's own git settings, and an
  older git does not obey that, so it would read them anyway. If `git` is
  missing, every import call says so.
- **For `ssh` addresses, an `ssh` program and a populated `known_hosts` file.**
  (`known_hosts` is the file where `ssh` remembers which servers you have
  already checked and trust. Strict checking means an unlisted server is
  refused.)

  - **What the host reads.** Exactly **one file** from the operator's home
    directory: the usual `~/.ssh/known_hosts`. It works out that path once, when
    it starts, and hands it to `ssh` by name. Nothing else under the home
    directory is reachable. The machine-wide known-hosts file still applies,
    because it holds server keys, not logins.
  - **What the host does not use, on purpose.** The operator's own `ssh`
    configuration file, any `ssh` agent, and the default keys (`~/.ssh/id_rsa`,
    `~/.ssh/id_ed25519` and the rest). The only login an `ssh` import ever
    offers is the key you named in `Credential`.
  - **What an operator loses by this.** A host alias or a jump host
    (`ProxyJump`, a way of reaching one server through another) defined in
    their own `~/.ssh/config` will **not** work. The address must name the real
    host, and you need a full URL plus a sealed `ssh_key` credential.
  - **Unknown servers are refused, not trusted on first use.** Add the host to
    `known_hosts` first. An operator does this once, for example by connecting
    to it by hand and confirming the key.
  - **If the home directory cannot be found at all,** the host points at an
    empty file instead, so every `ssh` host fails the check with the same
    refusal an unlisted host gets. It fails closed (it refuses, it never
    relaxes the check).
- **Network reach** to whatever git host you name. See section 6.

`git` is the **one host-side subprocess this SDK runs**, and it runs only to
read a repository into the host. It never touches a fleet node and never reaches
a managed server. The project rule that fleet-facing tools (OpenTofu, `kubectl`,
scanners) run on a **runner node** through the Bolt facet is not being bent:
fetching a control repo is not a fleet-facing action.

## 6. What granting `code:import` really allows

An operator who grants `code:import` is granting the ability to **reach any git
host the pack names**, including one on a **private network**. The host does not
block private address ranges. This is deliberate: a site's own control repo
usually lives on an internal git server, so blocking internal addresses would
make import useless on the deployments it exists for. The protections that do
exist are narrower: only `https` and `ssh` addresses are accepted, a credential
must be named and cannot ride in the address, a fetch with no credential named
cannot borrow the operator's own `ssh` key or agent, the fetch is size- and
time-bounded, and nothing is written without a human's approval. So granting
`code:import` grants network reach to the git hosts the pack names. It does
**not** grant the operator's own git access.

Decide the grant knowing that. If a pack should not be able to probe your
internal network, do not give it `code:import`.

## 7. The bounds

These are fixed in code.

| What | Limit |
|---|---|
| Branches discovered or fetched in one call | 100. The cap is applied to the branches the remote reports, **before any** branch list you pass narrows the work. A repository with more than 100 branches therefore cannot be imported today, even if you name a single branch. This is a known limitation; naming fewer branches is not a way round it. |
| One file | 1 MiB |
| One branch's stored content | 4 MiB |
| One whole import's stored content | 8 MiB (over it, the call is refused and asks for a narrower branch list) |
| Findings kept per branch | 200, then one last note |
| Finding excerpt | 200 bytes |
| Discovering which branches exist | 30 seconds |
| Fetching the branches | 90 seconds |
| The whole `InspectImport` or `ProposeImport` call | 3 minutes |

## 8. What the errors look like

| What happened | What the caller sees |
|---|---|
| The pack lacks `code:rw` or `code:import` | `PermissionDenied` |
| The address is empty, uses `http`, `git`, `file`, a helper form, or carries a credential | `InvalidArgument`, "git url must use https or ssh" or "git url must not embed credentials" |
| A branch you named is not on the remote | `InvalidArgument`, "branch ... was not found on the remote" |
| A branch you selected cannot be imported, or `expected_commits` misses a selected branch, or `proposal_id` is empty | `InvalidArgument` |
| The credential name was never sealed | `NotFound`, "git credential ... is not configured" |
| The sealed value is malformed | `Internal`, naming the credential and the field |
| The remote rejected the login, or needs one, or the repository was not found | `FailedPrecondition`, "git remote rejected the credential or requires one". A missing login and a wrong login give the **same** message, so the caller cannot tell which |
| The ssh host key is not in `known_hosts` | `FailedPrecondition`, "git ssh host key could not be verified against the host's known_hosts" |
| The host cannot be reached or the call timed out | `Unavailable` |
| `git` is missing or too old | `FailedPrecondition`, naming the minimum version |
| The selected branches together are over the 8 MiB ceiling, or none is importable | `FailedPrecondition` |
| A selected branch has moved since the report | `FailedPrecondition`, detail code `import_branch_moved` (`local.IsCodeImportBranchMoved`) |
| `ApplyImport` on a proposal that is pending or rejected | `FailedPrecondition`, "overwrite proposal ... is not approved" |
| `ApplyImport` when an environment appeared that the approver never saw replaced | `FailedPrecondition`, detail code `import_unflagged_collision` (`local.IsCodeImportUnflaggedCollision`); nothing is imported |

Branch on the detail code, not on `FailedPrecondition` alone, because many
refusals share that gRPC code. Error messages never repeat the repository
address or any part of a credential.

## 9. What this does not do

- **It does not deploy.** Import fills the Code facet's model. It does not push
  anything to a Puppet server or a node.
- **It does not write back to the real repository.** It only reads. Changes you
  make in the Code facet afterwards never travel back to git.
- **It does not adopt `r10k.yaml` or `g10k.yaml`.** A deploy-tool config file
  that lives on disk is not read, and the sources it lists are not followed.
- **It does not merge into an existing environment.** An environment of the same
  name is replaced as a whole, never combined item by item.
- **It does not approve part of an import.** One proposal covers every selected
  branch. To skip a branch, leave it out of the list and propose again.
- **It cannot decide its own proposal.** Nothing in the three calls can approve
  or reject, and approval is never taken from a request.

## What the gate does not stop

The overwrite gate guards against accidental replacement through the Code
facet's own calls, and it is not a control against someone who means to get
around it. The limitation that
[`docs/code-overwrite-gating.md`](code-overwrite-gating.md) records in section 6
applies to import too. On `host.Local` the Documents facet has no access
control, so a pack that can also call `Documents.Put` directly can write a
complete approval record into the `code-overwrites` collection, or write the
Code collections themselves, and get around the gate. The same holds for the
marker that makes an approval single-use. `code:import` does not make this
better or worse; it is the sixth gated path, not an exemption and not a
stronger one. If you need protection against a pack that acts maliciously, do
not give that pack direct Documents access in the host that embeds this SDK.

## Manual verification

This section lets a person confirm import by hand. There is one walk for each
of the three RPCs, then two walks for the two things no automated test in this
SDK can do: an authenticated fetch of a real private repository, and an `ssh`
fetch against a real host. You will run a small scratch test and read what it
prints. Nothing here changes the repository; delete the scratch directory at
the end.

Never put a credential in a git address, and never pass one as a command
argument. The implementation refuses the first, and the second would be readable
by anyone listing processes on the machine. The walks below read secrets from
environment variables or a file, and only ever send the **name** of a sealed
secret in a request.

### Setting up

1. From the repository root, create a scratch directory:

   ```
   mkdir manualcheck
   ```

2. Save this as `manualcheck/import_test.go`. It builds a host with the
   permissions import needs, and has helpers to seal a credential and to play
   the approving operator:

   ```go
   package manualcheck

   import (
       "context"
       "encoding/json"
       "os"
       "testing"

       "github.com/puppet-stagehand/stagehand-sdk/approval"
       "github.com/puppet-stagehand/stagehand-sdk/code"
       hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
       "github.com/puppet-stagehand/stagehand-sdk/host"
       "github.com/puppet-stagehand/stagehand-sdk/host/local"
   )

   var ctx = context.Background()

   var kind = approval.Kind{Collection: code.OverwriteCollection, ApproveScope: code.OverwriteApproveScope}

   func newHost() *host.Host {
       return local.New([]string{"code:rw", "code:import", "secrets:rw", "tokens:issue", "documents:rw"}, "manual-check")
   }

   // publicURL is a public control repo that needs no login.
   func publicURL() string {
       if u := os.Getenv("STAGEHAND_TEST_GIT_URL"); u != "" {
           return u
       }
       return "https://github.com/puppetlabs/control-repo.git"
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

   // seal stores a credential as a sealed secret under a name.
   func seal(t *testing.T, h *host.Host, name string, cred map[string]any) {
       t.Helper()
       b, err := json.Marshal(cred)
       if err != nil {
           t.Fatal(err)
       }
       if _, err := h.Secrets.Store(ctx, &hostv1.StoreSecretRequest{Name: name, Plaintext: b}); err != nil {
           t.Fatal(err)
       }
   }
   ```

   Run `go vet ./manualcheck`. Expect no output. Add the walks below as new
   test functions in the same file. Walks 1 to 3 build on each other, so put
   them in one function, `TestPublic`.

### Walk 1: `InspectImport`

**Prerequisite:** the machine running the test has `git` 2.32 or newer and can
reach `github.com` over `https`. No credential and no private infrastructure
are needed. To use another public control repo, set `STAGEHAND_TEST_GIT_URL`.

1. Add to `TestPublic`:

   ```go
   h := newHost()
   rep, err := h.Code.InspectImport(ctx, &hostv1.InspectImportRequest{Url: publicURL()})
   if err != nil {
       t.Fatal(err)
   }
   for _, b := range rep.Snapshot.Branches {
       t.Logf("branch %q commit=%s importable=%v will_overwrite=%v data_files=%d hiera=%d bytes settings=%v",
           b.Branch, b.Commit, b.Importable, b.WillOverwrite, len(b.DataFiles), len(b.HieraYaml), b.Settings != nil)
       for _, f := range b.Findings {
           t.Logf("  %s %s %s:%d: %s", f.Severity, f.Kind, f.File, f.Line, f.Message)
       }
   }
   envs, _ := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{})
   t.Logf("environments after inspect: %v", envs)
   docs, _ := h.Documents.List(ctx, &hostv1.ListDocumentsRequest{Collection: code.OverwriteCollection})
   t.Logf("proposals after inspect: %v", docs)
   ```

2. Run `go test ./manualcheck -run TestPublic -v`.

**Expected:** a line for each branch of the repository. For the default
repository that is one branch, `production`, importable, with a commit hash, no
overwrite, a few data files and settings present. Under it, at least one
finding: a `WARNING puppetfile_forge_directive` on `Puppetfile:1` and a
`WARNING hiera_unmodelled_key` for `plan_hierarchy`. Then
`environments after inspect: page:{}` and `proposals after inspect: page:{}`:
the lists are **empty**, so nothing was written and no proposal was filed. (A
different repository will show different branches and findings; what must hold
is the two empty lists.)

**What this proves:** inspect reports without writing. If either list is not
empty after inspect, stop and report it.

### Walk 2: `ProposeImport`

**Prerequisite:** Walk 1 passed, in the same function so the same host is used.

1. Add to `TestPublic`:

   ```go
   pr, err := h.Code.ProposeImport(ctx, &hostv1.ProposeImportRequest{
       ProposalId: "imp1", Url: publicURL(), Branches: []string{"production"},
       ExpectedCommits: map[string]string{"production": rep.Snapshot.Branches[0].Commit},
   })
   if err != nil {
       t.Fatal(err)
   }
   t.Logf("proposed %s with %d branches, commit %s", pr.ProposalId, len(pr.Snapshot.Branches), pr.Snapshot.Branches[0].Commit)
   ```

   If you imported a repository whose first branch is not the one you want, put
   its name in `Branches` and use that branch's commit from Walk 1.

2. Run the test again.

**Expected:** `proposed imp1 with 1 branches` and the **same commit hash** that
Walk 1 printed for that branch. The proposal is pending and holds the frozen
snapshot. Environments are still empty: propose does not apply. To see it
stored, list the `code-overwrites` collection and find `imp1` with status
`pending`.

Then check the staleness guard: change `ExpectedCommits` to a made-up value
such as `"deadbeef"` and run again with a different proposal id. **Expected:**
`FailedPrecondition` with detail code `import_branch_moved` and a message
saying the branch "is at commit ... but the report the caller read showed
deadbeef". The message never contains the repository address.

**What this proves:** the proposal freezes exactly what the report showed, and a
moved branch is refused instead of being filed.

### Walk 3: `ApplyImport`

**Prerequisite:** Walk 2 passed, same function, the proposal `imp1` still
pending.

1. Add to `TestPublic`:

   ```go
   _, err = h.Code.ApplyImport(ctx, &hostv1.ApplyImportRequest{ProposalId: "imp1"})
   t.Logf("apply before approval: %v", err)

   approve(t, h, "imp1")

   done, err := h.Code.ApplyImport(ctx, &hostv1.ApplyImportRequest{ProposalId: "imp1"})
   if err != nil {
       t.Fatal(err)
   }
   t.Logf("applied: %v", done)
   mods, _ := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "production"})
   t.Logf("modules: %d", len(mods.GetModules()))
   hier, _ := h.Code.GetHieraHierarchy(ctx, &hostv1.GetHieraHierarchyRequest{Environment: "production"})
   t.Logf("hierarchy: %v", hier)
   files, _ := h.Code.ListHieraDataFiles(ctx, &hostv1.ListHieraDataFilesRequest{Environment: "production"})
   t.Logf("data files: %v", files.GetPaths())
   settings, _ := h.Code.GetEnvironmentSettings(ctx, &hostv1.GetEnvironmentSettingsRequest{Environment: "production"})
   t.Logf("settings: %v", settings)

   again, err := h.Code.ApplyImport(ctx, &hostv1.ApplyImportRequest{ProposalId: "imp1"})
   t.Logf("applied again: %v, error: %v", again, err)
   ```

2. Run the test again.

**Expected:**

- `apply before approval:` shows `FailedPrecondition` saying overwrite proposal
  `"imp1"` **is not approved**. This is the refusal half. Confirm that no
  environment exists at this point.
- After `approve`, `applied:` lists the environment `production`.
- `modules:` is the number of `mod` lines the repository actually declares. The
  default public repository has none switched on in its Puppetfile, so expect 0
  there; a repository with active `mod` lines shows how many came in.
- `hierarchy:` shows version 5 and the levels the repository's `hiera.yaml`
  names, for the default repository a level called `YAML backend` with paths
  `nodes/%{trusted.certname}.yaml` and `common.yaml`.
- `data files:` lists the files that came in under the data folder, and
  `settings:` shows the `environment.conf` values (for the default repository a
  `modulepath` and a `config_version`).
- `applied again:` shows the same environment and `error: <nil>`: a repeat after
  a lost reply is harmless.

**What this proves:** the gate refuses before approval, and after approval
every imported resource reads back through the ordinary Code read calls.

### Walk 4: an authenticated `https` fetch of a private repository

**Prerequisite:** a **real private** git repository you can reach over `https`,
and an access token for it that can read it. No automated test in this SDK can
do this walk. It exists because research verified the token flow only against a
reference-listing call under an isolated environment, and could not verify a
real shallow fetch of a private repository. This walk is where that gap closes.
Skip it if you have no such repository; do not half-run it.

1. In your shell, export the settings without putting them on a command line.
   Typing `read -s STAGEHAND_TEST_GIT_TOKEN` and pasting the token keeps it out
   of your shell history, and an environment variable is not part of any
   command line, so it never shows in a process listing's command column:

   ```
   export STAGEHAND_TEST_GIT_URL_PRIVATE="https://git.example.com/platform/control-repo.git"
   export STAGEHAND_TEST_GIT_USER="your-user-name"
   read -s STAGEHAND_TEST_GIT_TOKEN && export STAGEHAND_TEST_GIT_TOKEN
   ```

   The address is the plain address with **no login in it**.

2. Add this test:

   ```go
   func TestPrivate(t *testing.T) {
       url := os.Getenv("STAGEHAND_TEST_GIT_URL_PRIVATE")
       if url == "" {
           t.Skip("set STAGEHAND_TEST_GIT_URL_PRIVATE, _USER and _TOKEN")
       }
       h := newHost()

       // 4a: the right credential, sealed and named.
       seal(t, h, "good", map[string]any{
           "kind": "https_token", "username": os.Getenv("STAGEHAND_TEST_GIT_USER"),
           "token": os.Getenv("STAGEHAND_TEST_GIT_TOKEN")})
       rep, err := h.Code.InspectImport(ctx, &hostv1.InspectImportRequest{Url: url, Credential: "good"})
       if err != nil {
           t.Fatal(err)
       }
       for _, b := range rep.Snapshot.Branches {
           t.Logf("branch %q commit=%s importable=%v", b.Branch, b.Commit, b.Importable)
       }

       // 4b: no credential at all.
       _, err = h.Code.InspectImport(ctx, &hostv1.InspectImportRequest{Url: url})
       t.Logf("no credential: %v", err)

       // 4c: a wrong credential.
       seal(t, h, "wrong", map[string]any{
           "kind": "https_token", "username": os.Getenv("STAGEHAND_TEST_GIT_USER"),
           "token": "this-is-not-a-real-token"})
       _, err = h.Code.InspectImport(ctx, &hostv1.InspectImportRequest{Url: url, Credential: "wrong"})
       t.Logf("wrong credential: %v", err)

       // 4d: a credential name that was never sealed.
       _, err = h.Code.InspectImport(ctx, &hostv1.InspectImportRequest{Url: url, Credential: "never-sealed"})
       t.Logf("unknown credential: %v", err)
   }
   ```

3. Run `go test ./manualcheck -run TestPrivate -v`.

**Expected:**

- 4a: one line per branch of the private repository, with real commit hashes.
  This is the success the walk exists to see.
- 4b: `FailedPrecondition`, "git remote rejected the credential or requires one".
  (Some servers answer a private repository they will not show you with "not
  found" instead; the host then says "git repository was not found or is not
  readable with the supplied credential". Either is a correct refusal.)
- 4c: the **identical** `FailedPrecondition` message. The two failures read the
  same on purpose, so a caller cannot use the difference to probe for access.
- 4d: `NotFound`, `git credential "never-sealed" is not configured`.

Check that the token appears nowhere in the output of the run, and that no
command line shown by `ps` while it runs contains it.

**What this proves:** a named, sealed credential reaches a real authenticated
fetch, a missing or wrong one is refused with one message, and the secret does
not leak. It is also a place to judge by eye whether those messages are clear.

### Walk 5: an `ssh` fetch against a real host

**Prerequisite:** a real git server reachable over `ssh`, an **unencrypted**
private key whose public half the server accepts, and that server's host key
already listed in your `~/.ssh/known_hosts`. For the last step the first host
must also be one that **your own key would normally open** (for example a private
repository your day-to-day login can read), because a host that refuses everyone
proves nothing there. No automated test in this SDK can
do this walk. It exists because no `ssh` server was available to research, so
this walk is the only verification of the `ssh` path beyond the arguments the
host builds. Skip it if you have no such server.

1. Set the settings in your shell. The address is the scp-style form, which
   carries only a login name, never a password:

   ```
   export STAGEHAND_TEST_SSH_URL="git@git.example.com:platform/control-repo.git"
   export STAGEHAND_TEST_SSH_KEY_FILE="$HOME/.ssh/id_stagehand_test"
   export STAGEHAND_TEST_SSH_UNKNOWN_URL="git@host-not-in-known-hosts.example.org:org/repo.git"
   ```

   The key is read from the file by the test; it never goes on a command line.
   Make sure the first host is in `known_hosts` and the third is not (check
   with `ssh-keygen -F host-not-in-known-hosts.example.org`, which should
   print nothing).

2. Add this test:

   ```go
   func TestSSH(t *testing.T) {
       url := os.Getenv("STAGEHAND_TEST_SSH_URL")
       keyFile := os.Getenv("STAGEHAND_TEST_SSH_KEY_FILE")
       if url == "" || keyFile == "" {
           t.Skip("set STAGEHAND_TEST_SSH_URL and STAGEHAND_TEST_SSH_KEY_FILE")
       }
       key, err := os.ReadFile(keyFile)
       if err != nil {
           t.Fatal(err)
       }
       h := newHost()
       seal(t, h, "key", map[string]any{"kind": "ssh_key", "username": "git", "private_key": string(key)})

       // 5a: a host already in known_hosts.
       rep, err := h.Code.InspectImport(ctx, &hostv1.InspectImportRequest{Url: url, Credential: "key"})
       if err != nil {
           t.Fatal(err)
       }
       for _, b := range rep.Snapshot.Branches {
           t.Logf("branch %q commit=%s importable=%v", b.Branch, b.Commit, b.Importable)
       }

       // 5b: a host whose key is not in known_hosts.
       if unknown := os.Getenv("STAGEHAND_TEST_SSH_UNKNOWN_URL"); unknown != "" {
           _, err = h.Code.InspectImport(ctx, &hostv1.InspectImportRequest{Url: unknown, Credential: "key"})
           t.Logf("unknown host: %v", err)
       }

       // 5c: the same host, no credential named. Your own key and agent must
       // not be used, so this has to be refused.
       _, err = h.Code.InspectImport(ctx, &hostv1.InspectImportRequest{Url: url})
       t.Logf("no credential: %v", err)
   }
   ```

3. Run `go test ./manualcheck -run TestSSH -v`.

4. Read the 5c line on its own. It runs the import against the first host with
   the credential name left empty. Your own key is in `~/.ssh` and your `ssh`
   agent may well be running, and the host must ignore both.

**Expected:**

- 5a: one line per branch of the repository, with real commit hashes.
- 5b: `FailedPrecondition`, "git ssh host key could not be verified against the
  host's known_hosts". The host is **not** trusted on first use and nothing is
  added to your `known_hosts`.
- 5c: `FailedPrecondition`, "git remote rejected the credential or requires
  one" (or "git repository was not found or is not readable with the supplied
  credential", which is the same refusal worded for servers that hide private
  repositories). **No branches are listed.** If 5c lists branches, the fetch fell back to
  your own key or agent: stop and report it as a bug.

**What this proves:** an `ssh` key reaches a real host under strict host-key
checking, an unknown host fails rather than being trusted, and a fetch with no
credential named cannot fall back to your own key, your `ssh` agent or your
`ssh` configuration.

### One more read, by a human

The finding message for a branch name that breaks the environment rule is the one
message in this feature written for an operator to read. Against any repository
with a branch such as `feature/x`, run an inspect and read the `branch_name_invalid`
finding. It should say, in plain words, that the name does not match
`^[a-z0-9_]+$`, that the branch is not imported, that real r10k would deploy it
under a corrected name, and that the fix is to push it under a valid name. If a
newcomer could not follow it, report that: whether it reads clearly is a
judgment no test can make.

### Clean up

Delete the scratch directory with `rm -r manualcheck`, unset the environment
variables you exported (for example `unset STAGEHAND_TEST_GIT_TOKEN`), and
confirm `git status` shows nothing new.

If Walk 1 left anything in the environment or proposal lists, or Walk 3 applied
before approval, the gate is broken: stop and report it, because content went in
without an approved proposal.
