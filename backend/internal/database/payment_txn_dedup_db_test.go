package database

// Regression gate for migration 37 (AUD-010/032, P1): provider webhook
// dedup must be enforced AT THE DATABASE, not only by the app-side
// SELECT-then-INSERT probe. The partial unique index
// uq_payment_txn_provider_webhook makes a provider transaction id globally
// unique across webhook rows:
//
//   - the same id replayed on a DIFFERENT payment must unique-violate;
//   - non-webhook rows (sync/refund/intent) sharing the id must stay legal;
//   - rows without a provider id must stay legal (legacy + id-less events).
//
// Needs a real PostgreSQL (it runs the full migration chain). Point
// TEST_DATABASE_URL at a DISPOSABLE database:
//
//      TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/database/ -run TestPaymentTxnDedup -v

import (
        "context"
        "errors"
        "strings"
        "testing"
        "time"

        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgconn"
        "github.com/jackc/pgx/v5/pgxpool"
)

func TestPaymentTxnDedupIndex(t *testing.T) {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
        defer cancel()

        pool, err := pgxpool.New(ctx, integrationDSN(t))
        if err != nil {
                t.Fatalf("connect: %v", err)
        }
        defer pool.Close()

        if err := RunMigrations(ctx, pool); err != nil {
                t.Fatalf("apply migrations: %v", err)
        }

        // The index must exist with the exact partial predicate.
        var indexdef string
        if err := pool.QueryRow(ctx, `
                SELECT indexdef FROM pg_indexes
                WHERE schemaname = 'public' AND indexname = 'uq_payment_txn_provider_webhook'
        `).Scan(&indexdef); err != nil {
                t.Fatalf("migration 37 index missing after RunMigrations: %v", err)
        }
        // Postgres normalizes the stored indexdef (explicit ::text casts and
        // extra parentheses); normalize identically before matching so the test
        // asserts the PREDICATE, not one specific serialization of it.
        lower := strings.ToLower(indexdef)
        lower = strings.ReplaceAll(lower, "::text", "")
        lower = strings.Map(func(r rune) rune {
                if r == '(' || r == ')' {
                        return -1
                }
                return r
        }, lower)
        lower = strings.Join(strings.Fields(lower), " ")
        for _, marker := range []string{"create unique index", "provider_transaction_id", "txn_type = 'webhook'", "provider_transaction_id is not null"} {
                if !strings.Contains(lower, marker) {
                        t.Fatalf("index predicate drifted — expected %q inside %q", marker, indexdef)
                }
        }

        // Seed two payments on one company. A fresh chain database has plans
        // (migration 26 seeds 'starter') but NO companies — the suite must be
        // self-sufficient, so it creates its own company when none exists and
        // removes it afterwards (defers run LIFO: payments are cleaned before
        // the company that owns them).
        var companyID, planID string
        seedErr := pool.QueryRow(ctx, `
                SELECT c.id::text, p.id::text FROM companies c
                JOIN plans p ON p.slug = 'starter'
                LIMIT 1
        `).Scan(&companyID, &planID)
        if errors.Is(seedErr, pgx.ErrNoRows) {
                if err := pool.QueryRow(ctx, `
                        INSERT INTO companies (name, email)
                        VALUES ('Payment Dedup Test Co', 'payment-dedup-test@pharmacy-os-test.invalid')
                        RETURNING id::text
                `).Scan(&companyID); err != nil {
                        t.Fatalf("seed company: %v", err)
                }
                defer func() {
                        _, _ = pool.Exec(ctx, `DELETE FROM companies WHERE id = $1`, companyID)
                }()
                if err := pool.QueryRow(ctx, `
                        SELECT id::text FROM plans WHERE slug = 'starter' LIMIT 1
                `).Scan(&planID); err != nil {
                        t.Fatalf("starter plan missing (migration 26 seed): %v", err)
                }
        } else if seedErr != nil {
                t.Fatalf("seed lookup (companies/plans missing after migrations): %v", seedErr)
        }

        var paymentA, paymentB string
        for i, id := range []*string{&paymentA, &paymentB} {
                if err := pool.QueryRow(ctx, `
                        INSERT INTO payments (company_id, plan_id, billing_interval, amount_piastres, currency, provider, status)
                        VALUES ($1, $2, 'monthly', 10000, 'EGP', 'paymob', 'pending')
                        RETURNING id::text
                `, companyID, planID).Scan(id); err != nil {
                        t.Fatalf("seed payment %d: %v", i, err)
                }
                defer cleanupPaymentRow(ctx, pool, *id)
        }

        insertWebhook := func(paymentID, txnID string) error {
                _, err := pool.Exec(ctx, `
                        INSERT INTO payment_transactions (payment_id, txn_type, provider_transaction_id, amount_piastres, hmac_verified, payload)
                        VALUES ($1, 'webhook', $2, 10000, TRUE, '{}'::jsonb)
                `, paymentID, txnID)
                return err
        }

        // 1. First webhook row with txn X lands.
        if err := insertWebhook(paymentA, "TXN-DUP-1"); err != nil {
                t.Fatalf("first webhook insert must succeed: %v", err)
        }
        // 2. Same txn X on ANOTHER payment → the DB itself must reject it.
        err = insertWebhook(paymentB, "TXN-DUP-1")
        if err == nil {
                t.Fatalf("duplicate provider txn id on a different payment MUST unique-violate at the DB (AUD-010/032)")
        }
        var pgErr *pgconn.PgError
        if !errors.As(err, &pgErr) || pgErr.SQLState() != "23505" {
                t.Fatalf("expected a unique violation (23505), got: %v", err)
        }
        // 3. Same id under txn_type='sync' → legal (settlement sync reuses ids).
        if _, err := pool.Exec(ctx, `
                INSERT INTO payment_transactions (payment_id, txn_type, provider_transaction_id, hmac_verified, payload)
                VALUES ($1, 'sync', 'TXN-DUP-1', FALSE, '{}'::jsonb)
        `, paymentB); err != nil {
                t.Fatalf("non-webhook rows sharing the id must stay legal: %v", err)
        }
        // 4. Id-less webhook rows (legacy shape) → legal, no false collision.
        if err := insertWebhook(paymentB, ""); err != nil {
                t.Fatalf("NULL provider txn id must bypass the partial index: %v", err)
        }
}

func cleanupPaymentRow(ctx context.Context, pool *pgxpool.Pool, paymentID string) {
        _, _ = pool.Exec(ctx, `DELETE FROM payment_transactions WHERE payment_id = $1`, paymentID)
        _, _ = pool.Exec(ctx, `DELETE FROM payment_settlements WHERE payment_id = $1`, paymentID)
        _, _ = pool.Exec(ctx, `DELETE FROM payments WHERE id = $1`, paymentID)
}
