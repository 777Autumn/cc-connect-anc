# Offline original-work device recovery

`Host.Bind`, input activation and output reads continue to require the exact
persisted `device:inode:uid:gid` identity. A changed device number is a refusal,
not evidence that the directory is safe to adopt automatically.

An explicitly authorized operator may re-attest one or two original native
works through the bounded maintenance command below. It is not an agent tool,
startup migration, new-session workaround or permission to edit SQLite manually.
It preserves work/session/principal, inputs, file checks, versions and delivery
receipts. It sends nothing. Only the three directory identity fields per selected
work change in one transaction; other work records remain unchanged.

## Required evidence and exclusion

Before any write, freeze the actual installed runtime/configuration, original
ledger, active sessions, complete work trees and immutable snapshots. Reconcile
each selected project's configured actor/chat/session, native session identifier,
work ID and deterministic work root against historical records. Match recorded
inputs and accepted deliveries to their stored hashes and snapshots. Historical
volume identity or unanchored draft provenance that cannot be verified remains
unknown; owner re-attestation does not mark those drafts checked or delivered.

Retain a verified protected backup and independently reviewed maintenance plan.
Use the shared CRM release contract and maintenance startup helper to pause the
verified service. Prove no gateway, model, Office/render or other writer remains;
the instance lock and SQLite transaction do not exclude filesystem writers.
Recheck configuration, active sessions and the frozen whole-tree inventory after
the stop and immediately before invoking recovery. A mismatch stops maintenance.

## Invocation and manifest

Run as the ordinary gateway account, using the frozen absolute configuration
path and its SHA-256. The existing same-configuration instance lock must be free.
No platform, agent or sender is initialized by this command.

Model-owned `0600` drafts can be unreadable by the gateway. Do not change their
permissions, skip their hashes or run the ledger transaction as root. A reviewed
maintenance parent may pre-open precisely the frozen output files read-only,
using no-follow directory traversal and verifying the frozen identities, then
spawn this command under the original gateway UID/GID/groups with those handles
inherited. The optional final argument `--inherited-output-fds=7,9,12` contains
only descriptor numbers. Their order is manifest work order, then sorted relative
paths of **all** non-directory `outputs/` entries for each work. No config, ledger,
input, snapshot or directory handle is accepted. No descriptor remapping or
permission change is needed; Python's `pass_fds`, `user`, `group`, and
`extra_groups` can preserve that boundary without a `preexec_fn`.

The list must be canonical, unique, bounded and exactly match those output
entries. The command verifies read-only regular handles against both the frozen
inventory and current paths. Both tree passes read their actual bytes from offset
zero; invalid handles never fall back to opening another path. All existing
ownership, single-link, no-follow, content, size and transaction checks remain.
The command closes inherited handles before returning. The parent must also
close its copies before its existing no-writers check. Read handles do not lock
out external writers; stopped-runtime exclusion and the full post-check remain
required. Invalid or excessive inherited handles reject before the transaction;
later I/O failure aborts recovery and still requires read-only reconciliation.

```text
cc-connect-anc files-recover-device /absolute/config.toml <config-sha256> < manifest.json
```

The manifest is the canonical Go JSON encoding of `files.DeviceRecoveryRequest`:

```json
{"state_sha256":"<raw file_state.payload SHA-256>","works":[{"project":"<configured native project>","work_sha256":"<work ID SHA-256>","entries":{".":{"identity":"<device:inode:uid:gid>","mode":1512,"directory":true},"inputs":{"identity":"<device:inode:uid:gid>","mode":1512,"directory":true},"outputs":{"identity":"<device:inode:uid:gid>","mode":1528,"directory":true}}}]}
```

The example omits files; an actual manifest must include every file/directory.
Struct field order is as shown, entry map keys are sorted, and whitespace is
optional. `mode` is the raw POSIX `st_mode & 07777`, encoded as a decimal integer.
Every regular file includes its `sha256` after `directory:false`; directories
omit it. Use `encoding/json.Marshal` on the exported request types to create the
manifest. Duplicate, unknown or aliased keys and noncanonical encodings reject.

Only device numbers may differ from the three saved identities; inode, UID and
GID must match. Old devices must agree, as must new devices. All trees retain
existing protected-root/child ownership rules. Files must be regular, singly
linked, not group/world writable and owned by the existing input/output owner.
Symlinks, foreign devices, missing or extra entries and changed hashes reject.
Per work, tree plus referenced snapshots is bounded to 1,024 entries and 200 MiB,
with 20 MiB per file and directory depth 12. Pending imports or any delivery that
is not accepted with a receipt reject; recovery does not normalize submitted
deliveries. The raw state hash is checked within the write transaction, then
canonical state equivalence prevents unknown fields from being discarded.

## Commit and acceptance

Freeze the intended after-state before invocation. Record a durable one-shot
intent and retain stdout/stderr and the result's before/after state hashes. After
the command, read back the full ledger and prove the semantic delta is exactly
the selected three/six directory identity fields. Recheck the complete trees,
snapshots, sessions, connection bindings and other business ledgers unchanged.

On timeout, cancellation, network loss, nonzero exit or missing receipt, the
commit may be uncertain. Perform read-only reconciliation against the frozen
before/after states. Do not automatically replay recovery, restore an old ledger,
restart the service or resend accepted files. A stale manifest is rejected.

After the reviewed exact-SHA GitHub/CI/candidate gates, necessary binary update,
verified single start and health window, use fresh markers in the two authorized
test private chats. Confirm real Reply receipts, same original work identity and
no repeated historical file delivery. Preserve the initial failed acceptance
separately from recovery acceptance. Synthetic isolated fault results and actual
Feishu results are distinct evidence.

Regression entry points: `TestBindDeviceChange*`, `TestRecoverDeviceChange*`,
`TestDecodeDeviceRecovery*`, `TestRecoverFileDevices*`. Native Ubuntu/macOS CI
runs these with the race detector. No device fault is injected into production.
The same two CI platforms explicitly run the privileged disposable setup for
`TestRecoverDeviceChangeAcrossUIDReadOnlyFiles`; both real command invocations
inside that fixture use an ordinary ledger UID distinct from the private-file UID.
