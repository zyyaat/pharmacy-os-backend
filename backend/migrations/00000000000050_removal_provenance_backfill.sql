-- Migration 50: provenance backfill for imports that predate migration 48.
--
-- The Egypt drug library (Task 167/168) went live BEFORE the ownership
-- wave (migration 48) shipped, so any pharmacy that imported during that
-- window holds rows with source_library_id NULL — the removal feature
-- would report an honest but useless 0/0 for them. This backfill stamps
-- provenance conservatively, leaning KEEP (the owner's decision ④):
--
--   * a row is stamped ONLY when its catalog id is a member of a library
--     the pharmacy has a sync pointer for, AND
--   * the row was CREATED at or after that pointer's creation —
--     pharmacy_library_syncs.created_at is the FIRST import time (the
--     update path touches last_synced_version/synced_at only), so
--     first_added_at >= created_at means the row was minted by the import
--     or a later sync «added», never a product the pharmacy owned before
--     the library existed.
--
-- Rows created BEFORE the pointer (pre-existing products that the import
-- only price-refreshed as «already have») stay NULL: on removal they are
-- kept and detached — deleting a product the pharmacy made itself is the
-- one mistake this migration refuses to make. Idempotent: the WHERE guard
-- skips stamped rows; re-running is a no-op.

UPDATE pharmacy_products pp
SET source_library_id = s.library_id,
    updated_at = NOW()
FROM pharmacy_library_syncs s
JOIN library_products lp ON lp.library_id = s.library_id
WHERE s.pharmacy_id = pp.pharmacy_id
  AND lp.global_product_id = pp.global_product_id
  AND pp.source_library_id IS NULL
  AND pp.first_added_at >= s.created_at;

COMMENT ON COLUMN pharmacy_products.source_library_id IS
    'Library that CREATED this row (import/sync add, migration 48) or the '
    'conservative time-window backfill (migration 50: first_added_at >= the '
    'pharmacy''s first import of that library). NULL = pharmacy-owned before '
    'any library (manual create, explicit link, or pre-library product).';
