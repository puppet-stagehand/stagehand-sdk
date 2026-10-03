# Control Repo Authoring — a worked proof, not a real pack

This is a proof example. It is not an Expansion Pack you could install, and
it does not listen on a network. What it shows is that the Code facet
(environments, Puppetfile modules, Hiera, environment settings, import), the
Forge facet (search, dependency check, recommend) and the `approval` package
(propose a change, have a human approve it, only then apply it) genuinely fit
together into a real control-repo authoring workflow. It runs entirely
against `host.Local` — the in-process test double that stands in for the real
console during development — with in-memory fixtures: no network, no `git`
binary, and no language-model provider.

If you are writing a pack that authors Puppet code and "a human must say yes
before anything is replaced," copy the shapes here into your own pack.

## The story it tells

Read `examples/control-repo-authoring/control_repo_authoring_test.go` for the
full run (`TestControlRepoAuthoring_EndToEnd`). In plain language, in order:

1. Create two blank environments (`CreateEnvironment`).
2. Describe what you need in a sentence and get ranked suggestions of real
   modules (`RecommendModules`).
3. Check the top suggestion against the registry, and see that a look-alike
   module is marked deprecated (`SearchModules`).
4. Look at the module's dependency tree (`ResolveModule`).
5. Write the module into the environment's Puppetfile, then the one
   dependency you accept (`AddModule`).
6. Add a Hiera level and a data key (`AuthorHieraLevel`,
   `AuthorHieraDataKey`).
7. Ask a person to approve the environment's settings (`ProposeSettings`),
   then, once they have, apply them (`ApplySettings`). A person holding the
   right token says yes (`Approve`).
8. Read everything back (`ListEnvironments`, `RenderPuppetfile`, `Settings`,
   `Hierarchy`, `DataFile`).
9. Adopt an existing control repo from a git address, using a credential
   referenced by name (`InspectImport`).
10. Read the per-branch report and see which environment would be replaced,
    ask for approval, and only then apply it (`ProposeImport`, `Approve`,
    `ApplyImport`).
11. Bump one module that is already imported, through the same ask-first
    loop (`ProposeModuleOverwrite`, `Approve`, `ApplyModuleOverwrite`).

The fixture environments are named `authored` and `canary`; the import
brings in a branch that replaces `canary` and a new one called `qa_two`, and
a third branch, `feature-spike`, is refused by name and never opened. The
fixture git address is `https://git.example.test/org/control-repo.git` and the
credential is called `control-repo-login`. If you run the test with `-v`, it
prints this story as a numbered transcript.

## The two halves, and why there are two

The example is split into two types, not one type with two methods:
`ProposerBackend` (built with `NewProposer`) and `ApproverBackend` (built with
`NewApprover`).

`ProposerBackend` holds every step that authors, proposes or applies.
`ApproverBackend` holds only the two steps that decide: `Approve` and
`Reject`.

The one sentence that explains the split: the code that proposes a change must
not be able to reach the code that approves it, or nobody was actually asked.
`ApproverBackend.Approve` takes the approval token's secret as an argument, and
nothing in this package can produce that secret — the token is minted only
inside the test file's `approverToken` helper, standing in for an operator who
obtained it out of band, away from any code the pack itself runs.

`TestControlRepoAuthoring_ProposerCannotSelfApprove` is the test that checks
this claim by reading this package's own source, not by trusting a code
review: it parses the Go files in this directory only, and fails if anything
reachable from `ProposerBackend` could ever call into approving or
token-minting. If a future change ever puts the two halves in one call graph,
this test is what catches it. See `docs/approval-pattern.md` for the rule this
test enforces, in full.

## What the manifest declares

`examples/control-repo-authoring/manifest.json` declares six permissions, and
each buys exactly one thing:

- `code:rw` — read and write environments, Puppetfile modules, Hiera and
  environment settings.
- `code:import` — read an existing control repo from a git address and propose
  importing it. It is separate from `code:rw` because it reaches a git host
  outside the console.
- `forge:rw` — search the registry and resolve a module's dependency tree.
- `forge:recommend` — ask for ranked module suggestions from a sentence. It is
  separate because it sends text to a language-model provider.
- `secrets:rw` — the sealed credential for the git remote and the provider's
  API key live in the Secrets facet, never in the manifest or the image.
- `tokens:issue` — the manifest's approve and reject routes carry an access
  scope, and a route that carries a scope needs this permission.

The vocabulary point a pack author must get right: `code:approve` is **not** a
permission. It is a route's access scope — it gates one decision, and arrives as
a short-lived token, not a standing grant. A pack can never grant itself the
authority to decide its own proposals. `tokens:issue` is present only because
those two routes carry that scope.

## How to run it

The example's own tests:

```
go test ./examples/control-repo-authoring/... -race -v
```

You should see nine tests pass:
`TestControlRepoAuthoring_EndToEnd` (the whole story above),
`TestControlRepoAuthoring_PendingImportDoesNotApply` (an import nobody has
approved changes nothing),
`TestControlRepoAuthoring_RejectedOverwriteDoesNotApply` (a rejected overwrite
does not apply, a rejection must give a reason, and a decision is final),
`TestControlRepoAuthoring_ReplayRefused` (one approval covers one application
only),
`TestControlRepoAuthoring_RecommendIsGrounded` (a module the registry did not
return is dropped, not suggested),
`TestControlRepoAuthoring_ManifestDeclaresExactly` (the manifest's permissions
and routes match what the code uses, with no headroom),
`TestControlRepoAuthoring_CodeKindIsPinned` (the approval collection and scope
this README and both guides quote match the code),
`TestControlRepoAuthoring_ProposerCannotSelfApprove` (the two halves share no
call path), and
`TestControlRepoAuthoring_ReadmeStatesTheBoundary` (this file still says what
the last section says).

The manifest fixture:

```
go run ./cmd/pack-check --format json examples/control-repo-authoring/manifest.json
```

You should see `"ok": true` and no findings.

## What it does not do

- It **does not deploy**. Nothing here runs r10k or g10k, and no environment
  it authors is ever sent to a Puppet server. It is author-only, which is this
  milestone's scope boundary.
- It **does not write back** to a real control repo. Nothing here pushes a
  commit, opens a pull request, or otherwise changes the repository it read
  from. Every byte it authors lives in `host.Local`'s in-memory Code facet and is
  read back from there.
- No network listener — the routes are declared in the manifest, but nothing
  listens on them. This is a Go package, not a running worker.
- No OpenAPI fragment, because `pack-build` does not exist yet in Preview
  0.0.1; `manifest.json` still declares its four routes so the fixture is
  complete on its own terms.
- Environment settings are gated on every call after the environment is
  created. There is no ungated way to author them: a proposal, an approval and
  an apply, every time.
- An import that collides with an existing environment replaces that
  environment as a whole. It does not merge item by item, which is why the
  story keeps `authored` out of the import's way.
- The registry, the language model and the git remote are all fixtures, so the
  suggestions and the module versions are not real advice.
- It is not an installable pack, so it ships no `USER-GUIDE.md` or
  `TESTER-GUIDE.md` of its own. For the walk-through, read
  [`docs/control-repo-authoring.md`](../../docs/control-repo-authoring.md); for
  the checks a person makes by hand, read
  [`docs/control-repo-authoring-testing.md`](../../docs/control-repo-authoring-testing.md).
