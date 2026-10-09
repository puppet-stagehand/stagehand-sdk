# The real-tool harness

## What this is

Most of this SDK is checked against pretend things: an in-memory host, a fake
git remote, a fake language model. That is fast and safe, but a pretend tool
cannot tell you how the real tool behaves when something goes wrong.

The harness fixes that for the deploy features. It starts two small machines in
containers on your computer:

- a pretend **Puppet server**, which is the real Puppet Server 9 program, and
- a pretend **runner machine**, which has the real deploy tools (r10k, g10k and
  Bolt) installed.

Then it runs the real tools on purpose in good ways and bad ways, writes down
exactly what they did, and keeps those notes in git as **fixtures**. Later
phases of the SDK are built from what the notes say, not from what we guessed.

The harness never touches a real Puppet server, a real fleet or any real
secret. Everything lives in containers that are deleted when the run ends.

## Some words first

- **Environment** - one named copy of the Puppet code, such as `production`.
  Nodes are told which environment to use.
- **Puppet Server** - the program that hands finished instructions (a
  **catalog**) to each machine. It keeps a copy of each environment's code in
  memory.
- **Compiler** - the part of Puppet Server that turns your code into a catalog
  for one machine.
- **Environment cache** - what Puppet Server remembers about an environment's
  code. After new code is put on disk, the cache must be flushed (emptied) or
  the server keeps using the old code.
- **Runner** - the machine that runs fleet-facing tools for the SDK. The SDK's
  own worker never runs them.
- **r10k** and **g10k** - two programs that read a **Puppetfile** and put the
  right code and modules into each environment's directory. g10k is a faster
  rewrite of r10k.
- **Bolt** - a program that runs a small script (a **task**) on one or more
  machines and reports what happened for each machine.
- **Deploy marker** - a small hidden JSON file (`.r10k-deploy.json` or
  `.g10k-deploy.json`) the deploy tool leaves inside an environment to say which
  commit it put there and whether that worked.
- **Fixture** - a JSON file in `harness/fixtures/` that records one real run:
  the exit code, the marker fields, the key lines of output and a few extra
  facts. A fixture is a recording, not a guess.

## Run it

Everything goes through one command, from the repository root:

```
./harness/run.sh
```

That starts the containers, runs the harness tests in **compare mode** (the
tools are run again and the result must match the committed fixtures), and
removes the containers and volumes when it ends. Running it twice in a row must
pass both times and must change nothing under `harness/fixtures/`.

Other forms:

```
HARNESS_RECORD=1 ./harness/run.sh                 # re-record the fixtures from the real tools
./harness/run.sh -run TestHarnessBolt             # run one area; any go test flag works
```

Re-recording rewrites fixtures. Read the `git diff` afterwards: a changed
exit code or marker field means a tool behaved differently, and that is exactly
the news this harness exists to deliver.

The ordinary `go test ./...` never runs the harness. Every harness test file
carries the `harness` build tag, so the default test run starts no container and
needs no registry. Only `./harness/run.sh` (which passes `-tags harness`)
runs them. The small fixture checker in `harness/fixtures_test.go` has no tag;
it only reads the JSON files and runs everywhere.

## Before your first run

1. Copy `harness/.env.example` to `harness/.env` and fill in two values: your
   registry host and your registry project. Values only, never a username,
   password or token. The file is gitignored, and no registry host is written
   anywhere in the repository.
2. Log in to your registry so Docker can pull the images:
   `docker login <your-registry-host>`. You type the password yourself.
3. Check `harness/images.lock`. It lists the image tags, the platforms, the
   image digests and the Puppet Server version. `run.sh` pulls both images by
   digest. If a digest is empty, the image was never pushed (see below).

You need Docker with Compose v2 and Go (the version in `go.mod`).

## Building and pushing the images (people only)

The runner image holds r10k, g10k and Bolt and has no secrets in it. The Puppet
Server 9 image is built from Puppet Core packages, which need a licence
credential. Because of that:

1. Export your Puppet Core credential for the build only:
   `PUPPET_CORE_USER` and `PUPPET_CORE_API_KEY`.
2. `./harness/images.sh build server` (and `build runner` if needed). The
   credential reaches the build only as a BuildKit secret, so it is never left
   in an image layer.
3. Check no key is in the image (human test H4 below). Do this before the next
   step, because the check needs the key still set in your shell.
4. `unset PUPPET_CORE_USER PUPPET_CORE_API_KEY` as soon as the check is done.
5. `./harness/images.sh push --confirm` pushes both images and writes the
   digests to `harness/images.lock`.

