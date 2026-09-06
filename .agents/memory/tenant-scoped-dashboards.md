---
name: Tenant-scoped dashboards
description: Rules for keeping dashboard and pharmacy data isolated by the authenticated session.
---

Dashboard reads must derive company and pharmacy scope from the authenticated principal; client-supplied tenant identifiers are not an authorization boundary. Empty database results are valid empty states, while query failures must remain visible as errors rather than becoming demo data.

**Why:** The imported project exposed hardcoded dashboard values and placeholder handlers, which could make one account appear to see another account's data and could hide backend failures.

**How to apply:** When adding dashboard, inventory, employee, branch, attendance, or activity endpoints, scope every query using session-derived identifiers and make the UI distinguish loading, empty, and error states.