// TestLibraryBulkWholesaleDB — Task 168 gates (api_level 116): the wholesale
// («استيراد» on a whole library) flows must work at the scale of the seeded
// Egyptian guide AND keep every contract the Task-166/167 audits pinned.
// On the Task-167 shape this suite's wholesale execute was 2 statements per
// item (~27 minutes extrapolated at production RTT for 26,790 items) and its
// dedup was one un-indexable OR over the whole pair space (60.7s measured on
// 27K×500) — the gates below pin the rewritten shapes from the outside:
//
//   - preview dedup arms: containment BOTH directions (own ⊇ cand and
//     cand ⊇ own), the fuzzy arm above the suspect threshold, matches[0]
//     best-first, and NO matches on already_have candidates — rows already
//     linked to another candidate of the library are outside the link game
//     (a suggestion for them would silently re-point a committed identity),
//   - explicit execute: plan limit consumed strictly in item order, links
//     are free (they never consume the ceiling), zero official price skips
//     with no_official_price, cost price never written,
//   - wholesale execute (mode:'all'): the server derives the full item set
//     itself — every missing product created, every owned product price-
//     refreshed, zero official price skipped — same red lines,
//   - WireSkipCap: a wholesale import against a plan-capped pharmacy
//     produces 1000 wire skips + skipped_truncated (counts stay exact),
//   - sync claim guard: two added products whose names both clear the
//     auto-link threshold against the SAME barcode-less pharmacy row — the
//     row is claimed by exactly ONE twin (the Task-167 loop linked both
//     sequentially and the second silently overwrote the first while
//     counting two),
//   - the red lines: zero inventory rows, cost untouched, removed never
//     deletes.
//
// TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/services/ -run TestLibraryBulkWholesaleDB
package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pharmacy-os/backend/internal/auth"
)

