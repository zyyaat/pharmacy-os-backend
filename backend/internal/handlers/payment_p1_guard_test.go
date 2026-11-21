package handlers

// Task 139 (P1 audit close-out) — source contracts for the payments/POS
// integrity batch. Same discipline as payment_flip_guard_test.go (AUD T-D2-1):
// the invariant is pinned on the SOURCE text because that is the only place
// that survives refactors of the surrounding Go conditions.
//
// Contracts pinned here:
//
//   - AUD-010 currency discipline: a verified webhook whose currency
//     disagrees with the stored snapshot is failed+flagged on BOTH provider
//     paths and on the settlement resync pull path (currency_mismatch_failed
//     / currency_mismatch_flagged literals).
//   - AUD-010 constant-time secrets: the XPay URL-token branch must compare
//     through crypto/subtle, never a plain string equality.
//   - AUD-010/032 DB-level dedup: insertWebhookTxn inserts with the partial
//     unique index as its ON CONFLICT arbiter, and migration 37 creates that
//     index (uq_payment_txn_provider_webhook).
//   - AUD-009 tenant ownership: the POS sale path resolves the customer
//     inside the sale transaction against the caller's pharmacy.
//   - AUD-015 protocol: a keyless POS sale is a 400 idempotency_key_required.

import (
        "os"
        "strings"
        "testing"
)

func mustSource(t *testing.T, name string) string {
        t.Helper()
        src, err := os.ReadFile(name)
        if err != nil {
                t.Fatalf("read %s: %v", name, err)
        }
        return string(src)
}

func TestCurrencyMismatchGuardContract(t *testing.T) {
        cases := []struct {
                file    string
                marker  string
                wantMsg string
        }{
                {"paymob_checkout_handler.go", "webhookCurrency", "currency_mismatch_failed"},
                {"xpay_checkout_handler.go", "webhookCurrency", "currency_mismatch_failed"},
                {"billing_settlement_handler.go", "session.Currency", "currency_mismatch_flagged"},
        }
        for _, tc := range cases {
                t.Run(tc.file, func(t *testing.T) {
                        text := mustSource(t, tc.file)
                        if !strings.Contains(text, tc.marker) {
                                t.Fatalf("%s lost the currency-compare marker %q — the AUD-010 currency discipline was removed or renamed", tc.file, tc.marker)
                        }
                        if !strings.Contains(text, tc.wantMsg) {
                                t.Fatalf("%s lost %q — currency mismatches must be flagged, not silently accepted", tc.file, tc.wantMsg)
                        }
                        // The compare must be case-insensitive on the provider's currency
                        // spelling and only enforced when both sides are known.
                        if !strings.Contains(text, "strings.EqualFold(") {
                                t.Fatalf("%s: currency compare must use strings.EqualFold (provider casing is not ours to assume)", tc.file)
                        }
                })
        }
}

func TestXPayURLTokenConstantTimeContract(t *testing.T) {
        text := mustSource(t, "xpay_checkout_handler.go")
        if !strings.Contains(text, "crypto/subtle") {
                t.Fatalf("xpay_checkout_handler.go lost the crypto/subtle import — the URL token must be compared constant-time (AUD-010)")
        }
        if !strings.Contains(text, "subtle.ConstantTimeCompare([]byte(provided), []byte(h.config.XPAYWebhookToken))") {
                t.Fatalf("xpay URL-token compare left subtle.ConstantTimeCompare — a plain != leaks match length through timing")
        }
        // Mirror check: the Paymob path already compared constant-time; if it
        // regresses the pair is no longer symmetric.
        paymob := mustSource(t, "paymob_checkout_handler.go")
        if !strings.Contains(paymob, "subtle.ConstantTimeCompare") {
                t.Fatalf("paymob_checkout_handler.go lost subtle.ConstantTimeCompare — webhook token compare regressed")
        }
}

