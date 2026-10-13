# Controlled file work candidate

Disabled by default. This candidate does not authorize deployment, account or
permission changes, model execution, or live Feishu sends. The offline launcher
has no provider network/authentication; real model readiness is **incomplete**.
An explicit `runtime = "native"` uses the existing managed employee Claude instead.
It retains that employee's cwd, authentication, knowledge clients and Skills,
adding the selected work directory with `--add-dir`. It does not provide a separate
OS sandbox between the same employee's works. Account separation remains unchanged.

An enabled project uses one fixed sender/chat Feishu route and a distinct existing
`run_as_user`. Each directed top-level request starts a native session and work.
Replying to that request or a returned file resumes that exact work. An unknown
reply is rejected; an unmentioned group message is accepted only with a recorded
parent from the same authenticated principal. New work never borrows recent inputs.
Concurrent requests receive the existing busy response; no background queue is added.

Inputs are limited to four files, 20 MiB each and 40 MiB total, with a 30-second
download deadline. The first format boundary accepts local DOCX/XLSX packages;
encrypted, macro/active content and external relationships are rejected. All Word
fields (including PAGE/TOC) are currently unsupported; they are rejected rather
than silently stripped. Static Word text/tables and local spreadsheet formulas
such as SUM are supported. Originals
are retained with hashes. Output files must be in this work's `outputs`, regular,
single-link, owned by the configured model UID, and at most 20 MiB (100 MiB expanded).
One checked descriptor supplies the immutable snapshot and bytes actually sent.

Only `work-context`, `file-deliver`, `file-status` are added to the existing `/tool`
handler. They take the live host session token; identity, destination and work
authority are never caller-selected. The Unix endpoint has no management routes.
Native mode reuses the existing fixed `127.0.0.1:18743/tool` listener through
Claude's sandbox HTTP proxy; leave `tools_socket` empty in native mode. The client
uses the gateway-supplied `CC_FILE_WORK_ROOT`, not the employee's cwd, for output
publication. A missing proxy fails; there is no direct-network or admin fallback.
`ops/file_tool.py` is installed as `cc-connect-file` only during separately
authorized runtime provisioning. JSON comes from stdin, not a shell argument.

Delivery versions use compare-and-set. The host durably records `submitted` and
the delivery UUID before the single network attempt. Accepted requires a Feishu
message receipt. Missing/uncertain results stay `unknown` and block resends,
including renamed copies. Restart converts incomplete submissions to unknown.
Known input/format/scope/version refusals return `blocked` with a safe code;
post-send journal failures remain uncertain and must not invite resubmission.
An API acceptance does not prove an employee received or opened the file.

Banquet materials use CRM authorization on both publication import and pending
input activation. `handoff` defaults to `purpose: "current"`; `revision` is an
explicit reference request for the currently authorized original submitter in
their own private chat. The CRM decides whether the latest trusted publication
is stale and returns `purpose`, `reference_only` and `publication_basis_id`.
These host-owned fields survive the file ledger, restart, duplicate import and
`work-context`. A recorded revision reference cannot be converted to current by
omitting the purpose. It is input for a new, independently read-back delivery and
the existing approval flow, not current handoff evidence.

Publication imports now upgrade file state to schema 5 without reducing a later
schema. Schemas 1–4 remain readable; old publication entries gain basis metadata
only after fresh authorization. Old binaries reject schema 5, so rollback must
retain a compatible file host or disable affected handoffs; never downgrade the
ledger or restore historical authorization. Existing downloaded bytes cannot be
revoked. CRM checks exact known publication hashes to refuse unchanged stale
files; a changed hash alone does not establish correct document content.

Client-side preparation failures retain `status: blocked`,
`code: file_access_not_prepared` and exit code 2, adding `stage: local_preflight`
and `request_state: not_sent`. Their safe `reason` is `output_changed` for a byte
count/digest mismatch, `invalid_output` for a missing or invalid path/file,
`access_denied` for an explicit permission denial, or `preparation_failed` for
other preparation errors. These fields describe only this call before any request,
not earlier delivery attempts. Refresh `work-context` and reconcile prior receipts;
after resolving the cause, submit only unsent files using their final saved hashes.
Retain accepted and uncertain deliveries. Timeouts, invalid responses and host
post-send errors remain uncertain, without these local-preflight fields or retries.

The authenticated `handoff` tool retains the 30-second connection guard and has
a separate 150-second processing budget for the CRM read and at most four file
imports, including fresh authorization at import and activation. Its HTTP write
deadline is 190 seconds on both local transports; the paired CRM client waits
185 seconds. Other tools keep their existing deadlines. Waiting for the project
tool lock precedes these budgets, so these are not end-to-end queue guarantees.
Request cancellation and service shutdown still interrupt imports; a failed later
file does not undo an earlier import. Reconcile the work context before retrying.

Example configuration (all paths are fictional; do not apply to a live host):

```toml
[projects.file_work]
enabled = false
ledger = "/srv/file-host/project/ledger/files.db"
snapshots = "/srv/file-host/project/snapshots"
work_base_dir = "/srv/file-host/project/works"
tools_socket = "/srv/file-host/project/tools/action-tools.sock"
# Under the existing projects.agent.options table:
# file_work_launcher = "/opt/file-candidate/work_sandbox.py"
# file_work_config = "/srv/file-host/project/runtime.json"
# cmd must name the protected native CLI executable; legacy wrappers are not used.
```

The ledger/snapshot/tool parents and work base must already exist under protected
host ownership. `cc-connect files-init /absolute/protected/files.db` is an explicit
operator-only initialization and refuses an existing file; startup never initializes
or resets a ledger. Only new session directories are created by the host. Existing
directory ownership/modes are checked rather than repaired. A missing, replaced or
corrupt ledger fails closed. Do not restore an older ledger to clear unknown sends.
The work base group must be the distinct model account's primary group and an
existing gateway membership; work directories remain gateway-owned. See the
exact directory and publication modes in the isolation document. This candidate
does not provision the group, migrate old work directories or change service users.

The DOCX/XLSX journey uses explicitly fictional documents. Common PAGE/TOC fields
remain rejected without deletion. **Real template compatibility is unverified**;
the absence of a sanitized real template does not block fictional transport and
revision testing. No live file send or employee receipt is inferred from CI.

See [the isolation boundary](FILE_WORK_SANDBOX.md). The candidate does not install
CLI, document libraries, Skills, credentials, sudoers or ACLs. Provider access,
approved policy/Skill visibility, and knowledge client access inside the namespace
remain runtime acceptance work before enabling real models. Reverting source and
keeping `enabled = false` rolls back capability exposure; preserve the ledger and
snapshots for reconciliation, without replaying any send or business operation.