The image holds licensed Puppet Core binaries, so the registry project should
normally be private. The project this repository was built against was made
public by the owner's explicit decision, on a private network; that decision is
recorded in the Phase 13 notes, and you should make your own choice for your
own registry (human test H6 checks it).

An AI agent never does any of this (D-18). It has no Puppet Core credential, it
never runs `docker login`, and it never runs `images.sh build` or `images.sh
push`. Only people do.

## Puppet Server 9 on an Apple-silicon Mac

`harness/images.lock` records the result of trying Puppet Server 9 on arm64:

- `SERVER_ARM64_NATIVE=true` means the Puppet Server 9 image runs natively on an
  arm64 machine (Apple silicon), with no emulation. It reached `running` in
  about 16 seconds natively, against about 50 seconds when emulated as amd64.
- `SERVER_PLATFORMS=linux/arm64,linux/amd64` means the pushed image has both an
  arm64 and an amd64 variant. The Puppet Server version is
  `PUPPETSERVER_VERSION=9.0.17`.
- `RUNNER_PLATFORMS=linux/amd64,linux/arm64` means the runner image is also
  published for both.

`run.sh` picks the platform that matches your shell's `DOCKER_DEFAULT_PLATFORM`
when the image has it, otherwise your machine's own platform.

A note on honesty: if your shell exports `DOCKER_DEFAULT_PLATFORM=linux/amd64`,
everything runs as amd64 under emulation, even on a Mac. All fixtures committed
so far were recorded that way, so they match the amd64 CI runner. The Puppet
Server 9 arm64 variant was started and seen healthy on its own, but the
whole harness has not been recorded on arm64 and compared with the amd64
recording (see "What was not run").

## What the evidence says

Each row below is something the real tools did, with the fixture that proves it.
Where the real tool disagreed with what research expected, the fixture keeps the
real behaviour.

