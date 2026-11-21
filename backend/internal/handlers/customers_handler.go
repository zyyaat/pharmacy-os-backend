package handlers

// Customer accounts (حسابات العملاء / البيع الآجل) + low-stock alerts.
//
// Money here is money.Piastres (integer piastres, 1 EGP = 100 piastres).
// A customer's balance is DERIVED, never stored:
//
//      balance = Σ(credit sales − their returns) − Σ(payments)
//
// so refunds of credit invoices automatically reduce the debt, and a
// payment can never disagree with the invoices it settles.
// Every query is pharmacy-scoped (explicit WHERE + RLS policies mirroring
// the sales tables, migration 19).

import (
        "context"
        "encoding/base64"
        "errors"
        "log"
        "net/http"
        "strconv"
        "strings"
        "time"

        "github.com/gin-gonic/gin"
        "github.com/jackc/pgx/v5"
        "github.com/pharmacy-os/backend/internal/auth"
        "github.com/pharmacy-os/backend/internal/money"
)

const maxCustomerNameLen = 120
const maxCustomerPhoneLen = 20
const maxPaymentNoteLen = 200

// R-8 paging bounds for the customer list. The old in-query CASE caps
// (debts=20، عادي=200) become the DEFAULTS so existing clients see zero
// behavior change؛ limit/offset turn the list into real pages with a true
// total، وسقف 1000 يمنع أي limit عدائي من توسيع الاستعلام.
const (
        customersDefaultLimit      = 200
        customersDebtsDefaultLimit = 20
        customersMaxLimit          = 1000
)

// ---------------------------------------------------------------------------
// List customers: GET /pharmacy/customers?search=[&debts=1]
// debts=1 → العملاء الذين عليهم رصيد مستحق فقط (الأكثر مديونية أولاً) — يغذي جرس الإشعارات.
// ---------------------------------------------------------------------------

func (h *Handler) ListPharmacyCustomers(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        search := strings.TrimSpace(c.Query("search"))
        debtsOnly := c.Query("debts") == "1"

        // R-8: real pages with a true total. The debts default stays 20 (the
        // notification bell reads exactly that), the plain default stays 200.
        defaultLimit := customersDefaultLimit
        if debtsOnly {
                defaultLimit = customersDebtsDefaultLimit
        }
        limit, offset := parseListPaging(c, defaultLimit, customersMaxLimit)

        // total mirrors the list's WHERE. Only the debts branch needs the shared
        // balance expression — counting the plain list from customers alone keeps
        // the correlated sales/returns subquery out of the every-page path.
        var total int64
        if debtsOnly {
                err := h.db.QueryRow(c.Request.Context(), `
                        SELECT count(*) FROM (
                                SELECT c.id AS id, `+customerBalanceExpr+` AS balance
                                FROM customers c
                                WHERE c.pharmacy_id = $1
                                  AND ($2 = '' OR c.name ILIKE '%' || $2 || '%' OR c.phone LIKE '%' || $2 || '%')
                        ) t
                        WHERE t.balance > 0
                `, pharmacyID, search).Scan(&total)
                if err != nil {
                        log.Printf("[CUSTOMERS] debts count failed: %v", err)
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "customers_query_failed", "message": "تعذر قراءة حسابات العملاء"})
                        return
                }
        } else {
                err := h.db.QueryRow(c.Request.Context(), `
                        SELECT count(*) FROM customers c
                        WHERE c.pharmacy_id = $1
                          AND ($2 = '' OR c.name ILIKE '%' || $2 || '%' OR c.phone LIKE '%' || $2 || '%')
                `, pharmacyID, search).Scan(&total)
                if err != nil {
                        log.Printf("[CUSTOMERS] list count failed: %v", err)
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "customers_query_failed", "message": "تعذر قراءة حسابات العملاء"})
                        return
                }
        }

        // Balance expression + scan live in sync_rows.go — shared with the
        // delta-sync endpoint so the two can never drift apart.
        rows, err := h.db.Query(c.Request.Context(), `
                SELECT t.id, t.name, t.phone, t.created_at, t.balance FROM (
                SELECT c.id::text AS id, c.name::text AS name, COALESCE(c.phone::text, '') AS phone, c.created_at AS created_at,
                       `+customerBalanceExpr+` AS balance
                FROM customers c
                WHERE c.pharmacy_id = $1
                  AND ($2 = '' OR c.name ILIKE '%' || $2 || '%' OR c.phone LIKE '%' || $2 || '%')
                ) t
                WHERE $3 = false OR t.balance > 0
                ORDER BY (CASE WHEN $3 THEN t.balance ELSE NULL::int8 END) DESC NULLS LAST, t.created_at DESC
                LIMIT $4 OFFSET $5
        `, pharmacyID, search, debtsOnly, limit, offset)
        if err != nil {
                log.Printf("[CUSTOMERS] list failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "customers_query_failed", "message": "تعذر قراءة حسابات العملاء"})
                return
        }
        defer rows.Close()

        customers := make([]gin.H, 0)
        for rows.Next() {
                customer, err := scanCustomerRow(rows)
                if err != nil {
                        log.Printf("[CUSTOMERS] list scan failed: %v", err)
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "customers_query_failed", "message": "تعذر قراءة حسابات العملاء"})
                        return
                }
                customers = append(customers, customer)
        }
        if err := rows.Err(); err != nil {
                log.Printf("[CUSTOMERS] list rows failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "customers_query_failed", "message": "تعذر قراءة حسابات العملاء"})
                return
        }
        // Additive keys only: pre-R-8 clients read data.customers and ignore the rest.
        c.JSON(http.StatusOK, gin.H{"data": gin.H{"customers": customers}, "total": total, "limit": limit, "offset": offset})
}

