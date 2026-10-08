# Authoring a control repo from start to finish: ask first, replace second (ELI10)

This guide is for a pack author who is new to Puppet and new to this SDK. It
is the end-to-end worked proof of authoring a control repo. It shows how the
Code facet, the Forge facet, the suggestion step and the ask-a-human gate fit
together in one workflow, and it points at the runnable proof in
[`examples/control-repo-authoring`](../examples/control-repo-authoring/README.md).

> Nothing that replaces content a person already has is written until that
> person has read exactly what would change and said yes.

Everything in this guide runs against `host.Local`, the in-process stand-in for
the real console. The registry, the language model and the git remote in the
example are all fixtures, so every module name and version you read below is
made up for the proof and is not advice.

## Some words first

- **Control repo** - the one git repository that holds all of a site's Puppet
  code. The Code facet stores a working copy of it as structured data.
- **Environment** - one named copy of the code, such as `production` or `dev`.
  Nodes are told which environment to use.
- **Puppetfile** - a plain text list of the modules an environment needs, with
  the version of each.
- **Hiera** - Puppet's lookup system for settings that differ per node or per
  group. A **hierarchy** is an ordered list of **levels**; each level says which
  data file to look in. A **data key** is one named value in a data file.
- **`environment.conf`** and **settings** - `environment.conf` is a small
  settings file each environment can have. The Code facet calls its contents the
  environment **settings**.
- **Moduledir** - the folder inside an environment where r10k or g10k puts the
  modules from the Puppetfile. Most people leave it as `modules`. Think of it as
  the shelf the modules are unpacked onto.
- **Proposal** - a written request that says exactly what you want to replace
  and what you want to replace it with. It holds a frozen copy of the change.
- **Approval scope** - a label on a short-lived token. The scope `code:approve`
  says the holder may say yes or no to proposals about code. It is not a
  permission and never goes in a manifest's `permissions`.
- **Registry** - a shop for Puppet modules. The public one is the Puppet Forge,
  and its source name in this SDK is `puppet-forge`. Each registry you can
  search is a **source**.
- **Suggestion** - one ranked module the Forge facet's `Recommend` call hands
  back for a sentence you wrote. Every suggestion is a copy of a real registry
  search result.
- **Import** - reading an existing control repo from a git address and turning
  each branch into an environment. Real Puppet tools make one environment per
  branch, and import follows that idea.
- **Credential** - in this guide, the *name* of a sealed secret that holds a git
  login. You pass the name, never the login itself.
- **The two personas** - the example is split into two types.
  `ProposerBackend` authors, proposes and applies. `ApproverBackend` only
  decides. They share no call path, so the code that asks cannot also say yes.

## 1. Two blank environments

Start with two empty environments. Keep the environment you will author by hand
apart from the one the import will land on, because an import replaces an
environment of the same name as a whole (section 7).

```go
for _, name := range []string{"authored", "canary"} {
    env, err := proposer.CreateEnvironment(ctx, name)
    // ...
}
```

Creating an environment is free: it replaces nothing. In the example,
`authored` is where the hand-written content goes, and `canary` is the
environment the import will replace later.

## 2. Describe a need, get ranked real modules

Write one sentence about what you need. `RecommendModules` hands it to the Forge
facet's `Recommend` call.

```go
rec, err := proposer.RecommendModules(ctx, "keep the clocks on my servers in sync", w.provider)
top := rec.Suggestions[0] // rank 1: puppetlabs/ntp, from puppet-forge, with reasoning
```

The suggestion step calls a language model that you configure and name; the
provider has no default. The model only proposes an order and says why; the host
moves deprecated modules to the end. The results it orders come from a real registry search, so a module name the model
invents is dropped with a warning (`recommend_unknown_module_dropped`) rather
than shown. Your provider's API key lives in the Secrets facet and reaches the
model client, never a prompt. [`docs/forge-recommend.md`](forge-recommend.md)
covers the call in full.

## 3. Check the suggestion: search it, then read its dependency tree

