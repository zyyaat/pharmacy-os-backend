-- ============================================
-- Migration 47: post-seed planner statistics (Task 168 — الدليل الدوائي المصري)
-- Purpose: migration 46 bulk-inserted 27,190 global_products rows and 27,190
--          library_products rows and ended WITHOUT ANALYZE — the planner ran
--          every library flow (dedup, dedup join order, visibility joins) on
--          row estimates from before the wholesale catalog existed. On the
--          measured scratch this alone turned the wholesale dedup from
--          minutes into seconds. This migration refreshes the statistics the
--          library flows lean on; it is idempotent, data-free, and
--          transactional like the rest of the chain.
-- Safety:  ANALYZE only — no shape change, no data touch.
-- ============================================

ANALYZE global_products;
ANALYZE library_products;
ANALYZE product_libraries;