| Finding | What happened | Fixture |
|---------|---------------|---------|
| g10k `-dryrun` is not read-only | An out-of-sync dry run exits 1, rewrites the live environment's files and its marker (marker now says the target commit, `deploy_success` true); a second dry run then exits 0. Even an in-sync dry run rewrites the marker's `finished_at`. | [dryrun-out-of-sync](../harness/fixtures/g10k-0.10.0/dryrun-out-of-sync.json), [dryrun-in-sync](../harness/fixtures/g10k-0.10.0/dryrun-in-sync.json), [dryrun-second-run](../harness/fixtures/g10k-0.10.0/dryrun-second-run.json) |
| Exit codes can hide failure | g10k exits 0 when the remote is unreachable (it only prints a warning) and 0 for an unknown environment; r10k exits 1 for both. | [g10k unreachable-remote](../harness/fixtures/g10k-0.10.0/unreachable-remote.json), [g10k unknown-environment](../harness/fixtures/g10k-0.10.0/unknown-environment.json), [r10k unreachable-remote](../harness/fixtures/r10k-5.0.3/unreachable-remote.json) |
| Markers after a failure mislead | After a bad module ref r10k leaves its checkout moved with the marker unchanged; g10k writes a new signature with `deploy_success` false. When r10k deploys everything and one environment fails, a sibling's marker can be set to `deploy_success: false` while that sibling's files are intact, depending on the order. | [r10k bad-module-ref](../harness/fixtures/r10k-5.0.3/bad-module-ref.json), [g10k bad-module-ref](../harness/fixtures/g10k-0.10.0/bad-module-ref.json), [r10k deploy-all-partial](../harness/fixtures/r10k-5.0.3/deploy-all-partial.json), [g10k deploy-all-partial](../harness/fixtures/g10k-0.10.0/deploy-all-partial.json) |
| A hostile `moduledir` escapes | r10k with an absolute or relative `moduledir` installs modules outside the environment and purges what it finds there; with `.` the module lands in the environment root; g10k escapes with a relative value but not an absolute one. This is why the SDK limits `moduledir` to one plain segment. | [r10k absolute](../harness/fixtures/r10k-5.0.3/hostile-moduledir-absolute.json), [r10k relative](../harness/fixtures/r10k-5.0.3/hostile-moduledir-relative.json), [r10k dot](../harness/fixtures/r10k-5.0.3/hostile-moduledir-dot.json), [g10k relative](../harness/fixtures/g10k-0.10.0/hostile-moduledir-relative.json), [g10k absolute](../harness/fixtures/g10k-0.10.0/hostile-moduledir-absolute.json) |
| A legal `moduledir` can still purge local files | With `moduledir 'manifests'` (a directory the control repo tracks), a tracked file survives but an untracked file in it is removed. | [tracked-dir-moduledir](../harness/fixtures/r10k-5.0.3/tracked-dir-moduledir.json) |
| Root ownership | r10k as root exits 1 on git's "dubious ownership" check until `safe.directory` is set; with it, files it writes become owned by root. | [run-as-root-ownership](../harness/fixtures/r10k-5.0.3/run-as-root-ownership.json) |
| r10k can report drift | `r10k deploy display --fetch` prints `status` `insync` or `outdated` per environment, exits 0 both times and writes no files. | [display-insync](../harness/fixtures/r10k-5.0.3/display-insync.json), [display-outdated](../harness/fixtures/r10k-5.0.3/display-outdated.json) |
| Bolt: partial failure looks like total failure | One reachable and one unreachable target gives exit 1, the same as every target failing. Only the per-target status tells them apart. | [mixed-targets-partial](../harness/fixtures/bolt-4.0.0/mixed-targets-partial.json) |
| Bolt: any string is a target | A made-up host name is tried as a host and fails as a connect error, so the SDK must allow only Inventory node ids itself. | [unknown-target](../harness/fixtures/bolt-4.0.0/unknown-target.json) |
| Bolt: dry run needs task support | `--noop` on a task that does not declare support is refused by Bolt with a top-level error and no per-target items; a task that supports it sees `PT__noop=true`. | [noop-unsupported](../harness/fixtures/bolt-4.0.0/noop-unsupported.json), [noop-supported](../harness/fixtures/bolt-4.0.0/noop-supported.json) |
| Bolt: secrets and the canary | A secret passed as a `sensitive: true` String parameter on stdin appeared in no Bolt output and no debug log (the canary check). A parameter typed `Sensitive[String]` is rejected when the value comes as JSON. | [sensitive-param-canary](../harness/fixtures/bolt-4.0.0/sensitive-param-canary.json), [sensitive-typed-param-rejected](../harness/fixtures/bolt-4.0.0/sensitive-typed-param-rejected.json) |
| The flush works with the runner certificate | The runner flushes one environment's cache on Puppet Server 9.0.17 through Bolt `http_request`: HTTP 204. | [flush-with-cert](../harness/fixtures/puppetserver-9.0.17/flush-with-cert.json) |
| A refused flush is a Bolt success | Without a certificate (or with a valid but unlisted one) the server answers 403, yet Bolt exits 0 with item status `success`. The refusal is only in `value.status_code`. | [flush-no-cert](../harness/fixtures/puppetserver-9.0.17/flush-no-cert.json), [flush-other-cert](../harness/fixtures/puppetserver-9.0.17/flush-other-cert.json) |
| The server does not check the environment name | An unknown name and a missing `environment` parameter both return 204; leaving the parameter out flushes every environment. | [flush-unknown-environment](../harness/fixtures/puppetserver-9.0.17/flush-unknown-environment.json), [flush-no-environment-param](../harness/fixtures/puppetserver-9.0.17/flush-no-environment-param.json) |
| The package default grants no one the flush | Puppet Server 9's own `auth.conf` has no environment-cache rule, so an operator must add an allow rule for the runner's certificate. | [package-default-auth-rule](../harness/fixtures/puppetserver-9.0.17/package-default-auth-rule.json) |
| A bad certificate path is a Bolt failure | Exit 1, kind `http_request/connect-error`. | [flush-bad-cert-path](../harness/fixtures/puppetserver-9.0.17/flush-bad-cert-path.json) |

## What was not run

- **Puppet Enterprise Code Manager.** Whether r10k running beside Code Manager's
  file sync causes trouble was not tested, because there is no Puppet
  Enterprise licence (D-10). The note and the requirement it creates are in
  [code-manager.UNVERIFIED.md](../harness/fixtures/code-manager.UNVERIFIED.md).
- **A recording on arm64 and a diff against amd64** (research assumption A8).
  Every fixture was recorded on linux/amd64 under emulation. Puppet Server 9.0.17
  was started natively on arm64 and reached `running`, but the runner's arm64
  variant was not exercised and no fixture was diffed across architectures. The
  nightly job runs on an amd64 runner and will compare against these
  recordings; a native arm64 re-record (unset `DOCKER_DEFAULT_PLATFORM`, then
  `HARNESS_RECORD=1 ./harness/run.sh`, then read the `git diff`) is the way to do
  the diff by hand.
- Bolt over a real SSH or WinRM transport, Bolt plans, and r10k or g10k against
  a real remote git host. The fixtures use local transport, local git and names
  under the reserved `.invalid` domain.

