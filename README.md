# Fidelic conformance suite

An executable conformance suite for the Salesforce REST API, written from Salesforce's
own public documentation. Point it at the [Fidelic](https://fidelic.dev) emulator or at
your own real org and it prints a scorecard.

Every test cites the doc page it encodes. The suite is the contract; the emulator is
measured against it.

## Scorecard

Against the emulator (`--target=emulator`):

```
Running 66 test(s) against target "emulator" (http://localhost:8080) — 5 SKIPPED (org prerequisite) — 2 PENDING (docs-derived, pending real-org verification)

Scorecard: 66/66 PASS
```

**66/66 pass · 5 skipped · 2 pending** — and the skips and pendings are the methodology
showing its work, not gaps swept under the rug:

- **66 pass** — every test that can run against a stock emulator passes.
- **5 skipped** — tests that need a real org's schema (a custom field, a validation
  rule) which the emulator can't provide. They run against `--target=real`; on the
  emulator they're skipped *with the reason printed*, never silently absent.
- **2 pending** — behaviors pinned from the docs but not yet confirmed against a real
  org. They're marked `pending` on purpose: a docs-derived claim awaiting an org capture
  is provenance made visible, not a passing test we're pretending we have.

## Methodology

Each test is written **from the public docs first**, then verified:

1. **Docs-cited.** Every test carries a `citation:` to the Salesforce documentation page
   it encodes — the REST API guide, SOQL/SOSL reference, or error-code reference.
2. **Written first.** Tests are written against what the docs *say*, before checking any
   implementation — so they measure conformance, not a re-description of one engine.
3. **Org-verified.** Where a behavior is confirmed against a real Developer Edition org,
   it's a plain test. Where it's only docs-derived so far, it's marked `pending` until an
   org capture confirms it. That boundary is always visible.

## Running it

You need [Go](https://go.dev) 1.21+.

### Against the emulator

Start the emulator (the [Fidelic](https://fidelic.dev) container or binary) on
`localhost:8080`, then:

```sh
go run ./cmd/confrun --target=emulator
```

The `emulator` target needs no credentials. Org-prerequisite tests skip automatically
(with the reason shown); Provisional tests show as `PENDING`.

### Against your own real org

```sh
cp targets.example.yaml targets.yaml
# edit targets.yaml: set base_url + a Connected App's client_id / client_secret
go run ./cmd/confrun --target=real --targets targets.yaml
```

`targets.yaml` is git-ignored, so credentials never enter the repo — tests reference them
only through `{{target:client_id}}` / `{{target:client_secret}}` tokens resolved at
runtime. Teardown runs unconditionally, so a real-org run leaves nothing behind.

Two of the real-org tests need one-time org setup (a custom `ExternalId__c` field and one
validation rule); the runner prints exactly which when a prerequisite is missing.

### Scope it

```sh
go run ./cmd/confrun --target=emulator --only tests/soql   # one area
go run ./cmd/confrun --target=emulator -v                  # print citations
```

Exit code is non-zero if any test fails, so it drops straight into CI.

## Layout

```
tests/           the conformance tests (YAML), by area — auth, crud, errors, soql,
                 validation, and emulator-features (reset, fault injection, hooks)
cmd/confrun/     the runner CLI + scorecard
runner/          test loader, HTTP driver, JSON comparator
targets.example.yaml   copy to targets.yaml for a real-org run
```

## License

Apache-2.0. Copyright 2026 Fidelic.