Do not trust a suggestion until you have checked it yourself. First search the
registry for the name. Searching is read-only.

```go
found, err := proposer.SearchModules(ctx, top.Module.Name, top.Module.Source)
broad, err := proposer.SearchModules(ctx, "ntp", top.Module.Source)
```

The broader search shows a look-alike next to the real module. In the example,
`example/timekeeper` is marked deprecated and superseded by `puppetlabs/ntp`,
and the registry's own fields are how you tell them apart.

Then read the dependency tree:

```go
tree, err := proposer.ResolveModule(ctx, top.Module.Name, "13.2.1", "authored")
```

The tree says `puppetlabs/ntp` 13.2.1 needs `puppetlabs/stdlib`, and that
`stdlib` is not in the Puppetfile yet. Read the tree as advice, not as a preview
of a real deploy. The real deploy tool resolves no dependencies of its own, so
the tree tells you what you should add, and it does not tell you what will
happen on a node. `ResolveModule` also never writes a Puppetfile.

## 4. Write the module and the dependencies you accept

Each module you want needs its own explicit write. Adding a name that is not in
the Puppetfile yet is free.

```go
mod, err := proposer.AddModule(ctx, "authored", &hostv1.PuppetfileModule{
    Name:   n.Name,
    Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: n.Version}},
})
```

The example writes `puppetlabs/ntp` first, then the one dependency it accepts,
`puppetlabs/stdlib`. You can use each name as the resolver returned it.

### What counts as the same module

A Forge module can be written two ways: `puppetlabs/stdlib` (with a slash) or
`puppetlabs-stdlib` (with a hyphen). The capital letters in the owner's name do
not matter either, so `PuppetLabs/stdlib` is the same module too. Think of it
like a street address: "12 High St" and "12 high street" are one house. The
Code facet treats every one of those spellings as **one module**.

That has two good effects. You cannot accidentally add a second copy of a module
that is already there, and you do not have to match the resolver's spelling
character for character. A module you add is written into the Puppetfile in the
lowercase hyphen form, so `puppetlabs/ntp` is stored as `puppetlabs-ntp`. This
matters in section 8.

### What a module value may contain

The Code facet checks every value it is about to write into the Puppetfile. The
rules are short, and you can satisfy them without reading any Go:

- **A Git module's name** is a single bare word. It may use letters (accented
  ones too), digits, underscore, dot and hyphen. It may not start with a dot or
  a hyphen, and it never contains a slash.
- **A pinned Forge version** may use letters, digits, dot, plus, underscore and
  hyphen. So `1.0`, `1.0.0` and `1.2.3-rc.1+build.5` all work, but a range such
  as `>= 1.0` does not. That is on purpose: r10k itself only understands a
  single pinned version or the latest one.
- **A `ref`, `tag`, `branch`, `commit` or `default_branch`** may be almost
  anything readable, but it may not contain a space or start with a hyphen.
- **No value anywhere** may contain a quote mark, a backslash, a line break or
  an invisible control character. Invisible formatting characters are refused
  too, such as a zero-width space or a right-to-left override, because they can
  make a value read as something other than what it is.

Why so strict? The Puppetfile is a Ruby file, and the facet writes your value
into it between quote marks. A value that contains a quote mark or a line break
could close the quote early and turn into extra lines in the file: a module
nobody asked for, or a replacement of one already there, that no reviewer ever
saw.

The rule to remember is that **the facet refuses such a value; it never tries to
escape it.** An escaped value would read back as something different from what
was written, and the facet's promise is that what it writes is what it reads. So
if a write is refused, change the value; do not look for a way to quote it.

The module directory setting follows the same rule and one stricter one: it
must be a single plain folder name. See "Why the module folder must be one plain
name" below.

## 5. A Hiera level and a data key

Add one level, then one key in the data file that level reads.