// ---------------------------------------------------------------------------
// Create customer: POST /pharmacy/customers {name, phone?}
// ---------------------------------------------------------------------------

type customerRequest struct {
        Name  string `json:"name"`
        Phone string `json:"phone"`
}

func (h *Handler) CreatePharmacyCustomer(c *gin.Context) {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.PharmacyID == "" || principal.ID == "" {
                c.JSON(http.StatusForbidden, gin.H{"error": "pharmacy_mutation_account_required", "message": "حساب مدير أو موظف صيدلية مطلوب"})
                return
        }
        var request customerRequest
        if err := c.ShouldBindJSON(&request); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_customer", "message": "بيانات العميل غير صحيحة"})
                return
        }
        name := strings.TrimSpace(request.Name)
        phone := strings.TrimSpace(request.Phone)
        if name == "" || len(name) > maxCustomerNameLen {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_customer_name", "message": "اسم العميل مطلوب (حتى 120 حرفاً)"})
                return
        }
        if len(phone) > maxCustomerPhoneLen {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_customer_phone", "message": "رقم الهاتف طويل جداً"})
                return
        }
        employeeID, _ := auth.ActorIDs(principal)

        var id string
        var createdAt time.Time
        // phone اختياري في الواجهة — عمود customers.phone NOT NULL، فنمرر ''
        // بدل NULL (كان يفشل 500 لأي عميل بلا رقم هاتف)
        err := h.db.QueryRow(c.Request.Context(), `
                INSERT INTO customers (pharmacy_id, name, phone, created_by)
                VALUES ($1, $2, COALESCE(NULLIF($3, ''), ''), NULLIF($4, '')::uuid)
                RETURNING id::text, created_at
        `, principal.PharmacyID, name, phone, employeeID).Scan(&id, &createdAt)
        if err != nil {
                log.Printf("[CUSTOMERS] create failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "customer_create_failed", "message": "تعذر إضافة العميل"})
                return
        }
        c.JSON(http.StatusCreated, gin.H{"data": gin.H{
                "customer": gin.H{"id": id, "name": name, "phone": phone, "balance_piastres": 0, "created_at": createdAt},
        }})
}

