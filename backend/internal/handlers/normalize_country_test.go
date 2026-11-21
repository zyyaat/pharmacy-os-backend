package handlers

import (
	"testing"
)

// Task 166 audit — the library visibility predicate compares
// product_libraries.country_code against pharmacies.country by EXACT
// (case/space-insensitive) equality, and pharmacies.country practically
// always holds the migration-5/9 default code «EG» (the onboarding UI never
// writes the column). An admin typing «مصر» or «Egypt» used to mint a
// library NO pharmacy could ever see. The write-side canonicalization is
// the guarantee — this gate pins the aliases that promise rests on.
func TestNormalizeLibraryCountry(t *testing.T) {
	eq := func(got, want string) {
		t.Helper()
		if got != want {
			t.Fatalf("normalizeLibraryCountry: got %q, want %q", got, want)
		}
	}
	// Egypt — the spellings that matter for the Egyptian library.
	if normalizeLibraryCountry("EG") == nil || *normalizeLibraryCountry("EG") != "EG" {
		t.Fatal("EG must pass through as EG")
	}
	eq(*normalizeLibraryCountry("  eg "), "EG")
	eq(*normalizeLibraryCountry("Egypt"), "EG")
	eq(*normalizeLibraryCountry("مصر"), "EG")
	eq(*normalizeLibraryCountry("Egy"), "EG")
	// Other Arabic-region spellings an admin realistically types.
	eq(*normalizeLibraryCountry("saudi arabia"), "SA")
	eq(*normalizeLibraryCountry("KSA"), "SA")
	eq(*normalizeLibraryCountry("السعودية"), "SA")
	eq(*normalizeLibraryCountry("الامارات"), "AE")
	eq(*normalizeLibraryCountry("UAE"), "AE")
	eq(*normalizeLibraryCountry("الكويت"), "KW")
	eq(*normalizeLibraryCountry("قطر"), "QA")
	eq(*normalizeLibraryCountry("البحرين"), "BH")
	eq(*normalizeLibraryCountry("عمان"), "OM")
	eq(*normalizeLibraryCountry("الأردن"), "JO")
	// Unknown free text passes through uppercased/trimmed — no silent drops.
	eq(*normalizeLibraryCountry(" turkey "), "TURKEY")
	// Empty means universal (NULL country_code).
	if normalizeLibraryCountry("   ") != nil {
		t.Fatal("blank country must become NULL (universal library)")
	}
	if normalizeLibraryCountry("") != nil {
		t.Fatal("empty country must become NULL (universal library)")
	}
}