func TestLibraryBulkWholesaleDB(t *testing.T) {
	ctx := context.Background()
	pool := platformStatsPool(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// ---- fixtures: account → pharmacy (EG) → branch → employee ------------
	var accountID, pharmacyID, branchID string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status)
                VALUES ($1, $2, 'active') RETURNING id::text
        `, "p168 gate "+suffix, "p168-"+suffix+"@test.local").Scan(&accountID))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, accountID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM product_libraries WHERE description = 'p168 gate'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM global_products WHERE name ILIKE 'p168 %'`)
	})
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active, country)
                VALUES ($1, $2, $3, true, 'EG') RETURNING id::text
        `, accountID, "p168-gate-ph "+suffix, "P168-"+suffix).Scan(&pharmacyID))
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO branches (pharmacy_id, name, code, is_active)
                VALUES ($1, 'main', $2, true) RETURNING id::text
        `, pharmacyID, "P168B-"+suffix).Scan(&branchID))
	var employeeID string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO employees (account_id, pharmacy_id, branch_id, email, first_name, last_name, status, permissions_initialized)
                VALUES ($1, $2, $3, $4, 'p168', 'gate', 'active', TRUE) RETURNING id::text
        `, accountID, pharmacyID, branchID, "p168-emp-"+suffix+"@test.local").Scan(&employeeID))
	employee := &auth.Principal{
		ID: employeeID, Type: auth.EmployeePrincipal,
		PharmacyID: pharmacyID, IsActive: true, EmailVerified: true,
		Email: "p168-emp-" + suffix + "@test.local", DisplayName: "p168 gate",
	}

	libraryID := makeLibrary(t, ctx, pool, "p168-lib-", "P168 Lib "+suffix, "EG", true)
	gpAlpha := makeCatalogProduct(t, ctx, pool, "p168-a-", "P168 Alpha", "7710000000017")
	gpBeta := makeCatalogProduct(t, ctx, pool, "p168-b-", "P168 Beta", "7710000000024")
	gpGamma := makeCatalogProduct(t, ctx, pool, "p168-c-", "P168 Gamma", "7710000000031")
	gpDelta := makeCatalogProduct(t, ctx, pool, "p168-d-", "P168 Delta", "7710000000048")
	gpEpsilon := makeCatalogProduct(t, ctx, pool, "p168-e-", "P168 Epsilon", "7710000000055")
	gpZero := makeCatalogProduct(t, ctx, pool, "p168-z-", "P168 Zeroprice", "")
	makeLibraryEntry(t, ctx, pool, libraryID, gpAlpha, 1000)
	makeLibraryEntry(t, ctx, pool, libraryID, gpBeta, 2000)
	makeLibraryEntry(t, ctx, pool, libraryID, gpGamma, 3000)
	makeLibraryEntry(t, ctx, pool, libraryID, gpDelta, 4000)
	makeLibraryEntry(t, ctx, pool, libraryID, gpEpsilon, 5000)
	makeLibraryEntry(t, ctx, pool, libraryID, gpZero, 0) // بيانات ناقصة — لا سعر رسمي

	// صفوف الصيدلية الخاصة — توائم الاحتواء والتشابه (كلها باركود مختلف أو
	// بلا باركود: الباركودات الكتالوجية فريدة فلا يتشارك مرشح وصف في واحد).
	gpOwnA := makeCatalogProduct(t, ctx, pool, "p168-oa-", "P168 Alpha Advance 20 tabs", "7720000000014")
	gpOwnB := makeCatalogProduct(t, ctx, pool, "p168-ob-", "p168 beta", "7720000000021")
	gpOwnC := makeCatalogProduct(t, ctx, pool, "p168-oc-", "P168 Gamla", "7720000000038")
	gpOwnD := makeCatalogProduct(t, ctx, pool, "p168-od-", "P168 Twin Seed", "")
	_, _err_ := pool.Exec(ctx, `
                INSERT INTO pharmacy_products (pharmacy_id, global_product_id, selling_price, cost_price, min_stock_level)
                VALUES ($1::uuid, $2::uuid, 999, 777, 0),
                       ($1::uuid, $3::uuid, 888, 777, 0),
                       ($1::uuid, $4::uuid, 777, 777, 0),
                       ($1::uuid, $5::uuid, 666, 777, 0)
        `, pharmacyID, gpOwnA, gpOwnB, gpOwnC, gpOwnD)
	require.NoError(t, _err_)
	var ownDRowID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpOwnD).Scan(&ownDRowID))

	// حارس الأداة: أسماء العينة يجب أن تعبر العتبات فلا يثبت الاختبار شيئًا فارغًا.
	// ملاحظة: ذراعا الاحتواء لا تعتمدان عتبة تشابه أصلًا — الاحتواء النصي
	// نفسه الإشارة الحاسمة (قد يحتوي اسمان متشابهان بدرجة trgm منخفضة).
	var simFuzzy, simTwinBest, simTwinRest float64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT similarity('P168 Gamla', 'P168 Gamma')`).Scan(&simFuzzy))
	require.GreaterOrEqual(t, simFuzzy, NameSuspectThreshold)
	require.Less(t, simFuzzy, NameAutoLinkThreshold, "التوأم الضبابي يجب أن يبقى اقتراحًا لا ربطًا واثقًا")
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT similarity('P168 Twin Seed', 'P168 Twin Seeds')`).Scan(&simTwinBest))
	require.GreaterOrEqual(t, simTwinBest, NameAutoLinkThreshold)

	// ---- 1) preview: أذرع التكرار — احتواء بالاتجاهين وتشابه وبلا مساس
	// بالمرشحين المملوكين ---------------------------------------------------
	all, err := LoadLibraryCandidates(ctx, pool, pharmacyID, libraryID, nil)
	require.NoError(t, err)
	require.Len(t, all, 6)
	require.NoError(t, AttachDedupMatches(ctx, pool, pharmacyID, all, NameSuspectThreshold))

	byName := map[string]LibraryCandidate{}
	for _, c := range all {
		byName[c.Name] = c
	}
	alpha := byName["P168 Alpha"]
	require.Equal(t, "link_existing", alpha.SuggestedAction, "احتواء: اسم الصيدلية يحتوي اسم المرشح")
	require.NotEmpty(t, alpha.Matches)
	require.Equal(t, "name_contains", alpha.Matches[0].MatchType)
	require.Equal(t, "P168 Alpha Advance 20 tabs", alpha.Matches[0].Name)

	beta := byName["P168 Beta"]
	require.Equal(t, "link_existing", beta.SuggestedAction, "احتواء معاكس: اسم المرشح يحتوي اسم الصيدلية")
	require.NotEmpty(t, beta.Matches)
	require.Equal(t, "p168 beta", beta.Matches[0].Name)

	gamma := byName["P168 Gamma"]
	require.Equal(t, "create_new", gamma.SuggestedAction,
		"التشابه بين عتبتي الشك والوثق: يُعرض للمراجعة بلا وسم ربط واثق — العقد القديم حرفيًا")
	require.NotEmpty(t, gamma.Matches, "التوأم الضبابي يُعرض للمراجعة فوق عتبة الشك")
	require.Equal(t, "name", gamma.Matches[0].MatchType)
	require.GreaterOrEqual(t, gamma.Matches[0].Score, NameSuspectThreshold)

	delta := byName["P168 Delta"]
	require.Equal(t, "create_new", delta.SuggestedAction)
	require.Empty(t, delta.Matches, "لا توائم ولا إزعاج")
	epsilon := byName["P168 Epsilon"]
	require.Equal(t, "create_new", epsilon.SuggestedAction)

	unlimited := PlanState{Used: 0, Limit: -1}

	// ---- 2) التنفيذ الصريح: السقف بترتيب العناصر والربط مجاني -------------
	out, err := ExecutePharmacyImport(ctx, pool, employee, pharmacyID, libraryID, []PharmacyImportItem{
		{GlobalProductID: gpDelta, Action: "create"},
		{GlobalProductID: gpEpsilon, Action: "create"},
		{GlobalProductID: gpAlpha, Action: "create"},
		{GlobalProductID: gpBeta, Action: "link", PharmacyProductID: strp(beta.Matches[0].PharmacyProductID)},
		{GlobalProductID: gpGamma, Action: "create"},
		{GlobalProductID: gpZero, Action: "create"},
	}, PlanState{Used: 0, Limit: 3})
	require.NoError(t, err)
	assert.Equal(t, 4, out.Applied, "ثلاثة إنشاءات + ربط مجاني")
	assert.Equal(t, 3, out.ProductsCreated)
	assert.Equal(t, 1, out.ProductsLinked)
	assert.Equal(t, 0, out.PricesUpdated)
	require.Len(t, out.Skipped, 2)
	assert.Equal(t, "plan_limit", out.Skipped[0].Reason, "السقف يُستهلك بترتيب العناصر حرفيًا")
	assert.Equal(t, gpGamma, out.Skipped[0].GlobalProductID)
	assert.Equal(t, "no_official_price", out.Skipped[1].Reason)
	assert.Equal(t, gpZero, out.Skipped[1].GlobalProductID)
	var cost int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT cost_price FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpDelta).Scan(&cost))
	assert.Equal(t, int64(0), cost, "التكلفة ملك الصيدلية — لا تُكتب أبدًا")
	var ownBCost int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT cost_price FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpBeta).Scan(&ownBCost))
	assert.Equal(t, int64(777), ownBCost, "الربط يعيد التوجيه بلا لمس التكلفة")
	var linkedPrice int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT selling_price FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpBeta).Scan(&linkedPrice))
	assert.Equal(t, int64(2000), linkedPrice, "الصف المرتبط يأخذ السعر الرسمي")

	// ---- 3) الاستيراد الجمعي mode:'all': الخادم يشتق المجموعة كاملة ------
	wholesale, err := ExecutePharmacyImportAll(ctx, pool, employee, pharmacyID, libraryID, unlimited)
	require.NoError(t, err)
	assert.Equal(t, 5, wholesale.Applied, "أربعة مملوكين تُحدَّث أسعارهم + إنشاء واحد")
	assert.Equal(t, 1, wholesale.ProductsCreated, "Gamma وحده ينشأ")
	assert.Equal(t, 4, wholesaleProductsOwned(t, pool, pharmacyID, gpAlpha, gpBeta, gpDelta, gpEpsilon))
	assert.Equal(t, 0, wholesale.PricesUpdated, "الأسعار رسمية أصلًا — لا تحديث فعلي")
	require.Len(t, wholesale.Skipped, 1)
	assert.Equal(t, "no_official_price", wholesale.Skipped[0].Reason)
	assert.False(t, wholesale.SkippedTruncated)
	var gammaPrice int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT selling_price FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpGamma).Scan(&gammaPrice))
	assert.Equal(t, int64(3000), gammaPrice)

	// ---- 4) سقف السطر: 1005 صنفًا على خطة سقفها صنف واحد ------------------
	var bulkAccount, bulkPharmacy string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status)
                VALUES ($1, $2, 'active') RETURNING id::text
        `, "p168 bulk "+suffix, "p168bulk-"+suffix+"@test.local").Scan(&bulkAccount))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, bulkAccount)
	})
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active, country)
                VALUES ($1, $2, $3, true, 'EG') RETURNING id::text
        `, bulkAccount, "p168-bulk-ph "+suffix, "P168X-"+suffix).Scan(&bulkPharmacy))
	bulkLibrary := makeLibrary(t, ctx, pool, "p168-bulk-", "P168 Bulk Lib "+suffix, "EG", true)
	_, _err_ = pool.Exec(ctx, `
                WITH gp AS (
                        INSERT INTO global_products (name, dosage_form, product_category, requires_prescription, is_verified, source)
                        SELECT 'P168 Bulk Item ' || g, 'tablet', 'medication', 'no', true, 'platform_admin'
                        FROM generate_series(1, 1005) g
                        RETURNING id
                )
                INSERT INTO library_products (library_id, global_product_id, official_price_piastres)
                SELECT $1::uuid, gp.id, 100 FROM gp
        `, bulkLibrary)
	require.NoError(t, _err_)
	capped, err := ExecutePharmacyImportAll(ctx, pool, employee, bulkPharmacy, bulkLibrary, PlanState{Used: 0, Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, capped.ProductsCreated)
	assert.Equal(t, 1, capped.Applied)
	require.Len(t, capped.Skipped, WireSkipCap, "سطر التخطيات مقصوص على السقف")
	assert.True(t, capped.SkippedTruncated, "علم القص صادق")
	for _, s := range capped.Skipped {
		assert.Equal(t, "plan_limit", s.Reason)
	}

	// ---- 5) السحب بحارس المطالبة: توأمان متنافسان على صف واحد ------------
	gpTwinBest := makeCatalogProduct(t, ctx, pool, "p168-tb-", "P168 Twin Seeds", "")
	gpTwinRest := makeCatalogProduct(t, ctx, pool, "p168-tr-", "P168 Twin Seedz", "")
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT similarity('P168 Twin Seed', 'P168 Twin Seedz')`).Scan(&simTwinRest))
	require.GreaterOrEqual(t, simTwinRest, NameAutoLinkThreshold)
	makeLibraryEntry(t, ctx, pool, libraryID, gpTwinBest, 7000)
	makeLibraryEntry(t, ctx, pool, libraryID, gpTwinRest, 8000)
	_, err = pool.Exec(ctx,
		`INSERT INTO library_changes (library_id, global_product_id, version, change_type, summary)
                 VALUES ($1::uuid, $2::uuid, 2, 'added', 'p168 twin best'),
                        ($1::uuid, $3::uuid, 2, 'added', 'p168 twin rest')`,
		libraryID, gpTwinBest, gpTwinRest)
	require.NoError(t, err)
	// تغيير سعر رسمي مملوك — السحب المجمّع يرفعه.
	_, err = pool.Exec(ctx,
		`UPDATE library_products SET official_price_piastres = 1500 WHERE library_id = $1::uuid AND global_product_id = $2::uuid`,
		libraryID, gpAlpha)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO library_changes (library_id, global_product_id, version, change_type, old_price_piastres, new_price_piastres, summary)
                 VALUES ($1::uuid, $2::uuid, 2, 'price_changed', 1000, 1500, 'p168 alpha price')`,
		libraryID, gpAlpha)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE product_libraries SET version = 2 WHERE id = $1::uuid`, libraryID)
	require.NoError(t, err)

	syncOut, err := SyncPharmacyLibrary(ctx, pool, employee, pharmacyID, libraryID, unlimited)
	require.NoError(t, err)
	assert.False(t, syncOut.NothingToDo)
	assert.Equal(t, 1, syncOut.ProductsLinked,
		"حارس المطالبة: الصف الواحد يُطالب بتوأم واحد — الحلقة القديمة كانت تربط الاثنين وتعدّ اثنين")
	assert.Equal(t, 1, syncOut.ProductsAdded, "التوأم الخاسر في المطالبة يُنشأ صفًا جديدًا")
	assert.Equal(t, 1, syncOut.PricesUpdated)
	var alphaPrice int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT selling_price FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpAlpha).Scan(&alphaPrice))
	assert.Equal(t, int64(1500), alphaPrice)
	var twinLinks int
	require.NoError(t, pool.QueryRow(ctx, `
                SELECT COUNT(*) FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id IN ($2::uuid, $3::uuid)
        `, pharmacyID, gpTwinBest, gpTwinRest).Scan(&twinLinks))
	assert.Equal(t, 2, twinLinks, "الصف المزروع يشير لأحدهما والتوأم الآخر له صف جديد")
	var ownDPointsAtTwin bool
	require.NoError(t, pool.QueryRow(ctx, `
                SELECT global_product_id IN ($2::uuid, $3::uuid) FROM pharmacy_products WHERE id = $1::uuid
        `, ownDRowID, gpTwinBest, gpTwinRest).Scan(&ownDPointsAtTwin))
	assert.True(t, ownDPointsAtTwin, "صف التوأم المزروع أُعيد توجيهه لتوأم واحد بالضبط")
	var ownDOrphans int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpOwnD).Scan(&ownDOrphans))
	assert.Equal(t, 0, ownDOrphans, "لا صف يعود ليشير لمنتج التوأم المزروع — المطالبة الواحدة")

	// ---- 6) القواعد الحمراء: صفر صفوف مخزون وصفوف المراجعة سليمة ----------
	var batches int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM inventory_batches b JOIN branches br ON br.id = b.branch_id WHERE br.pharmacy_id = $1::uuid`,
		pharmacyID).Scan(&batches))
	assert.Equal(t, 0, batches, "الاستيراد والسحب ينشئان صفر صفوف مخزون")
}

func wholesaleProductsOwned(t *testing.T, pool *pgxpool.Pool, pharmacyID string, gpIDs ...string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `
                SELECT COUNT(*) FROM pharmacy_products
                WHERE pharmacy_id = $1::uuid AND global_product_id = ANY($2::uuid[])
        `, pharmacyID, gpIDs).Scan(&n))
	return n
}