// ---------------------------------------------------------------------------
// Update customer: PUT /pharmacy/customers/:id {name?, phone?}
// ---------------------------------------------------------------------------

func (h *Handler) UpdatePharmacyCustomer(c *gin.Context) {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.PharmacyID == "" || principal.ID == "" {
                c.JSON(http.StatusForbidden, gin.H{"error": "pharmacy_mutation_account_required", "message": "حساب مدير أو موظف صيدلية مطلوب"})
                return
        }
        customerID := strings.TrimSpace(c.Param("id"))
        if !isUUID(customerID) {
                c.JSON(http.StatusNotFound, gin.H{"error": "customer_not_found", "message": "العميل غير موجود"})
                return
        }
        var request customerRequest
        if err := c.ShouldBindJSON(&request); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_customer", "message": "بيانات العميل غير صحيحة"})
                return
        }
        name := strings.TrimSpace(request.Name)
        phone := strings.TrimSpace(request.Phone)
        if name == "" || len(name) > maxCustomerNameLen {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_customer_name", "message": "اسم العميل مطلوب (حتى 120 حرفاً)"})
                return
        }
        if len(phone) > maxCustomerPhoneLen {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_customer_phone", "message": "رقم الهاتف طويل جداً"})
                return
        }

        tag, err := h.db.Exec(c.Request.Context(), `
                UPDATE customers SET name = $3, phone = NULLIF($4, '')
                WHERE id = $1::uuid AND pharmacy_id = $2
        `, customerID, principal.PharmacyID, name, phone)
        if err != nil {
                log.Printf("[CUSTOMERS] update failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "customer_update_failed", "message": "تعذر حفظ بيانات العميل"})
                return
        }
        if tag.RowsAffected() == 0 {
                c.JSON(http.StatusNotFound, gin.H{"error": "customer_not_found", "message": "العميل غير موجود"})
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{"customer": gin.H{"id": customerID, "name": name, "phone": phone}}})
}

// ---------------------------------------------------------------------------
// Statement: GET /pharmacy/customers/:id/statement
// Entries are merged chronologically; balance = Σ credit dues − Σ payments.
// ---------------------------------------------------------------------------