```go
put, err := proposer.AuthorHieraLevel(ctx, "authored", &hostv1.HieraLevel{
    Name: "common", Path: "common.yaml", Datadir: "hieradata",
})
df, err := proposer.AuthorHieraDataKey(ctx, "authored", "common.yaml", "profile::ntp::servers", "ntp1.example.test")
```

The level's data directory (`hieradata`) and the data file's path
(`common.yaml`) must agree: the path is read relative to the data directory.
`AuthorHieraLevel` sets the insert flag because a first level needs it. Replacing
a level that already exists is an overwrite, and it goes through the gate
instead.

## 6. Settings: the step that surprises everyone

Once an environment exists, writing its settings always goes through the gate.
You propose, a person approves, then you apply. There is no ungated way to
author them, and no setting turns that off.

```go
proposal, err := proposer.ProposeSettings(ctx, "settings-authored-1", &hostv1.EnvironmentSettings{
    Environment:        "authored",
    ConfigVersion:      ptr("scripts/config_version.sh"),
    EnvironmentTimeout: ptr("5m"),
})
// status: pending. A person holding a code:approve token decides:
approved, err := approver.Approve(ctx, "settings-authored-1", secret)
// then, straight after, on the proposing side:
applied, err := proposer.ApplySettings(ctx, "settings-authored-1")
```

Settings are gated on every call because `environment.conf` has no single "new"
item to add. A settings write always replaces the whole record. The apply call
takes only the proposal id and writes exactly what was frozen, so it cannot
write something the approver did not see. Settings you did not write stay unset;
they do not turn into Puppet defaults.

The token's secret reaches `Approve` from outside the proposing code. In the
example a helper in the test file mints it, standing in for an operator who got
it out of band. See [`docs/approval-pattern.md`](approval-pattern.md) for why
that separation is the whole point, and
[`docs/code-overwrite-gating.md`](code-overwrite-gating.md) for the gate itself.

## 7. Adopt an existing repository

Importing has a report step at the front, and nothing is written until after a
person has approved. [`docs/code-import.md`](code-import.md) covers it in full.

1. **Seal a credential once, and pass only its name.** The address must never
   carry a token or a password. The example's credential is called
   `control-repo-login`, and the fixture address is
   `https://git.example.test/org/control-repo.git`.
2. **Read the report.** It writes nothing.

   ```go
   snap, err := proposer.InspectImport(ctx, controlRepoURL, "control-repo-login")
   ```

   The report lists each branch: the commit that was read, whether it is
   importable, and whether it `will_overwrite` an environment of the same name.
   Here `canary` will overwrite the blank `canary` environment, `qa_two` is new,
   and `feature-spike` is refused by name because a branch name must match
   `[a-z0-9_]` to become an environment. It is never opened.
3. **Propose, pinned to what the report showed.**

   ```go
   proposed, err := proposer.ProposeImport(ctx, "import-adopt-1", controlRepoURL, credential,
       []string{"canary", "qa_two"},
       map[string]string{"canary": report["canary"].Commit, "qa_two": report["qa_two"].Commit})
   ```

   The commit map is optional, but when you pass it, it must name every
   selected branch. If a branch moved after the report, the call is refused.
4. **A pending import writes nothing.** `ApplyImport` now fails, and no
   environment changes.
5. **Get it approved, then apply.** `Approve` by a person, then
   `ApplyImport(ctx, "import-adopt-1")`. Apply makes no network call and needs
   no credential. It writes the frozen snapshot only.

An environment whose name matches an imported branch is replaced as a whole.
Its Puppetfile, hierarchy, data files and settings are all swapped for what was
frozen. That is why the example authors in `authored` and imports onto `canary`:
after the import, `authored` is untouched, `canary` holds only the imported
content, and `qa_two` exists because the import created it.

## 8. Change one module that is already there

Say `canary` now holds `puppetlabs-stdlib` at 9.4.1 and you want 9.6.0. The
import left the hyphenated spelling in place. Whichever way you spell it,
`puppetlabs/stdlib`, `puppetlabs-stdlib` or `PuppetLabs/stdlib`, it is the same
module, so a write that would replace it is refused until it is proposed and
approved. It cannot sneak past the gate as a second, ungated copy. The example
checks all three spellings and then walks the ladder using the slash spelling.

