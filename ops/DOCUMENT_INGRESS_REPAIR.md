# Document intake and authorization repair

Transport downloads can finish out of creation-time order. A selected file-only
upload must reach controlled work intake even when another upload advanced the
session watermark. The engine retains message-ID deduplication, association,
durable supplement queues and activation checks. Old text instructions, including
text carrying an attachment, retain the existing stale-message policy. This does
not infer a new work or another recipient from an attachment.

Publication and project-receipt authorization now run outside the Store gate and
SQLite transaction, including receipt lookup, binding, import and activation.
Each transaction rechecks its destination and current source against the returned
grant. Activation also checks its pending-input snapshot. Concurrent activation
of exactly the same material may reconcile already-active inputs, only after
matching provenance and saved bytes; changed or missing material fails closed.
Import and activation retain separate fresh authorization calls. No authorization
or business callback is replayed after a transaction or uncertain COMMIT failure.

The per-Store gate observes cancellation. SQLite waits at most 50 ms per BEGIN
attempt before the existing cancellable retry interval. There is no database or
schema migration. Local JSON validation, snapshot I/O and document validation
still serialize inside the transaction; this repair does not claim all Host work
is nonblocking. Remote membership is verified at the authorization response, not
by an atomic transaction spanning the external service and SQLite.

Regression coverage includes out-of-order attachment intake and queue drain,
duplicate messages, old mixed/text requests, both authorization phases and Store
topologies, pending/source changes during the wait, concurrent activation,
cancelled grants and real lock-wait cancellation. The existing uncertain-COMMIT
and saved-snapshot security tests remain required. `ops/build_ledger_experiment.py`
builds the opt-in Mac timing matrix with source hashes and timing-only overlays;
normal CI skips timing experiments, not the regressions.

Production acceptance must still verify actual registered input hashes, native
tool traces, delivery destinations and unchanged business/approval state.
Synthetic test success alone is not an online-release receipt.
