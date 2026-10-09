# Puppet Enterprise Code Manager coexistence: not run

Status: NOT RUN. Nothing in this file is evidence. It records what the harness
did not check, so a later phase does not mistake silence for safety (D-10).

## Why it was not run

Code Manager ships with Puppet Enterprise. There is no Puppet Enterprise licence
available to this project, and the harness runs the open-source Puppet Server 9
packages (Puppet Core), which do not include Code Manager. So no fixture under
`harness/fixtures/` says anything about Code Manager.

## What was not verified

- What happens when r10k (or g10k) deploys into the same environments directory
  that Code Manager's file sync also manages: whether the two overwrite each
  other, whether file sync reverts the SDK's deploy, or whether the SDK's deploy
  leaves file sync in a state it does not recover from.
- Whether a deploy marker written by r10k means anything to Code Manager.
- Whether Code Manager's own environment-cache flush and the SDK's flush race.

## Requirement carried to Phases 15 and 17

The SDK must detect Code Manager on the target and refuse to run r10k beside it
unless the operator explicitly overrides the refusal. This applies to the deploy
tool work in Phase 15 (TOOL-07) and to the deploy proposal gate in Phase 17: the
refusal is a plain error with a fix line, and an override is a recorded operator
choice, never a default.

## Candidate signals to verify in Phase 15 research

Every item below is a guess about how Code Manager might be detected. None of
them has been tried against a real Puppet Enterprise installation.

- UNVERIFIED: the target's `puppet.conf` or Puppet Server configuration points
  its environment directory at a Code Manager managed location.
- UNVERIFIED: the Puppet Server `file-sync` service is enabled in the server's
  service configuration (for example `file-sync` entries under the server's
  `conf.d` or `bootstrap.cfg`).
- UNVERIFIED: the Puppet Enterprise `pe-puppetserver` package is installed, or
  a `/etc/puppetlabs/puppetserver/conf.d/file-sync.conf` file is present.
- UNVERIFIED: the Code Manager API answers on its port (8170) at the target.
- UNVERIFIED: the environments directory is a staging path rather than the live
  `codedir` (Code Manager deploys to a staging directory and syncs from there).

Phase 15 research should confirm or discard each signal on a real Puppet
Enterprise system, or record that it could not, before the SDK relies on any of
them.
