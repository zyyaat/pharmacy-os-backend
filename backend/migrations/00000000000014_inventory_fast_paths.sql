-- Task 122 — مسار سريع لتجميع المخزون الحالي: معالج /pharmacy/inventory كان
-- يعتمد على الفيو current_inventory الذي يستدعي calculate_batch_current_stock
-- ثلاثًا لكل صف (الكمية + حالة status + شرط WHERE) — كل استدعاء SUM على
-- stock_movements بفهرس batch_id المفرد (idx_stock_movements_batch) يقرأ
-- الأعمدة من الجدول نفسه بعد الفهرس. الفهرس الغلاف هنا (batch_id, quantity)
-- يجعل كل تجميع مسحًا للفهرس فقط (index-only scan) — مع إعادة كتابة المعالج
-- على تجميع LATERAL واحد لكل صف بدل ثلاث — زمن الرد من عشرات الثواني
-- (تعليق «جاري تحميل البيانات» الأبدي) إلى أجزاء الثانية.

CREATE INDEX IF NOT EXISTS idx_stock_movements_batch_qty
    ON stock_movements(batch_id, quantity);