1. **The write is refused.** `AddModule` over the existing module fails with
   `FailedPrecondition` and the detail `code_overwrite_requires_approval`. The
   module is still at 9.4.1.
2. **Propose exactly that change.**

   ```go
   bumpProposal, err := proposer.ProposeModuleOverwrite(ctx, "bump-stdlib-1", "canary", bumped)
   ```

3. **A pending proposal is not an approval.** The write is still refused as
   requiring approval.
4. **A person approves.** Now the refusal changes shape to
   `code_overwrite_apply_pending`: an approval exists, but a plain write never
   applies one. The content has still not changed.
5. **A separate call applies it.**

   ```go
   appliedMod, err := proposer.ApplyModuleOverwrite(ctx, "bump-stdlib-1")
   ```

   `canary` now holds exactly one module, `puppetlabs-stdlib` at 9.6.0. An
   approval covers one application. Repeating the apply on an unchanged target
   changes nothing and is allowed. Once the target has moved by some other
   route, replaying the old approval is refused instead of quietly undoing the
   newer change.

## 9. Read it all back

Use the ordinary read calls to see what is really there: `ListEnvironments`,
`RenderPuppetfile`, `ListModules`, `Settings`, `Hierarchy` and `DataFile`.

```go
text, err := proposer.RenderPuppetfile(ctx, "authored")
// mod 'puppetlabs-ntp', '13.2.1'
// mod 'puppetlabs-stdlib', '9.6.0'
```

Notice the spelling. A module you added through the facet is stored in the
canonical lowercase hyphen form, which is why `puppetlabs/ntp` reads back as
`puppetlabs-ntp`. A Puppetfile you imported is different: the import keeps each
module name exactly as it was written, and never rewrites your file's spelling.
Either way, matching still treats the two spellings as one module.

## Why the module folder must be one plain name

The deploy tools, r10k and g10k, do two things with the module folder named in a
Puppetfile. They install modules into it, and they clean it up by deleting
anything in it that the Puppetfile does not list. That is fine for a folder
called `modules` inside the environment. It is dangerous if the setting names a
folder somewhere else: `/srv/modules` or `../up` can point outside the
environment, and the tool would then delete files there that have nothing to do
with Puppet.

So the facet accepts only one plain folder name: letters, digits, `_`, `.` and
`-`, at most 64 characters, and not starting with `.` or `-`. `modules` and
`thirdparty` are fine. A slash, `..`, a leading dot or a leading dash is
refused with the error code `moduledir_invalid`, and the error's fix line says
what to do. The facet never "tidies" a bad value for you, because a value it
changed would not be the value you meant.

If a write is refused, pick a plain name. If an environment already stores a
value that is no longer allowed (for example one stored before this rule), other
Puppetfile writes on that environment are refused with the same code until you
clear it. Call `SetModuledir` with an empty value, which removes the setting, or
set a plain name, and writes work again. Clearing is never refused.

## Why some notes are locked

The Documents facet is a plain notebook every pack can write in. The facets
keep their own working notes in it too: the Code facet's environments, the
overwrite proposals, the Inventory proposals, and the provider and source lists.
If a pack could edit those notes, it could rewrite the facet's records, or write
"approved" on a proposal and approve itself.

So the notebook now has locked pages. A pack may read them but not write or
delete them. The locked names start with `code-`, `deploy-`, `bolt-` or
`inventory-`, and the two names `forge-sources` and `llm-providers` are locked
too. A write to one is refused with the error code `collection_reserved`, and
the fix line lists every locked name. If your pack used one of those names for
its own notes, rename its collection.