func TestWebhookTxnInsertUsesPartialUniqueArbiter(t *testing.T) {
        paymob := mustSource(t, "paymob_checkout_handler.go")
        for _, marker := range []string{
                "ON CONFLICT (provider_transaction_id) WHERE txn_type = 'webhook' AND provider_transaction_id IS NOT NULL",
                "DO NOTHING",
        } {
                if !strings.Contains(paymob, marker) {
                        t.Fatalf("insertWebhookTxn lost the arbiter clause %q — duplicate webhooks must be absorbed by the DB-level unique index (AUD-010/032)", marker)
                }
        }

        mig := mustSource(t, "../../migrations/00000000000037_payment_txn_dedup.sql")
        for _, marker := range []string{
                "CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_txn_provider_webhook",
                "txn_type = 'webhook' AND provider_transaction_id IS NOT NULL",
        } {
                if !strings.Contains(mig, marker) {
                        t.Fatalf("migration 37 lost %q — the partial unique index IS the DB-level dedup guarantee", marker)
                }
        }
}

func TestPOSSaleOwnershipAndIdempotencyContract(t *testing.T) {
        // ADR-0005 (api_level 108): the POS sale contract split across the
        // layer boundary — the AUD-015 keyless-sale protocol guard stays in
        // the handler, the AUD-009 ownership SQL (and the whole transaction)
        // moved to internal/services/pos.go. Both halves are pinned where
        // they now live; losing either side weakens the contract.
        handlerText := mustSource(t, "product_pos_handler.go")
        for _, marker := range []string{
                // AUD-015: keyless sale is a protocol violation, not a silent
                // double-charge risk.
                "idempotency_key_required",
                // The handler must delegate the checkout to the service —
                // the SQL guards must never creep back into the protocol.
                "services.CreatePOSSale(",
        } {
                if !strings.Contains(handlerText, marker) {
                        t.Fatalf("product_pos_handler.go lost %q — the POS sale guard contract (AUD-009/015) was weakened", marker)
                }
        }

        serviceText := mustSource(t, "../services/pos.go")
        for _, marker := range []string{
                // AUD-009: ownership resolved inside the sale tx against the caller's
                // pharmacy — a foreign (but valid) customer UUID is invalid_customer.
                "SELECT EXISTS(",
                "FROM customers",
                "pharmacy_id = $2::uuid",
                // AUD-015: the idempotent replay of the original invoice.
                "idempotency_key = $2",
        } {
                if !strings.Contains(serviceText, marker) {
                        t.Fatalf("services/pos.go lost %q — the POS sale guard contract (AUD-009/015) was weakened", marker)
                }
        }
        // The ownership check must run INSIDE the sale transaction: it must
        // appear after pool.Begin and before the sales INSERT in source order.
        begin := strings.Index(serviceText, "tx, err := pool.Begin(ctx)")
        insert := strings.Index(serviceText, "INSERT INTO sales (pharmacy_id, branch_id")
        ownership := strings.Index(serviceText, "FROM customers")
        if begin < 0 || insert < 0 || ownership < 0 ||
                ownership < begin || ownership > insert {
                t.Fatalf("customer ownership check drifted out of the sale transaction window (begin=%d ownership=%d insert=%d)", begin, ownership, insert)
        }
}

func TestDebugSchemaRegistrationGates(t *testing.T) {
        text := mustSource(t, "debug_schema_handler.go")
        funcStart := strings.Index(text, "func (h *Handler) registerDebugSchema")
        if funcStart < 0 {
                t.Fatalf("registerDebugSchema helper vanished — the debug route must be registered only through the gated helper")
        }
        body := text[funcStart:]
        if end := strings.Index(body, "\n}\n"); end >= 0 {
                body = body[:end]
        }
        for _, marker := range []string{
                `os.Getenv("APP_DEBUG") != "true"`,
                "h.auth.Middleware(auth.PlatformRealm)",
                "requirePlatformSuperAdmin()",
        } {
                if !strings.Contains(body, marker) {
                        t.Fatalf("registerDebugSchema lost %q — AUD-016 requires env + platform auth + super-admin, all three", marker)
                }
        }
}
