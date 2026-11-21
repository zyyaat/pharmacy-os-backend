// ADR-0005 (api_level 107) — the first live service extraction under the
// layering policy: the platform dashboard aggregate moves verbatim out of
// platform_admin_handler.go (same query text, same scan order, same wire
// shape — pinned by TestPlatformStatsServiceDB and the unchanged route).
// From api_level 107 on, business rules and transactional SQL belong here,
// handlers stay protocol-only, and no NEW SQL is written inside
// internal/handlers (see docs/adr/ADR-0005-service-layer-policy.md).
package services

import (
        "context"

        "github.com/jackc/pgx/v5/pgxpool"
)

// PlatformStatsRow is the global platform dashboard aggregate handed to
// GetPlatformAdminStats. Field order mirrors the query columns below — the
// handler maps these 1:1 onto the response keys, so renaming or reordering
// any of them is a wire change, not a refactor (ADR-0005 rule 3: the wire
// contract is frozen during extraction; the mobile APKs are not exported
// from this repo and may lag the server for weeks).
type PlatformStatsRow struct {
        TotalCompanies     int
        ActiveCompanies    int
        SuspendedCompanies int
        TotalAccounts      int
        TotalPharmacies    int
        TotalUsers         int
        ActiveUsers        int
}

// PlatformStats returns the SaaS owner's global counters: companies by
// status, accounts, active pharmacies, and users (company users + employees,
// total and active). One round trip, read-only, no transaction needed.
//
// Task 164: companies.is_platform (the bootstrap shell hosting the super
// admin) is infrastructure — excluded from every customer counter: its
// company row, its bootstrap account, its pharmacy, and its single
// company_user (the counters used to read +1 inflated on a fresh database).
func PlatformStats(ctx context.Context, db *pgxpool.Pool) (PlatformStatsRow, error) {
        const query = `
                SELECT
                        (SELECT COUNT(*)::int FROM companies WHERE deleted_at IS NULL AND NOT is_platform),
                        (SELECT COUNT(*)::int FROM companies WHERE deleted_at IS NULL AND NOT is_platform AND status = 'active'),
                        (SELECT COUNT(*)::int FROM companies WHERE deleted_at IS NULL AND NOT is_platform AND status = 'suspended'),
                        (SELECT COUNT(*)::int FROM accounts a WHERE a.deleted_at IS NULL
                                AND NOT EXISTS (SELECT 1 FROM companies pc WHERE pc.id = a.company_id AND pc.is_platform)),
                        (SELECT COUNT(*)::int FROM pharmacies p WHERE p.is_active = true
                                AND NOT EXISTS (
                                        SELECT 1 FROM accounts pa
                                        JOIN companies pc ON pc.id = pa.company_id
                                        WHERE pa.id = p.account_id AND pc.is_platform
                                )),
                        (
                                SELECT COUNT(*)::int FROM (
                                        SELECT cu.id FROM company_users cu
                                        WHERE cu.deleted_at IS NULL
                                          AND NOT EXISTS (SELECT 1 FROM companies pc WHERE pc.id = cu.company_id AND pc.is_platform)
                                        UNION ALL
                                        SELECT id FROM employees
                                ) users
                        ),
                        (
                                SELECT COUNT(*)::int FROM (
                                        SELECT cu.id FROM company_users cu
                                        WHERE cu.deleted_at IS NULL AND cu.is_active = true
                                          AND NOT EXISTS (SELECT 1 FROM companies pc WHERE pc.id = cu.company_id AND pc.is_platform)
                                        UNION ALL
                                        SELECT id FROM employees WHERE is_active = true
                                ) users
                        )
        `

        var row PlatformStatsRow
        if err := db.QueryRow(ctx, query).Scan(
                &row.TotalCompanies, &row.ActiveCompanies, &row.SuspendedCompanies,
                &row.TotalAccounts, &row.TotalPharmacies, &row.TotalUsers, &row.ActiveUsers,
        ); err != nil {
                return PlatformStatsRow{}, err
        }
        return row, nil
}
