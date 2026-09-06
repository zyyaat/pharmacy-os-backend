---
name: Replit Go publishing
description: Environment constraints encountered when running and publishing this imported Go API on Replit.
---

Imported Go services can declare a newer toolchain than the workspace's default module. Pin an available compatible Go module, set `GOTOOLCHAIN=local`, and use vendored dependencies when the package firewall blocks automatic toolchain downloads.

**Why:** Automatic toolchain download failed when the imported project declared Go 1.25 but the workspace initially exposed Go 1.21 and disabled checksum verification. An explicitly installed Go 1.25 module and vendor mode made local and workflow builds deterministic.

**How to apply:** Check available Go modules early, pin the selected module in `.replit`, and make run commands local/vendor-based. Keep deployment settings explicit: compile a production binary and run that binary. Avoid output names or paths excluded by `.gitignore`, because the build output may be absent when the publish runtime starts.