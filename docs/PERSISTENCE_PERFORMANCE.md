# Persistence performance: measured costs and bounded improvements

Read-only audit, 2026-09-26. This is shared AIH work, not consumer-specific
orchestration. Source inspected: `49b1c3b`; operational snapshot: revision 4936.
The consumer was stopped. No SQLite/state rewrite, pruning, Git publication,
history deletion or format migration was performed for these measurements.

## Current size and growth

| Item | Observed value | Meaning |
| --- | ---: | --- |
| SQLite file | 1,695,744 bytes | About 1.6 MiB; not currently a large database |
| Current compact JSON body | 540,726 bytes | One overwritten SQLite snapshot row |
| Portable role runs | 637; 236,016 JSON bytes | About 44% of the current compact snapshot; no general retention bound |
| Tasks | 21; 279,678 JSON bytes | About 52%; includes findings, evidence and historical decisions |
| Local events | 2,780; 129,066 message bytes | Append-only table; no existing age/count retention |
| Local commands | 107 rows | No existing retention; pending commands must survive |
| Applied command IDs | 82 | Portable idempotency state; cannot be blindly truncated |
| State branch | 4,946 commits | Historical checkpoints, not records parsed on every current-state read |

Task #6 has 88 decisions occupying 33,271 JSON bytes. Some decisions are scope
recovery or guidance receipts used by the engine. Treating all old decisions as
disposable log text would damage recovery and authority attribution.

The control mirror reports approximately 118 MiB of loose objects and 20 MiB of
packs. Those figures include product source and state; they are not a measurement
of remote state storage alone. Git compression/deltas also mean repeated full
JSON encoding is not equivalent to transmitting that many full raw bytes.

AIH reads the latest snapshot, not the entire Git state history on each operation.
However, that latest snapshot carries accumulated runs and task history. JSON is
rewritten as a complete snapshot; it is not one continuously appended JSON file.
Local event/command tables and Git ancestry grow separately.

## Local processing measurements

Warm-cache, 30 samples, `GOMAXPROCS=1`, Go 1.27.1 on the current Windows host.
Release checks ran concurrently. These are local operation timings, not isolated
microbenchmark guarantees, complete controller latency or measured push/fsync cost.
The SQLite connection used `mode=ro`.

| Operation | Median | p95 |
| --- | ---: | ---: |
| Compact JSON marshal | 6.77 ms | 16.01 ms |
| Pretty JSON marshal | 7.32 ms | 10.17 ms |
| JSON unmarshal | 7.26 ms | 9.04 ms |
| `model.Clone` JSON round trip | 10.87 ms | 13.06 ms |
| Decode, migrate and validate | 30.25 ms | 45.94 ms |
| SQLite body read | 1.66 ms | 3.00 ms |
| SQLite read plus decode/validation | 34.31 ms | 49.55 ms |

Three read-only `git ls-remote` probes took 1,520, 1,476 and 1,482 ms. This is
network-command latency only. It does not prove actual push or controller-lock
duration, and multiplying it by publication count is not a delivery-time estimate.

## Measured validation repair

A CPU profile of 100 snapshot decodes at the audited source attributed 70.8% of
sampled cumulative CPU to `regexp.MustCompile`. Several validation helpers
compiled identical patterns for every visited record. The candidate now compiles
those private immutable patterns once, preserving their literals and every
validation predicate. This changes neither schema nor accepted inputs.

A second comparison used the identical 540,726-byte snapshot, 30 warm samples,
`GOMAXPROCS=1`, and no active release gate. Baseline `49b1c3b` ran first, followed
by repair `ec7e63d`:

| Operation | Before median / p95 | After median / p95 |
| --- | ---: | ---: |
| Decode, migrate and validate | 14.58 / 20.15 ms | 3.52 / 4.52 ms |
| SQLite read plus decode/validation | 18.73 / 24.59 ms | 5.43 / 9.89 ms |

Host/GC variance remains: unchanged clone code also measured 8.95 ms before and
5.00 ms after. The profile and tests support removing repeated compilation;
these sequential measurements do not establish a precise end-to-end speedup.
Model tests passed and scoped review found no material regression. The complete
integrated release and deployed consumer observation remain required.

## Repeated work in current code

`model.Clone` marshals and unmarshals the entire state. The 400 ms scheduler tick
uses several snapshot reads, including capacity and fresh preflight selection.
Freshness matters after admission mutations; replacing those reads with a stale
snapshot would be an incorrect optimization.

A meaningful `Controller.persist` keeps the controller mutex through:

1. Full clone, mutation and semantic no-op comparison.
2. Full JSON marshal/unmarshal for portable redaction.
3. Pretty encoding, safety scan and full decode/validation in `StateCommit`.
4. Local Git blob/tree/commit processes and fenced atomic publication.
5. Full JSON serialization and SQLite snapshot save.

Lease renewal also commits the full snapshot. No-op mutations and ordinary local
pulses normally avoid remote publication until lease half-life. A high-frequency
scheduler tick does not imply a remote commit on each tick.

