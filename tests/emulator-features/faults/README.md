# Fault-injection conformance

The YAML tests here verify each injectable fault against the emulator:

- `UNABLE_TO_LOCK_ROW` — armed on update
- `REQUEST_LIMIT_EXCEEDED` — armed after N calls
- injected latency

Two further tests cover the *arming contract* rather than a fault: that a body which arms
nothing is refused and the refusal names the accepted keys (`faults-006`), and that a
`latencyMs`-only body remains a legal arm (`faults-007`). Unknown JSON keys are dropped
silently, so a mistyped key has no other way to announce itself.

Faults are transport-level: they fire identically for every object, before any
per-object logic runs. Arm one via `POST /__emulator__/faults`, exercise the API, and
assert the error envelope and HTTP status match what a real org returns under that
failure — the failures a sandbox will never let you stage on demand.
