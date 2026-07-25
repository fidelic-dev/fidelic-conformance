// Command confrun runs the Salesforce API conformance suite against a target
// (the emulator or a real org) and prints a scorecard.
//
//	confrun --target=emulator --dir tests/
//	confrun --target=real --targets targets.yaml --only tests/auth
//
// Exit code is nonzero if any test fails, so it drops straight into CI.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/fidelic-dev/fidelic-conformance/runner"
)

func main() {
	var (
		target  = flag.String("target", "emulator", "target name from the targets file (e.g. emulator, real)")
		targets = flag.String("targets", "targets.yaml", "path to the targets config file")
		dir     = flag.String("dir", "tests", "directory of conformance test YAML files")
		only    = flag.String("only", "", "run only tests whose path contains this substring (e.g. tests/auth)")
		verbose = flag.Bool("v", false, "print citation and request detail for every test")
	)
	flag.Parse()

	if err := run(*target, *targets, *dir, *only, *verbose); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
}

func run(targetName, targetsPath, dir, only string, verbose bool) error {
	// Fall back to the checked-in example config so `confrun` works out of the
	// box against the emulator before anyone writes a real targets.yaml.
	if _, err := os.Stat(targetsPath); os.IsNotExist(err) && targetsPath == "targets.yaml" {
		if _, err := os.Stat("targets.example.yaml"); err == nil {
			targetsPath = "targets.example.yaml"
		}
	}

	tgt, err := runner.LoadTarget(targetsPath, targetName)
	if err != nil {
		return err
	}

	ctx := context.Background()
	if err := tgt.Authorize(ctx); err != nil {
		return fmt.Errorf("authorizing target %q: %w", targetName, err)
	}

	tests, err := runner.LoadTests(dir, only)
	if err != nil {
		return err
	}
	if len(tests) == 0 {
		return fmt.Errorf("no tests found under %q (only=%q)", dir, only)
	}

	// Emulator targets get a clean slate between tests (control-endpoint reset) and
	// exclude org-prerequisite tests, which need a real org's schema they can't have.
	isEmulator := strings.HasPrefix(targetName, "emulator")

	// Provenance/ergonomics guard: drop tests that don't run on this target
	// (e.g. emulator-only /__emulator__/ tests never dispatch at a real org), and on
	// an emulator target, tests that need org-side setup (a custom field / validation
	// rule the emulator can't provide) — shown as SKIPPED with the reason, not absent.
	var runnable []runner.Test
	var pending []runner.Test
	var orgPrereq []runner.Test
	skipped := 0
	for _, t := range tests {
		if t.Pending {
			pending = append(pending, t)
			continue
		}
		if isEmulator && len(t.Requires) > 0 {
			orgPrereq = append(orgPrereq, t)
			continue
		}
		if t.RunsOn(targetName) {
			runnable = append(runnable, t)
		} else {
			skipped++
		}
	}
	tests = runnable

	r := runner.New(tgt)
	passed := 0
	var failures []runner.Result

	fmt.Printf("Running %d test(s) against target %q (%s)", len(tests), targetName, tgt.BaseURL)
	if skipped > 0 {
		fmt.Printf(" — %d skipped (target-restricted)", skipped)
	}
	if len(orgPrereq) > 0 {
		fmt.Printf(" — %d SKIPPED (org prerequisite)", len(orgPrereq))
	}
	if len(pending) > 0 {
		fmt.Printf(" — %d PENDING (docs-derived, pending real-org verification)", len(pending))
	}
	fmt.Printf("\n\n")
	for _, t := range pending {
		fmt.Printf("PENDING %-12s %s\n", t.ID, t.Name)
	}
	for _, t := range orgPrereq {
		reason := ""
		if len(t.Requires) > 0 {
			reason = " — needs " + t.Requires[0]
		}
		fmt.Printf("SKIP  %-12s %s%s\n", t.ID, t.Name, reason)
	}
	for _, t := range tests {
		if isEmulator {
			_ = r.ResetEmulator(ctx) // clean slate; ignore (baseline re-applied server-side)
		}
		res := r.Run(ctx, t)
		if res.Pass {
			passed++
			fmt.Printf("PASS  %-12s %s\n", t.ID, t.Name)
		} else {
			failures = append(failures, res)
			fmt.Printf("FAIL  %-12s %s\n", t.ID, t.Name)
		}
		if verbose {
			fmt.Printf("      cite: %s\n", t.Citation)
		}
	}

	if len(failures) > 0 {
		fmt.Printf("\nFailures:\n")
		for _, res := range failures {
			fmt.Printf("\n  %s — %s\n", res.Test.ID, res.Test.Name)
			fmt.Printf("    cite: %s\n", res.Test.Citation)
			if res.Err != nil {
				fmt.Printf("    error: %v\n", res.Err)
			}
			for _, d := range res.Diffs {
				fmt.Printf("    diff: %s\n", d)
			}
			// Show the actual response so config-level failures (e.g. an OAuth
			// error_description) are visible instead of just "missing key".
			if len(res.Diffs) > 0 && len(res.RawBody) > 0 {
				fmt.Printf("    actual (HTTP %d): %s\n", res.Status, snippet(res.RawBody))
			}
			// Surface org prerequisites so a missing custom field / validation
			// rule reads as "provision the org", not a mysterious diff.
			if len(res.Test.Requires) > 0 {
				fmt.Printf("    ⚠ this test needs org setup (org prerequisites — see the test's requires:):\n")
				for _, r := range res.Test.Requires {
					fmt.Printf("        - %s\n", r)
				}
			}
		}
	}

	fmt.Printf("\nScorecard: %d/%d PASS\n", passed, len(tests))
	if passed != len(tests) {
		os.Exit(1)
	}
	return nil
}

// snippet renders a response body for diagnostics, trimming whitespace and
// capping length so a large HTML error page doesn't flood the scorecard.
func snippet(b []byte) string {
	const max = 500
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
