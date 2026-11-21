package handlers

import (
	"strings"
	"testing"
)

func TestPOSReturnContract(t *testing.T) {
	// ADR-0005 (api_level 109): the POS credit-note contract split across
	// the layer boundary — the request-shape guards (item bounds, reason
	// length, key shape, duplicates) stay in the handler as protocol, the
	// whole return transaction (FOR UPDATE serialization, idempotent
	// replay, largest-remainder refunds, exact-batch restock, movement
	// chain, status machine) moved to internal/services/sales_returns.go.
	// Both halves are pinned where they now live; losing either side
	// weakens the contract.
	handlerText := mustSource(t, "sales_history_handler.go")
	for _, marker := range []string{
		// Shape guards are protocol: a malformed request never reaches
		// the service.
		"invalid_return",
		"invalid_idempotency_key",
		"duplicate_return_item",
		// The handler must delegate to the service — the SQL must
		// never creep back into the protocol layer.
		"services.CreatePOSSaleReturn(",
		"services.POSSaleDetailByID(",
	} {
		if !strings.Contains(handlerText, marker) {
			t.Fatalf("sales_history_handler.go lost %q — the POS return contract was weakened", marker)
		}
	}
	for _, forbidden := range []string{
		"FOR UPDATE",
		"INSERT INTO sale_returns",
		"sale_return_items",
		"stock_movements",
	} {
		if strings.Contains(handlerText, forbidden) {
			t.Fatalf("sales_history_handler.go still contains %q — return SQL crept back into the protocol layer (ADR-0005 rule 1)", forbidden)
		}
	}

	serviceText := mustSource(t, "../services/sales_returns.go")
	for _, marker := range []string{
		// Serialized concurrency: the invoice is locked BEFORE the
		// returned aggregates are read (fresh snapshot after the wait).
		"FOR UPDATE",
		// The credit note + its items + the exact-batch restock and
		// its movement chain.
		"INSERT INTO sale_returns",
		"INSERT INTO sale_return_items",
		"return_from_customer",
		// The idempotent replay of a retried credit note.
		"idempotency_key = $2",
	} {
		if !strings.Contains(serviceText, marker) {
			t.Fatalf("services/sales_returns.go lost %q — the POS return contract was weakened", marker)
		}
	}
	// The invoice lock must run INSIDE the return transaction BEFORE the
	// credit-note insert: begin < FOR UPDATE < INSERT INTO sale_returns.
	// The window starts at the function itself — the package doc and the
	// stage comments legitimately mention the same vocabulary.
	fnStart := strings.Index(serviceText, "func CreatePOSSaleReturn(")
	if fnStart < 0 {
		t.Fatalf("services/sales_returns.go lost CreatePOSSaleReturn — the return transaction moved or was renamed")
	}
	fnText := serviceText[fnStart:]
	begin := strings.Index(fnText, "tx, err := pool.Begin(ctx)")
	lock := strings.Index(fnText, "FOR UPDATE")
	insert := strings.Index(fnText, "INSERT INTO sale_returns")
	if begin < 0 || lock < 0 || insert < 0 || lock < begin || lock > insert {
		t.Fatalf("invoice lock drifted out of its transaction window (begin=%d lock=%d insert=%d)", begin, lock, insert)
	}
}