func (h *Handler) GetPharmacyCustomerStatement(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        customerID := strings.TrimSpace(c.Param("id"))
        if !isUUID(customerID) {
                c.JSON(http.StatusNotFound, gin.H{"error": "customer_not_found", "message": "العميل غير موجود"})
                return
        }

        var customerName, customerPhone string
        err := h.db.QueryRow(c.Request.Context(), `
                SELECT name::text, COALESCE(phone::text, '') FROM customers
                WHERE id = $1::uuid AND pharmacy_id = $2
        `, customerID, pharmacyID).Scan(&customerName, &customerPhone)
        if errors.Is(err, pgx.ErrNoRows) {
                c.JSON(http.StatusNotFound, gin.H{"error": "customer_not_found", "message": "العميل غير موجود"})
                return
        }
        if err != nil {
                log.Printf("[CUSTOMERS] statement read failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "customers_query_failed", "message": "تعذر قراءة كشف الحساب"})
                return
        }

        // ---------------------------------------------------------------------------
        // R-6: keyset pagination "sync-style" (house pattern of
        // /pharmacy/sync): a server-clock cursor, a bounded page, an
        // opening balance that makes running balances correct on ANY page.
        // The per-invoice correlated SUM is gone (one grouped LEFT JOIN
        // over sale_returns), sales+payments ride ONE UNION ALL ordered by
        // (occurred_at, kind, id) — deterministic across pages — and the
        // O(n^2) selection merge is gone with it. Wire contract: "entries"
        // and "balance_piastres" keep their meaning (balance at the end of
        // the returned page; identical to the old full-history value when
        // no cursor is used); "has_more" and "next_cursor" are additive.
        // ---------------------------------------------------------------------------

        const (
                statementDefaultLimit = 500
                statementMaxLimit     = 1000
        )
        limit := statementDefaultLimit
        if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
                if n, err := strconv.Atoi(raw); err == nil && n > 0 {
                        limit = n
                }
        }
        if limit > statementMaxLimit {
                limit = statementMaxLimit
        }

        // unionSQL is the merged ledger; both the page and (when a cursor
        // is present) the opening-balance aggregate read this exact shape.
        const unionSQL = `
                SELECT e.kind, e.id, e.invoice_number, e.status, e.amount,
                       e.returned, e.due, e.payment, e.note, e.occurred_at
                FROM (
                        SELECT 'credit_sale'::text AS kind, s.id::text AS id,
                               s.invoice_number::int8 AS invoice_number,
                               s.status::text AS status,
                               s.total_amount::int8 AS amount,
                               COALESCE(r.returned, 0)::int8 AS returned,
                               GREATEST(s.total_amount::int8 - COALESCE(r.returned, 0)::int8, 0)::int8 AS due,
                               NULL::int8 AS payment,
                               NULL::text AS note,
                               s.created_at AS occurred_at
                        FROM sales s
                        LEFT JOIN (
                                SELECT sale_id, SUM(total_amount_piastres) AS returned
                                FROM sale_returns
                                GROUP BY sale_id
                        ) r ON r.sale_id = s.id
                        WHERE s.customer_id = $1::uuid AND s.payment_type = 'credit'
                        UNION ALL
                        SELECT 'payment'::text, p.id::text, NULL::int8, NULL::text,
                               NULL::int8, NULL::int8, NULL::int8, p.amount::int8,
                               COALESCE(p.note::text, ''), p.created_at
                        FROM customer_payments p
                        WHERE p.customer_id = $1::uuid
                ) e`

        // Cursor: base64url("RFC3339Nano|kind|id") of the LAST entry of the
        // previous page — a proper keyset tuple, so entries sharing a
        // timestamp never straddle a page boundary invisibly.
        var cursorTS time.Time
        var cursorKind, cursorID string
        if raw := strings.TrimSpace(c.Query("after")); raw != "" {
                decoded, err := base64.RawURLEncoding.DecodeString(raw)
                if err != nil {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_cursor", "message": "مؤشر التصفح غير صالح"})
                        return
                }
                parts := strings.SplitN(string(decoded), "|", 3)
                if len(parts) != 3 {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_cursor", "message": "مؤشر التصفح غير صالح"})
                        return
                }
                ts, err := time.Parse(time.RFC3339Nano, parts[0])
                if err != nil {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_cursor", "message": "مؤشر التصفح غير صالح"})
                        return
                }
                cursorTS, cursorKind, cursorID = ts, parts[1], parts[2]
        }

        // Opening balance: everything at-or-before the cursor tuple, so the
        // running balance of a MIDDLE page is exactly what a full-history
        // replay from day one would have produced.
        running := money.Piastres(0)
        if !cursorTS.IsZero() {
                var opening *int64
                err := h.db.QueryRow(c.Request.Context(), unionSQL+`
                        WHERE (e.occurred_at, e.kind, e.id) <= ($2::timestamptz, $3::text, $4::text)
                `, customerID, cursorTS, cursorKind, cursorID).Scan(&opening)
                if err != nil {
                        log.Printf("[CUSTOMERS] statement opening balance failed: %v", err)
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "customers_query_failed", "message": "تعذر قراءة كشف الحساب"})
                        return
                }
                if opening != nil {
                        running = money.Piastres(*opening)
                }
        }

        // One page (limit+1 sentinel reveals has_more without a COUNT).
        rows, err := h.db.Query(c.Request.Context(), unionSQL+`
                WHERE ($2::text = '' OR (e.occurred_at, e.kind, e.id) > ($3::timestamptz, $2::text, $4::text))
                ORDER BY e.occurred_at ASC, e.kind ASC, e.id ASC
                LIMIT $5::int
        `, customerID, cursorKind, cursorTS, cursorID, limit+1)
        if err != nil {
                log.Printf("[CUSTOMERS] statement page failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "customers_query_failed", "message": "تعذر قراءة كشف الحساب"})
                return
        }
        defer rows.Close()

        payload := make([]gin.H, 0, limit)
        hasMore := false
        var lastTS time.Time
        var lastKind, lastID string
        for rows.Next() {
                var kind, id string
                var status *string
                var invoiceNumber *int64
                var amount, returned, due, payment *int64
                var note *string
                var occurredAt time.Time
                if err := rows.Scan(&kind, &id, &invoiceNumber, &status, &amount,
                        &returned, &due, &payment, &note, &occurredAt); err != nil {
                        log.Printf("[CUSTOMERS] statement page scan failed: %v", err)
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "customers_query_failed", "message": "تعذر قراءة كشف الحساب"})
                        return
                }
                if len(payload) == limit {
                        // sentinel row (the limit+1th): the page is full and
                        // at least one further entry exists; it is not emitted
                        hasMore = true
                        break
                }
                if kind == "credit_sale" {
                        dueAmt := money.Piastres(0)
                        if due != nil {
                                dueAmt = money.Piastres(*due)
                        }
                        running = running.Add(dueAmt)
                        payload = append(payload, gin.H{
                                "kind":                     "credit_sale",
                                "id":                       id,
                                "invoice_number":           invoiceNumber,
                                "status":                   status,
                                "amount_piastres":          amount,
                                "returned_amount_piastres": returned,
                                "due_amount_piastres":      dueAmt,
                                "created_at":               occurredAt,
                                "balance_piastres":         running,
                        })
                } else {
                        payAmt := money.Piastres(0)
                        if payment != nil {
                                payAmt = money.Piastres(*payment)
                        }
                        running = running - payAmt
                        payload = append(payload, gin.H{
                                "kind":            "payment",
                                "id":              id,
                                "amount_piastres": payAmt,
                                "note":            note,
                                "created_at":      occurredAt,
                                "balance_piastres": running,
                        })
                }
                lastTS, lastKind, lastID = occurredAt, kind, id
        }
        if err := rows.Err(); err != nil {
                log.Printf("[CUSTOMERS] statement page rows failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "customers_query_failed", "message": "تعذر قراءة كشف الحساب"})
                return
        }
        rows.Close()

        // next_cursor = the tuple of the LAST EMITTED entry; the next page
        // resumes strictly after it (the sentinel row, if any, is re-fetched
        // there, so ties on the same timestamp can never straddle pages).
        nextCursor := ""
        if hasMore && !lastTS.IsZero() {
                nextCursor = base64.RawURLEncoding.EncodeToString(
                        []byte(lastTS.Format(time.RFC3339Nano) + "|" + lastKind + "|" + lastID))
        }

        c.JSON(http.StatusOK, gin.H{"data": gin.H{
                "customer":         gin.H{"id": customerID, "name": customerName, "phone": customerPhone},
                "entries":          payload,
                "balance_piastres": running,
                "has_more":         hasMore,
                "next_cursor":      nextCursor,
        }})
}

