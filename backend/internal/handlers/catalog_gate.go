package handlers

// Catalog identity edit contract (migration 48 — «المنتج ملك الصيدلية»).
//
// The OLD T-B1 boundary (refuse identity writes on platform-governed rows)
// is retired. The shared-catalog write boundary it guarded against moved
// one layer down: pharmacy edits of name / generic name / barcode /
// strength / dosage form now land in the pharmacy_products override
// columns (migration 48) and global_products is NEVER written from
// pharmacy context — not on governed rows, not on crowd-sourced rows, not
// from any endpoint. Other tenants and the platform libraries always keep
// the official identity; the editing pharmacy sees its local naming
// through the COALESCE lens every read shares:
//
//	effective = COALESCE(pp.<field>_override, gp.<field>)
//
// catalogIdentity + catalogFieldsChanged remain the shared vocabulary for
// «did the edit form touch any identity field» — the edit handler uses
// them to decide whether an override write (and its barcode-uniqueness
// check) is needed at all; a pure price/packaging/stock edit never pays
// for either.

// catalogIdentity is the product presentation slice the pharmacy edit form
// touches. All strings arrive through the same COALESCE lens the GET
// endpoint reads ('' for NULL, 'tablet' for a NULL dosage form) and are
// already trimmed/normalized on the request side.
type catalogIdentity struct {
        name        string
        genericName string
        barcode     string
        strength    string
        dosageForm  string
}

// catalogFieldsChanged reports whether an edit request mutates any
// catalog identity field. Price/packaging/stock settings are
// pharmacy-owned (pharmacy_products) and are never part of this
// comparison — a pure price edit must pass without touching overrides.
func catalogFieldsChanged(requested, stored catalogIdentity) bool {
        return requested.name != stored.name ||
                requested.genericName != stored.genericName ||
                requested.barcode != stored.barcode ||
                requested.strength != stored.strength ||
                requested.dosageForm != stored.dosageForm
}