In the ten minutes ending at the stopped revision, there were 42 state commits:
16 changed capacity, eight changed runs and 23 changed tasks, with overlap between
these categories. This was a recovery/activation/provider-failure window, not a
representative steady-state rate. None changed only controller/revision. Real data
changes do not prove that every observation needs its own synchronous publication;
classify the changes before coalescing them.

## Improvement order and quality constraints

1. **Measure the actual critical section.** Add opt-in, bounded local-only phase
   timings for mutex wait, clone, redaction, state commit, push and SQLite save.
   Record instrumentation overhead. No extra remote state commits or per-phase
   fsyncs; preserve errors, publication order, lease fencing and recovery.
2. **Reduce repeated local snapshot work where freshness is preserved.** Use one
   valid snapshot per read stage or a proven immutable view. Never expose mutable
   controller state. Require equivalence, race and recovery tests, plus measured
   allocation/time changes at current and larger fixture sizes.
3. **Bound active state using an explicit history contract.** Keep active/recent
   runs and operational proof in the hot snapshot; archive older completed run
   detail in immutable bounded records with aggregates and verified references.
   Preserve throughput attribution, failed/incomplete runs, command idempotency,
   guidance, scope authority and cross-machine reconstruction. This requires a
   versioned model/migration, not an arbitrary `Runs = Runs[len-N:]` edit.
4. **Separate durable coordination from observations.** Identify capacity-only
   status changes that can stay local or accompany the next meaningful checkpoint.
   Backlog cursor/grace decisions, retry/hold state, accepted commands, source
   checkpoints, authority and integration results remain durable at their required
   boundaries. A blanket publish timer would compromise crash recovery/fencing.
5. **Add conservative local retention.** Retain unresolved commands and useful
   diagnostic windows; clean old acknowledged observations in bounded transactions.
   Measure WAL/checkpoint and filesystem growth over an active run. The small
   stopped database does not establish a current SQLite storage bottleneck.

JSON remains a useful deterministic, inspectable portable format. Format changes
are not the first intervention: the present local costs are milliseconds, while
an actual Git network command takes seconds and unnecessary provider/review loops
take much longer. That comparison motivates phase measurements; it does not prove
which component dominates the complete delivery critical path.

Implementation belongs to the existing controller/store/model/status boundaries.
No second persistence service or orchestrator is needed. Completion requires a
smaller measured hot path, bounded growth, unchanged validation/recovery semantics
and useful consumer delivery. Existing operational ownership: #88/#24, with
publication-lock analysis coordinated with #103.

## Evidence and implementation boundary

Read-only probes and sanitized results are preserved in the operator candidate's
ignored `.cache/persistenceprobe`, `.cache/publicationprobe` and measurement JSON.
They contain no committed live snapshot or credential data. Opt-in phase profiling
and the validation repair are implemented in the reviewed restoration candidate,
not yet deployed. `AIH_PERSISTENCE_PROFILE=1` enables a fixed-size local aggregate
for the six existing persistence phases. It adds one local runtime-record write
after the controller mutex is released, with no new remote publication, schema
field or authority change. Counts mean publications containing a phase; repeated
clone work in one publication is summed. The aggregate exposes mean and maximum,
not percentiles. Record writes are serialized independently of publication.
Opt-in, malformed-record, concurrent aggregation and real Git lease/fencing
fixtures passed after review corrections. Its own local write is outside the
phase timings; whole-operation overhead still needs deployed measurement.

History compaction, scheduler copy changes and publication coalescing remain
proposals. Production phase measurements and useful delivery are the next evidence
boundary, rather than a claim that the measured decode gain fixes all throughput.

## Separate candidate: capacity-only scheduler copy

`persistCapacity` needs the fresh capacity record for its no-op comparison, but
previously obtained it by cloning the complete snapshot. A separate candidate
(`c7369c2`, PR143) uses the same controller mutex and a capacity-only JSON round
trip. It retains detachment, nil/empty normalization and time encoding. Native
verification ownership is still reread inside the ordinary mutation; scheduling,
lease timing, transition logic and publication boundaries remain unchanged.

Pure tests cover full-clone equivalence, mutable-slice isolation and concurrent
paired-value consistency. Scoped review approved the source and these tests.
The race detector was unavailable with the host's CGO-disabled toolchain; the
concurrency fixture is not a claim of race-detector coverage.

Synthetic benchmarks on exact `c7369c2`, `GOMAXPROCS=1`, `-benchtime=100x`:

| Snapshot fixture | Full snapshot copy: time / allocated bytes | Capacity-only copy: time / allocated bytes |
| --- | ---: | ---: |
| 631 KiB | 8.17 ms / 1,498,730 | 15.67 microseconds / 1,565 |
| 2,521 KiB | 26.94 ms / 6,137,000 | 10.17 microseconds / 1,554 |

These are synthetic local operation measurements taken while the separate
restoration release ran. Their strongest evidence is that this read no longer
allocates copies of unrelated task/run history. Host/GC variance remains, and
these numbers do not establish an equivalent whole-tick or delivery speedup.
Ignored benchmark logs retain source, command, host and fixture-size metadata.
Lifecycle checks and an integrated release are pending. This candidate is not
part of the frozen PR142 release and is not deployed.