// ---------------------------------------------------------------------------
// Record a payment (تحصيل): POST /pharmacy/customers/:id/payments
// ---------------------------------------------------------------------------

type customerPaymentRequest struct {
        AmountPiastres int64  `json:"amount_piastres"`
        Note           string `json:"note"`
}

func (h *Handler) CreatePharmacyCustomerPayment(c *gin.Context) {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.PharmacyID == "" || principal.ID == "" {
                c.JSON(http.StatusForbidden, gin.H{"error": "pharmacy_mutation_account_required", "message": "حساب مدير أو موظف صيدلية مطلوب"})
                return
        }
        customerID := strings.TrimSpace(c.Param("id"))
        if !isUUID(customerID) {
                c.JSON(http.StatusNotFound, gin.H{"error": "customer_not_found", "message": "العميل غير موجود"})
                return
        }
        var request customerPaymentRequest
        if err := c.ShouldBindJSON(&request); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_payment", "message": "بيانات الدفعة غير صحيحة"})
                return
        }
        note := strings.TrimSpace(request.Note)
        if len(note) > maxPaymentNoteLen {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_payment", "message": "ملاحظة الدفعة طويلة جداً"})
                return
        }
        if request.AmountPiastres <= 0 {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_payment", "message": "قيمة الدفعة يجب أن تكون أكبر من صفر"})
                return
        }
        employeeID, companyUserID := auth.ActorIDs(principal)

        tx, err := h.db.Begin(c.Request.Context())
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "payment_failed", "message": "تعذر تسجيل الدفعة"})
                return
        }
        defer func() { _ = tx.Rollback(c.Request.Context()) }()
        if _, err := tx.Exec(c.Request.Context(), `
                SELECT set_config('app.current_pharmacy_id', $1, true),
                       set_config('app.current_user_id', $2, true)
        `, principal.PharmacyID, principal.ID); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "payment_failed", "message": "تعذر تسجيل الدفعة"})
                return
        }

        // Lock the customer row so concurrent payments serialize cleanly.
        var customerName string
        err = tx.QueryRow(c.Request.Context(), `
                SELECT name::text FROM customers
                WHERE id = $1::uuid AND pharmacy_id = $2
                FOR UPDATE
        `, customerID, principal.PharmacyID).Scan(&customerName)
        if errors.Is(err, pgx.ErrNoRows) {
                c.JSON(http.StatusNotFound, gin.H{"error": "customer_not_found", "message": "العميل غير موجود"})
                return
        }
        if err != nil {
                log.Printf("[CUSTOMERS] payment customer lock failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "payment_failed", "message": "تعذر تسجيل الدفعة"})
                return
        }

        var paymentID string
        var createdAt time.Time
        if err := tx.QueryRow(c.Request.Context(), `
                INSERT INTO customer_payments (pharmacy_id, customer_id, amount, note, employee_id, company_user_id)
                VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, '')::uuid, NULLIF($6, '')::uuid)
                RETURNING id::text, created_at
        `, principal.PharmacyID, customerID, request.AmountPiastres, note, employeeID, companyUserID).
                Scan(&paymentID, &createdAt); err != nil {
                log.Printf("[CUSTOMERS] payment insert failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "payment_failed", "message": "تعذر تسجيل الدفعة"})
                return
        }

        balance, err := customerBalance(c, tx, principal.PharmacyID, customerID)
        if err != nil {
                log.Printf("[CUSTOMERS] payment balance failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "payment_failed", "message": "تعذر حساب الرصيد"})
                return
        }
        if err := tx.Commit(c.Request.Context()); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "payment_failed", "message": "تعذر تسجيل الدفعة"})
                return
        }
        c.JSON(http.StatusCreated, gin.H{"data": gin.H{
                "payment_id":       paymentID,
                "balance_piastres": balance,
        }})
}