## Human testing guide

These are the steps an AI cannot do for you. Run them from the repository root.
If a step does not match its Expected line, stop and report the step number and
what you saw.

### H1: Two full runs in a row are green and change nothing

Do: run `./harness/run.sh`, wait for it to finish, run it again, then run
`git status --porcelain harness/fixtures`.

Expected: both runs end with `ok` for the harness package and exit 0. The `git
status` command prints nothing.

What this proves: the harness is repeatable from one command, and compare mode
does not quietly rewrite the evidence.

### H2: The default test run never starts the harness

Do: run `go test ./... -count=1 -v -run TestHarness 2>&1 | grep -c '=== RUN'`.

Expected: it prints `0`.

What this proves: ordinary `go test ./...` starts no container and needs no
registry.

### H3: A changed fixture makes the next run fail and say which field

Do: edit `harness/fixtures/r10k-5.0.3/deploy-success.json` and change the
`exit_code` value from `0` to `7`. Run `./harness/run.sh -run TestHarnessR10kDeploySuccess`.
Then restore the file with `git checkout -- harness/fixtures/r10k-5.0.3/deploy-success.json`.

Expected: the run fails with a line like `fixture fixtures/r10k-5.0.3/deploy-success.json:
field exit_code: recorded 7, observed 0`. After the `git
checkout`, `git status --porcelain harness/fixtures` prints nothing and the
run passes again.

What this proves: compare mode really compares; a tool upgrade that changes a
result cannot slip through.

### H4: The server image holds no credential

Do: right after a rebuild of the Puppet Server 9 image, while
`PUPPET_CORE_API_KEY` is still set in your shell, run these two commands. The
first prints only a count, never the key:

```
docker history --no-trunc <your-server-image> | grep -cF "$PUPPET_CORE_API_KEY"
docker run --rm --entrypoint cat <your-server-image> /etc/apt/auth.conf.d/apt-puppetcore-puppet.conf
```

Then `unset PUPPET_CORE_USER PUPPET_CORE_API_KEY`, and confirm
`env | grep PUPPET_CORE` prints nothing.

Expected: the first command prints `0` (a plain `grep -i api_key` would print
a non-zero count, because the build's placeholder comment contains those
words; that is why the check searches for the key's value instead). The second
prints a 72-byte file in which every line starts with `#` (a commented
template, no login and no password).

What this proves: the Puppet Core credential reached the build only as a secret
mount and is not in any image layer or file.

### H5: The nightly job works on the real runner

Do: in GitHub, open the Actions tab, choose the `harness` workflow and press
`Run workflow`. Before the first time, set the repository variables
`STAGEHAND_HARBOR_HOST` and `STAGEHAND_HARBOR_PROJECT` and the Actions secrets
`HARBOR_USERNAME` and `HARBOR_PASSWORD` (a pull-only robot account), and check
the self-hosted runner has Docker Compose v2 and Go.

Expected: the job ends green. Its last step, "Log out and remove harness/.env",
runs even if an earlier step failed. No log line shows the password.

What this proves: the harness runs on the CI machine, pulling the images by
digest, with credentials taken only from CI settings.

### H6: The registry project visibility is what you decided

Do: in your registry's web page, open the project the harness images are in and
read its access level.

Expected: it shows the access level you chose on purpose. The image holds
licensed Puppet Core binaries, so private is the normal choice. If it is
public, that must be a decision you made knowingly.

What this proves: nobody is publishing licensed binaries by accident.

## Troubleshooting

- **`harness/.env is missing`** (exit 2): copy `harness/.env.example` to
  `harness/.env` and fill in the host and project.
- **`Could not pull the ... image by digest`**: you are not logged in to the
  registry, or the digest in `harness/images.lock` was never pushed. Run
  `docker login <your-registry-host>` and try again. The runner image falls
  back to a local build; the Puppet Server 9 image cannot (it needs the Puppet
  Core credential), and the server scenarios then fail with a clear message.
- **`Puppet Server 9 image not available`**: same cause; the server scenarios
  need that image.
- **The server takes a long time to become healthy**: under amd64 emulation on
  an Apple-silicon Mac it takes about 50 seconds instead of 16. The harness
  waits; if it still times out, close other heavy containers, or unset
  `DOCKER_DEFAULT_PLATFORM` to run natively.
- **A leftover container from an interrupted run**: `run.sh` removes its own
  containers and volumes on exit; if your terminal was killed, run
  `docker compose -f harness/compose.yaml down -v --remove-orphans` with the
  same `HARNESS_PROJECT` name, or just run `./harness/run.sh` again.
