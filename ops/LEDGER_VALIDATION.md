# Isolated file ledger measurements, 2026-10-14

The real file Host and SQLite Store at source `f28d9f479d55b70e08bdfc4ba36caa49e46ec7a9`
serialize other users' reads while publication authorization waits. On the isolated
Mac, a 30-second authorization wait produced 30.010–30.013-second reads through
the same Store and 30.044–30.065-second reads through another Store sharing SQLite.
Independent SQLite reads in those conditions took 0.313–0.952 milliseconds.
This is a controlled experiment, not a production latency SLA or capacity claim.

No runtime rules, production data, deployed binaries or database design changed.
The test binary ran as an ordinary user in a separate Mac temporary directory.
Only the external authorization service's waiting behavior was replaced in the
contention matrix. A source overlay adds clocks without changing Store control flow;
its unit test removes those statements and checks exact source equivalence.

## Reproduce

From this repository, build with the intended Go toolchain:

```text
python -B ops/build_ledger_experiment.py --go /path/to/go --output-dir /new/evidence/directory
```

This cross-builds a Darwin/arm64 test binary and records all file-package source
hashes, the overlay, toolchain and binary hash. Transfer the binary to a separate
Mac directory owned by the test user. Its temporary directory must be mode 0700
and have a group that the user belongs to; a wheel-owned inherited directory
failed the real Host's work-root checks in the first attempt.

```sh
TMPDIR=/isolated/owned/tmp MYANC_LEDGER_EXPERIMENT=1 ./ledger-experiment.test \
  -test.run TestValidationLedger -test.v -test.timeout 25m
```

The matrix takes about eleven minutes. It has three topologies (one Store,
separate Stores sharing SQLite, independent SQLite), two authorization stages
(import/activation), three delays (0/5500/30000 ms), and three repeats: 54 runs.
Each run concurrently performs message lookup, work-context and file-status
reads for a different user. Each `VALIDATION_JSON` line is an original sample.
Report each condition's three samples, median and maximum; do not claim P95.
Normal CI skips the opt-in timing experiments. The COMMIT acknowledgement-loss
regression runs normally and uses a real SQLite commit with a test driver wrapper.

## Findings and interpretation

- All 54 matrix runs returned successful reads, exactly two authorization calls,
  one original source delivery and successful subsequent reuse. Import did not
  redeliver the source. The 5.5-second delay crosses the current five-second SQLite
  busy wait; it is not a promised service limit.
- With a 1.5-second authorization wait and a 100-ms reader deadline, cancellation
  returned only after the competing work released its lock (about 1.4 seconds
  after the deadline). Reuse succeeded. Independent SQLite reads completed before
  their deadlines: those are explicitly **not** successful cancellation samples.
- Cancelling the import itself inside authorization returned in 0.030250 ms at
  stage one and 0.017833 ms at stage two. The callback counts were respectively
  one and two, the Store remained reusable and no additional file was sent.
- A separate test loses the acknowledgement after a real successful SQLite
  COMMIT. The callback executes once, the failed Store rejects another callback,
  and a fresh connection sees the committed state. This covers that explicit
  uncertain-result boundary, not every database failure mode.
- Synthetic histories of 100/1000/10000 works occupied 56,312/569,412/5,763,412
  payload bytes (SQLite files 61,440/577,536/5,775,360 bytes). Lookup medians were
  1.615/9.402/49.305 ms. These records have minimal synthetic metadata; they are
  not a simulation of three months of real documents or normal business traffic.

`mutex` includes contention between the three readers as well as the importing
writer. `sqlite_begin` includes actual BEGIN execution plus SQLite lock waiting;
the 50-ms retry backoff is only in total elapsed time. Authorization timing belongs
to the importing operation, so it must not be added to reader timings. File I/O,
JSON processing and scheduling remain in the total's unattributed remainder.
Synthetic scale rows measure message lookup, not all three operations.

The local evidence archive preserves the initial fixture-permission failure,
an invalid synthetic-message setup, the initial incomplete cancellation coverage,
and the corrected follow-up without overwriting any first results. A full Mac
file-package run initially lacked a testdata workbook; supplying the repository's
unchanged testdata made that suite pass. The Windows full repository run failed
on existing platform assumptions and absent web assets; exact GitHub CI remains
required for source delivery.

Priority: first shorten the transaction that contains remote authorization while
preserving reauthorization and commit boundaries; then make lock waits cancellable;
then evaluate narrower state reads as history grows. These are proposed next-batch
changes, not implementations or a recommendation to replace SQLite now. Real
Feishu end-to-end observations belong to the separate acceptance report.
