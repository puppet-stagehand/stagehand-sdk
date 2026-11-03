# PlanExecution contract

PlanExecution is an additive contract_version 1 facet with Go RPC bindings.
Declare `bolt:run` for all four methods: ListCandidates, Preview, Start and Get.
There is no Bolt service in this SDK. Older hosts return Unimplemented: disable
launch and explain that host support is required. Never use an alternate HTTP
execution route as a fallback. There is no generated TypeScript RPC transport;
Node integrations use their existing Go bridge and application DTOs.

Every call requires a fresh host-correlated request context. Call the worker
client using the route's request context; its interceptor carries
`stagehand-request-id`. Background contexts have no actor correlation. Worker
Principal roles/scopes and caller-supplied identities cannot authorize execution.
The host resolves the live actor, current exact instance/team/node permissions
and, for Preview/Start, its deployer floor. Get checks ownership and current
scope; opaque IDs are not bearer authorization.

ListCandidates requires an explicit instance and returns only scoped node
references and whitelisted OS family/name/version, FQDN and stable identity
facts. Page size defaults to and is capped at 100. Opaque cursors are bound to
pack, actor and instance. No credentials or raw fact dump is returned.

Preview accepts an installed host-allowlisted public plan, typed JSON object
parameters and ordered bindings of public TargetSpec parameter names to trusted
node IDs. Host validation rejects unknown/duplicate/missing bindings, raw bound
keys and unsupported external targets. It derives the exact authorized union
inventory and preserves binding order. Booleans and arrays remain native JSON.
Preview never executes a plan, including with a dry-run flag. The host freezes
an immutable private envelope containing actor, pack, instance, canonical values
and schema/code/fact/credential revisions. Public canonical fields and checks
are redacted; initial hosts reject sensitive parameters. No private path, CLI,
credential or execution identity can be supplied by the caller.

Start accepts only preview ID and idempotency key. It revalidates actor, scope,
expiry, prerequisites and revisions before atomic target reservation and launch.
Matching retries return the same run; a key reused for another preview conflicts.
The launch envelope cannot widen authority after request correlation expires.
Get uses a fresh request context. Unknown states/checks fail closed. Native plan
failures are structured, not task-item success inferred from exit status.
Interrupted does not imply rollback. Cancellation is not part of this contract.

Normative host bounds (UTF-8 byte counts for strings):

| Value | Maximum |
| --- | --- |
| Instance ID / plan / parameter name / idempotency key | 128 bytes each |
| Node ID | 160 bytes |
| Parameter JSON object | 64 KiB, depth 16 |
| Bindings / unique node union | 32 / 256 |
| Candidate page | 100 |
| Checks per preview, run or candidate | 256 |
| Check code / check or error message | 64 / 1024 bytes |
| Structured error details object | 16 KiB, depth 16 |
| Redacted result JSON | 1 MiB, depth 16 |
| Preview TTL / execution timeout | 5 minutes / 1 hour, host owned |

These are host obligations, not client-side authorization or an SDK executor.
`host.Local` returns PermissionDenied without `bolt:run`, and Unimplemented with
it. Tests may explicitly register a PlanExecution fake through
`workertest.NewFakeHost`; such fixtures verify transport only and supply no
production executor or actor authority. No console implementation or launch is
included in this slice.
