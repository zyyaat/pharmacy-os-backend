package handlers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// AUD T-D2-1 contract: a provider webhook may flip a payment to 'failed'
// ONLY while the payment row is still 'pending'. A settled (succeeded)
// payment must never regress — not via the definitive-failure branch, not
// via the amount-tamper branch, not via an intent-creation failure. The
// enforcement lives in the SQL itself (`AND status = 'pending'` inside the
// same statement, under the FOR UPDATE row lock), because that is the only
// place that survives future refactors of the surrounding Go conditions.
//
// This test pins that invariant for BOTH provider handlers. If you add a
// new failure-flip statement, it must carry the guard or this test fails.

const (
	flipMarker = "SET status = 'failed'"
	flipGuard  = "AND status = 'pending'"
)

// Every failure-flip UPDATE in the provider handlers is a Go backtick raw
// string; the regex captures those literals so the guard can be asserted
// per-statement instead of per-file.
var rawStringRe = regexp.MustCompile("`([^`]*)`")

func TestPaymentFailureFlipBoundedToPending(t *testing.T) {
	files := []struct {
		name     string
		provider string
	}{
		{"paymob_checkout_handler.go", "paymob"},
		{"xpay_checkout_handler.go", "xpay"},
	}

	for _, tc := range files {
		t.Run(tc.provider, func(t *testing.T) {
			src, err := os.ReadFile(tc.name)
			if err != nil {
				t.Fatalf("read %s: %v", tc.name, err)
			}
			text := string(src)

			total := strings.Count(text, flipMarker)
			if total == 0 {
				t.Fatalf("no %q statements found in %s — either the flip moved elsewhere (update this contract) or the failure path was removed", flipMarker, tc.name)
			}

			inLiterals := 0
			for _, lit := range rawStringRe.FindAllStringSubmatch(text, -1) {
				body := lit[1]
				marks := strings.Count(body, flipMarker)
				if marks == 0 {
					continue
				}
				inLiterals += marks
				if !strings.Contains(body, flipGuard) {
					line := 1 + strings.Count(text[:strings.Index(text, body)], "\n")
					t.Errorf("%s:%d: `SET status = 'failed'` without `AND status = 'pending'` in the same statement — a late webhook on a succeeded payment would regress the ledger", tc.name, line)
				}
			}

			if inLiterals != total {
				t.Errorf("%s: %d of %d %q occurrences are outside backtick SQL literals — the contract cannot verify them; keep failure-flips in raw-string statements", tc.name, total-inLiterals, total, flipMarker)
			}
		})
	}
}
