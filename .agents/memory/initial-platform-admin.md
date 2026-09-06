---
name: Initial platform admin provisioning
description: Operational rule for creating the first Super Admin when production has no existing platform administrator.
---

The first production Super Admin must be provisioned through the backend's one-time startup bootstrap, not by mutating the production database through the read-only database tooling. The bootstrap is enabled only in Production by a managed password secret and stops changing data once any active Super Admin exists.

**Why:** Production SQL access through the database tooling is read-only, while platform admin routes intentionally require an existing Super Admin.

**How to apply:** Publish the backend with the temporary bootstrap secret, verify the account can log in, then remove the bootstrap secret immediately.