Approvals have one more lock. Saying "approved" on a proposal needs the
approver's key, the short-lived token from the person deciding, sent along with
the write. A pack that only holds the notebook has no such key, so it cannot
approve its own proposal by editing the note. A decided proposal is also frozen:
it cannot be edited afterwards. Writing a fresh *pending* proposal is still an
ordinary write. Your own collections, such as `state` or `locks`, are not
locked. `docs/approval-pattern.md` has the technical detail.

## What the errors look like

Branch on the exported helper, never on the message. Several of these share one
status code, so the helper is the only honest way to tell them apart.

| What happened | Status code | Structured code | Helper that recognises it |
|---|---|---|---|
| A write over existing content, with no approved proposal | `FailedPrecondition` | `code_overwrite_requires_approval` | `local.IsCodeOverwriteRequiresApproval` |
| The same write while a proposal exists but is only pending | `FailedPrecondition` | `code_overwrite_requires_approval` | `local.IsCodeOverwriteRequiresApproval` (and `local.IsCodeOverwriteApplyPending` is false) |
| A write whose proposal is approved but not yet applied | `FailedPrecondition` | `code_overwrite_apply_pending` | `local.IsCodeOverwriteApplyPending` |
| An apply against a proposal that is pending or rejected | `FailedPrecondition` | none | none; only the status code is meaningful |
| An apply after the target changed since the approval | `FailedPrecondition` | none | none; only the status code is meaningful |
| An import whose target environment appeared after the report was frozen | `FailedPrecondition` | `import_unflagged_collision` | `local.IsCodeImportUnflaggedCollision` |
| An import whose branch moved since the report | `FailedPrecondition` | `import_branch_moved` | `local.IsCodeImportBranchMoved` |
| A proposal decided a second time | `FailedPrecondition` | `proposal_already_decided` | `approval.IsAlreadyDecided` |
| A rejection with no reason | `InvalidArgument` | `reject_reason_required` | none exported; use the status code |
| A write whose module value cannot be stored safely (a name, version, url or ref with a quote, a backslash, a line break or a control character) | `InvalidArgument` | none | none; only the status code is meaningful |
| A module directory that is not one plain folder name (`/srv/modules`, `../up`, `modules/third`, `.hidden`) | `InvalidArgument` | `moduledir_invalid` | `local.IsModuledirInvalid` |
| A proposal id that is already used | `AlreadyExists` | `proposal_already_exists` | none exported; use the status code |

Never match an error message as a string. Eight of these twelve rows share
`FailedPrecondition`, so the helper is the only honest discriminator. For the
rows with no helper, check the status code and then check that nothing was
written.

## What this does not do

- **It does not deploy.** Nothing here runs r10k or g10k, and no environment it
  authors is ever sent to a Puppet server or a node. It is author-only, which is
  this milestone's scope boundary.
- **It does not write back to the real repository.** Import only reads. Nothing
  here pushes a commit or opens a pull request, and every byte the example
  authors lives in `host.Local`'s in-memory Code facet.
- **It does not merge an import item by item.** An environment whose name
  matches an imported branch is replaced as a whole, never combined.
- **It is not an installable pack, and nothing listens on its routes.** The
  manifest declares four routes so the fixture is complete, but this is a Go
  package, not a running worker.
- **It does not stop a caller who means to get around the gate.** The gate
  guards against accidental replacement through the Code facet's own calls.
  Section 6 of [`docs/code-overwrite-gating.md`](code-overwrite-gating.md) and
  the closing section of [`docs/code-import.md`](code-import.md) say exactly
  what it does not stop. Two routes are now closed: smuggling extra lines in
  through a module value, and a pack that holds only the Documents facet writing
  an approval or a facet's records itself; the route that remains is the documented one
  of removing an item and putting it back (section 6 of the gating guide).

## Where to go next

- [`examples/control-repo-authoring/README.md`](../examples/control-repo-authoring/README.md)
  is the short tour of the example.
- [`docs/control-repo-authoring-testing.md`](control-repo-authoring-testing.md)
  lists the checks a person makes by hand.
- [`docs/pack-author-guide.md`](pack-author-guide.md) is where to start if you
  are making a pack of your own.
