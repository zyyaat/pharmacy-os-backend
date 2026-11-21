-- Migration 49: invoice identity snapshot («السجل وثيقة مجمّدة»)
--
-- The invoice is a document created at a moment in time: it keeps the price
-- it was sold at (sale_items.unit_price since migration 11), the quantity
-- interpretation of that moment (sale_quantity + units_per_box_snapshot
-- since migration 24) — and from today, the product identity of that
-- moment. Until now the name/barcode/strength were read LIVE from the
-- catalog at display time, so renaming or re-barcoding a product rewrote
-- yesterday's invoices. The pharmacy's records must never move under an
-- edit: each record carries its own name, its own code, its own price.
--
-- New nullable columns on sale_items, written at sale time with the
-- EFFECTIVE identity of the moment (pharmacy-local override if the
-- pharmacist had one, else the official catalog value — migration 45):
--   product_name_snapshot      what the invoice line displayed
--   product_barcode_snapshot   what the scanner matched
--   product_strength_snapshot  what the label showed
--
-- Legacy rows stay NULL on purpose and keep reading through the live join
-- (the same documented legacy fallback contract as migration 24's
-- quantity snapshot): no backfill, no reinterpretation of history.
-- Display and search read COALESCE(snapshot, live) so old and new rows
-- render identically.

ALTER TABLE sale_items
    ADD COLUMN IF NOT EXISTS product_name_snapshot VARCHAR(255),
    ADD COLUMN IF NOT EXISTS product_barcode_snapshot VARCHAR(100),
    ADD COLUMN IF NOT EXISTS product_strength_snapshot VARCHAR(100);

COMMENT ON COLUMN sale_items.product_name_snapshot IS
    'Effective product name at sale time; NULL = pre-snapshot row (legacy fallback: live catalog read).';
COMMENT ON COLUMN sale_items.product_barcode_snapshot IS
    'Effective product barcode at sale time; NULL = pre-snapshot row.';
COMMENT ON COLUMN sale_items.product_strength_snapshot IS
    'Effective product strength at sale time; NULL = pre-snapshot row.';
