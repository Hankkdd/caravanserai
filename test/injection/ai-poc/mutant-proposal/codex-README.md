# Surviving product-code mutants

Each patch is independent and applies from the repository root with `patch -p1`.
Only product Go files are changed by the patches. No test files were changed.

```sh
patch -p1 < mutants/grace-config.patch
go test ./... -count=1
go test -tags e2e ./test/integration/... ./internal/store/postgres/... -count=1
patch -R -p1 < mutants/grace-config.patch
```

Validation used Go 1.26.5 on linux/arm64. The unmodified baseline and each
individually applied mutant passed `go test ./... -count=1`. Each mutant was
then reverted with `patch -R -p1`. SHA-256 comparisons verified all 373 original
files, including tests, against the initial snapshot after each reversal.

The integration command was attempted on the baseline and on every mutant.
PostgreSQL and MinIO fixtures could not start because `/var/run/docker.sock`
is absent. Integration survival is therefore **untested**, not a pass or a
mutation-related failure. The tagged packages compiled; infrastructure-dependent
assertions could not run. `results.json` records exit codes and durations;
`logs/` contains the complete command output.

| Patch | Rule | Unit survived | Integration survived | Trigger and consequence |
| --- | --- | --- | --- | --- |
| `grace-config.patch` | R1 | yes | untested | With grace-expired release disabled, the server still enables it; an overlap-allowed stateless Project can leave a lost Node without stop evidence. |
| `termination-config.patch` | R5 | yes | untested | Configure a running grace of 30s and termination timeout of 10m; the server wires 30s into the termination controller and marks TerminationBlocked early. |
| `preflight-budget.patch` | R4 | yes | untested | An unexpected Docker preflight error consumes an attempt without executing recovery; spaced-out failures can exhaust the budget before any restart runs. |
| `snapshot-decode.patch` | R8 | yes | untested | A 200 ownership-list response with malformed JSON is accepted as an empty or partial snapshot; the orphan sweep can stop containers whose ownership is unknown. |
| `fenced-conflict.patch` | R7 | yes | untested | A version conflict in the fenced status loop returns success immediately; the caller's status mutation is silently dropped instead of retried. |
| `fenced-cas.patch` | R2 | yes | untested | Delete and recreate the same Project name between the read and swap, with the successor at the same resource version; the old assignment's report overwrites the successor's status. |
| `cleanup-last-kind.patch` | R9 | yes | untested | An earlier resource kind retains resources but the final kind is empty; the cleaner reports ResourcesRemoved and permits deletion despite the remaining resources. |
| `apply-clears-stop.patch` | R3 | yes | untested | Apply a changed spec to a Running assignment with WorkloadStopped recorded; the apply clears the durable no-start latch without changing the assignment, allowing an in-place update to restart it. |

## Observed coverage gaps

- Server configuration tests and controller tests do not exercise the actual
  configuration-to-controller wiring in `cmd/cara-server/main.go`.
- Recovery gate tests cover missing volumes and stale containers, but do not
  assert that an unexpected preflight error leaves the attempt budget unchanged.
- Ownership sweep tests exercise HTTP server errors, but do not cover a
  successful HTTP response whose ownership list cannot be decoded.
- Store retry unit tests exercise `retryProjectStatusUpdate` through a fake CAS;
  they do not exercise the separate `reportProjectStatusFenced` conflict path.
- Assignment integration tests reject already-stale reports. The same-version
  delete-and-recreate hook test targets ResourcesRemoved's UID-bound update,
  rather than the fenced status report's swap.
- The multi-kind cleanup test leaves the **last** resource kind unresolved;
  replacing accumulation with assignment still preserves that last result.
- Spec-update tests check the agent refuses an existing WorkloadStopped latch,
  but do not cover the server applying a spec after recording that latch.

Integration gaps above are based on source inspection, not a successful
integration-suite execution in this environment.