// customerBalance derives the live balance on the open transaction:
// Σ(credit invoice dues) − Σ(payments).
func customerBalance(ctx context.Context, tx pgx.Tx, pharmacyID, customerID string) (money.Piastres, error) {
        var credit, paid money.Piastres
        err := tx.QueryRow(ctx, `
                SELECT
                    COALESCE((
                        SELECT SUM(s.total_amount - COALESCE(r.total, 0))::int8
                        FROM sales s
                        LEFT JOIN (
                            SELECT sale_id, SUM(total_amount_piastres) AS total
                            FROM sale_returns
                            GROUP BY sale_id
                        ) r ON r.sale_id = s.id
                        WHERE s.customer_id = $1 AND s.payment_type = 'credit'
                    ), 0),
                    COALESCE((
                        SELECT SUM(p.amount)::int8
                        FROM customer_payments p
                        WHERE p.customer_id = $1
                    ), 0)
                FROM (SELECT 1) one
        `, customerID).Scan(&credit, &paid)
        if err != nil {
                return 0, err
        }
        return credit - paid, nil
}

// ---------------------------------------------------------------------------
// Low-stock alerts: GET /pharmacy/inventory/low-stock
// Business rule (pharmacist's definition): حد الطلب is counted in FULL BOXES
// only — leftover strips of an opened box never count toward the threshold.
// A product alerts when floor(total base quantity / units_per_box) is strictly
// below its min_stock_level. The header bell fetches this so the pharmacist
// can reorder in time. Sorted by urgency (closest to zero first).
// ---------------------------------------------------------------------------

