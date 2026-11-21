package handlers

// Catalog identity edit contract (migration 48 — «المنتج ملك الصيدلية»).
//
// The retired T-B1 gate (catalogRowIsPlatformGoverned → 403) is gone: a
// pharmacy identity edit never touches global_products at all — it writes
// the pharmacy_products override columns, so the question «is this row
// governed?» no longer exists on any pharmacy write path. What remains is
// the cheap pre-write classification the edit handler uses:
//
//   - catalogFieldsChanged decides whether the request touches ANY identity
//     field (name/generic/barcode/strength/dosage form) — only then does
//     the handler pay for the override write and the per-pharmacy barcode
//     uniqueness check;
//   - a pure pharmacy_products edit (prices/packaging/stock/active) must
//     NOT trip the comparison: it is false when every identity field
//     matches through the same COALESCE lens the GET endpoint reads.

import "testing"

func TestCatalogFieldsChanged(t *testing.T) {
	stored := catalogIdentity{
		name:        "Panadol Extra 24tab",
		genericName: "paracetamol",
		barcode:     "6221048200123",
		strength:    "500mg",
		dosageForm:  "tablet",
	}

	cases := []struct {
		name        string
		mutate      func(i *catalogIdentity)
		wantChanges bool // true = identity write (override + barcode check)
	}{
		{
			name:        "identical_identity_is_not_an_identity_write",
			mutate:      func(i *catalogIdentity) {},
			wantChanges: false,
		},
		{
			name:        "name_change_is_an_identity_write",
			mutate:      func(i *catalogIdentity) { i.name = "Panadol Extra 48tab" },
			wantChanges: true,
		},
		{
			name:        "barcode_change_is_an_identity_write",
			mutate:      func(i *catalogIdentity) { i.barcode = "6221048200999" },
			wantChanges: true,
		},
		{
			name:        "strength_change_is_an_identity_write",
			mutate:      func(i *catalogIdentity) { i.strength = "1000mg" },
			wantChanges: true,
		},
		{
			name:        "generic_name_change_is_an_identity_write",
			mutate:      func(i *catalogIdentity) { i.genericName = "paracetamol+caffeine" },
			wantChanges: true,
		},
		{
			name:        "dosage_form_change_is_an_identity_write",
			mutate:      func(i *catalogIdentity) { i.dosageForm = "syrup" },
			wantChanges: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			requested := stored // copy
			tc.mutate(&requested)
			got := catalogFieldsChanged(requested, stored)
			if got != tc.wantChanges {
				t.Fatalf("catalogFieldsChanged() = %v, want %v", got, tc.wantChanges)
			}
		})
	}
}

func TestCatalogFieldsChangedNullLens(t *testing.T) {
	// The stored lens mirrors the GET endpoint's COALESCE defaults: a NULL
	// generic_name reads as '' and a NULL dosage_form reads as 'tablet'.
	// The edit form echoing those defaults back must never count as an
	// identity write — otherwise every price-only edit on such a row would
	// rewrite the overrides.
	cases := []struct {
		name   string
		stored catalogIdentity
		sent   catalogIdentity
		want   bool
	}{
		{
			name:   "null_generic_echoed_as_empty_is_noop",
			stored: catalogIdentity{name: "P", genericName: "", barcode: "", strength: "", dosageForm: "tablet"},
			sent:   catalogIdentity{name: "P", genericName: "", barcode: "", strength: "", dosageForm: "tablet"},
			want:   false,
		},
		{
			name:   "null_dosage_form_echoed_as_default_is_noop",
			stored: catalogIdentity{name: "P", genericName: "", barcode: "", strength: "", dosageForm: "tablet"},
			sent:   catalogIdentity{name: "P", genericName: "", barcode: "", strength: "", dosageForm: "tablet"},
			want:   false,
		},
		{
			name:   "empty_barcode_set_to_value_is_a_write",
			stored: catalogIdentity{name: "P", genericName: "", barcode: "", strength: "", dosageForm: "tablet"},
			sent:   catalogIdentity{name: "P", genericName: "", barcode: "6221048200123", strength: "", dosageForm: "tablet"},
			want:   true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := catalogFieldsChanged(tc.sent, tc.stored)
			if got != tc.want {
				t.Fatalf("catalogFieldsChanged() = %v, want %v", got, tc.want)
			}
		})
	}
}
