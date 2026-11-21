package handlers

import (
	"strings"
	"testing"
)

// ADR-0005 (api_level 110) — ledger item 3 contract: the central libraries
// split across the layer boundary. The protocol edge (file parsing, column
// mapping, the Arabic-reason row validator, page parsing, the frozen wire
// vocabularies) stays in these two handler files; the transactions, the
// catalog dedup, the plan-limit arithmetic, the sync pointer and the audit
// rows moved to internal/services/libraries_{platform,pharmacy}.go. Both
// halves are pinned where they now live; losing either side weakens the
// contract.
//
// The forbidden-marker lists are the SQL the two handler files carried
// before the extraction — any of them reappearing in the protocol layer is
// ADR-0005 rule 1 (no SQL in handlers) regressing.
func TestLibraryPlatformContract(t *testing.T) {
	handlerText := mustSource(t, "platform_libraries_products_handler.go")
	for _, marker := range []string{
		// Request-shape guards are protocol: a malformed row/payload never
		// reaches the service.
		"الاسم مطلوب",
		"invalid_body",
		"product_source_required",
		"invalid_price",
		"invalid_product",
		// The frozen wire vocabulary — the stage→code maps this file owns.
		"library_products_count_failed",
		"library_products_query_failed",
		"library_products_scan_failed",
		"library_product_upsert_failed",
		"catalog_barcode_query_failed",
		"catalog_name_query_failed",
		"library_change_log_failed",
		// The handler must delegate — every mutation and read goes through
		// the service.
		"services.ListLibraryProducts(",
		"services.AddLibraryProduct(",
		"services.UpdateLibraryProduct(",
		"services.RemoveLibraryProduct(",
		"services.ExecuteLibraryImport(",
		"services.ListCatalogProducts(",
		"services.UpdateCatalogProduct(",
	} {
		if !strings.Contains(handlerText, marker) {
			t.Fatalf("platform_libraries_products_handler.go lost %q — the libraries contract was weakened", marker)
		}
	}
	for _, forbidden := range []string{
		"FOR UPDATE",
		"INSERT INTO global_products",
		"INSERT INTO library_products",
		"INSERT INTO library_changes",
		"UPDATE library_products",
		"UPDATE global_products",
		"DELETE FROM library_products",
		"libraryDraftVersion",
		"logLibraryChange",
		"touchLibrary",
		"createCatalogProduct",
		"drawInternalBarcode",
	} {
		if strings.Contains(handlerText, forbidden) {
			t.Fatalf("platform_libraries_products_handler.go still contains %q — library SQL crept back into the protocol layer (ADR-0005 rule 1)", forbidden)
		}
	}

	serviceText := mustSource(t, "../services/libraries_platform.go")
	for _, marker := range []string{
		// Transaction ownership lives in the service (ADR-0005 rule 2).
		"pool.Begin(ctx)",
		"tx.Commit(ctx)",
		"FOR UPDATE",
		"INSERT INTO library_products",
		"INSERT INTO library_changes",
		"UPDATE global_products",
		// The audit rows ride the same transaction through the canonical
		// writer.
		"auditlog.Platform(",
	} {
		if !strings.Contains(serviceText, marker) {
			t.Fatalf("services/libraries_platform.go lost %q — the service half of the libraries contract was weakened", marker)
		}
	}
}

func TestLibraryPharmacyContract(t *testing.T) {
	handlerText := mustSource(t, "pharmacy_libraries_handler.go")
	for _, marker := range []string{
		// The frozen wire vocabulary of the pharmacy flows.
		"library_not_visible",
		"library_not_imported",
		"pharmacy_library_preview_failed",
		"pharmacy_library_import_failed",
		"pharmacy_library_sync_failed",
		"pharmacy_libraries_query_failed",
		"pharmacy_libraries_scan_failed",
		// Task 167 — the preview bound on wholesale (mode=all) previews.
		"preview_too_large",
		"services.PreviewCandidateCap",
		// The handler resolves the gate once and delegates — no SQL, no
		// transactions in the protocol layer.
		"services.LoadLibraryCandidates(",
		"services.AttachDedupMatches(",
		"services.ApplyPlanLimitToCandidates(",
		"services.ExecutePharmacyImport(",
		"services.SyncPharmacyLibrary(",
	} {
		if !strings.Contains(handlerText, marker) {
			t.Fatalf("pharmacy_libraries_handler.go lost %q — the pharmacy libraries contract was weakened", marker)
		}
	}
	for _, forbidden := range []string{
		"FOR UPDATE OF l",
		"INSERT INTO pharmacy_products",
		"UPDATE pharmacy_products",
		"INSERT INTO pharmacy_library_syncs",
		"UPDATE pharmacy_library_syncs",
		"libraryVisibilityWhere",
		"similarity(",
		"writeAuditLog(",
	} {
		if strings.Contains(handlerText, forbidden) {
			t.Fatalf("pharmacy_libraries_handler.go still contains %q — pharmacy library SQL crept back into the protocol layer (ADR-0005 rule 1)", forbidden)
		}
	}

	serviceText := mustSource(t, "../services/libraries_pharmacy.go")
	for _, marker := range []string{
		// The skip-reason vocabulary is a BUSINESS outcome — it lives with
		// the transaction that produces it, mapped 1:1 by the handler.
		"no_official_price",
		"plan_limit",
		"not_in_library",
		"link_conflict",
		"link_target_invalid",
		// The red lines are enforced where the transaction lives.
		"libraryVisibilityWhere",
		"FOR UPDATE OF l",
		"INSERT INTO pharmacy_products",
		"ON CONFLICT (pharmacy_id, library_id) DO UPDATE SET",
		"GREATEST(pharmacy_library_syncs.last_synced_version",
		"auditlog.Tenant(",
	} {
		if !strings.Contains(serviceText, marker) {
			t.Fatalf("services/libraries_pharmacy.go lost %q — the service half of the pharmacy libraries contract was weakened", marker)
		}
	}
}