func (h *Handler) GetPharmacyLowStock(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        rows, err := h.db.Query(c.Request.Context(), `
                SELECT pp.id::text,
                       COALESCE(gp.name::text, ''),
                       COALESCE(gp.strength::text, ''),
                       COALESCE(pp.barcode_override::text, gp.barcode::text, ''),
                       COALESCE(pp.packaging_type::text, ''),
                       COALESCE(pp.units_per_box::int8, 1),
                       GREATEST(FLOOR(COALESCE(SUM(ci.quantity), 0) / GREATEST(COALESCE(pp.units_per_box, 1), 1)), 0)::int8,
                       GREATEST(COALESCE(SUM(ci.quantity), 0), 0)::int8 % GREATEST(COALESCE(pp.units_per_box, 1), 1)::int8,
                       pp.min_stock_level::int8
                FROM pharmacy_products pp
                LEFT JOIN global_products gp ON gp.id = pp.global_product_id
                LEFT JOIN current_inventory ci ON ci.pharmacy_product_id = pp.id
                WHERE pp.pharmacy_id = $1 AND pp.is_active = true AND pp.min_stock_level > 0
                GROUP BY pp.id, pp.name_override, gp.name, gp.strength, pp.barcode_override, gp.barcode,
                         pp.packaging_type, pp.units_per_box, pp.min_stock_level
                HAVING GREATEST(FLOOR(COALESCE(SUM(ci.quantity), 0) / GREATEST(COALESCE(pp.units_per_box, 1), 1)), 0) < pp.min_stock_level
                ORDER BY (GREATEST(FLOOR(COALESCE(SUM(ci.quantity), 0) / GREATEST(COALESCE(pp.units_per_box, 1), 1)), 0)::numeric
                          / GREATEST(pp.min_stock_level, 1)) ASC,
                         COALESCE(pp.name_override, gp.name) ASC
                LIMIT 50
        `, pharmacyID)
        if err != nil {
                log.Printf("[LOWSTOCK] query failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "low_stock_query_failed", "message": "تعذر قراءة أصناف المخزون المنخفض"})
                return
        }
        defer rows.Close()

        items := make([]gin.H, 0)
        for rows.Next() {
                var id, name, strength, barcode, packagingType string
                var unitsPerBox, fullBoxes, strips, minStock int64
                if err := rows.Scan(&id, &name, &strength, &barcode, &packagingType, &unitsPerBox, &fullBoxes, &strips, &minStock); err != nil {
                        log.Printf("[LOWSTOCK] scan failed: %v", err)
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "low_stock_query_failed", "message": "تعذر قراءة أصناف المخزون المنخفض"})
                        return
                }
                items = append(items, gin.H{
                        "pharmacy_product_id": id,
                        "product_name":        name,
                        "strength":            strength,
                        "barcode":             barcode,
                        "packaging_type":      packagingType,
                        "units_per_box":       unitsPerBox,
                        "full_boxes":          fullBoxes,
                        "strips":              strips,
                        "min_stock_level":     minStock,
                })
        }
        if err := rows.Err(); err != nil {
                log.Printf("[LOWSTOCK] rows failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "low_stock_query_failed", "message": "تعذر قراءة أصناف المخزون المنخفض"})
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{"items": items, "total": len(items)}})
}
