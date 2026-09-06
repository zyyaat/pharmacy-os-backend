---
name: Authentication ownership
description: Product boundary between the public Marketing site and the authenticated Pharmacy App.
---

Marketing should guide users into the Pharmacy App, while the Pharmacy App owns registration, login, session handling, and authenticated account flows.

**Why:** Splitting account creation across two frontends caused redirect, cookie, and unverified-account confusion; keeping the full entry flow in one app gives the session a single origin and clearer user experience.

**How to apply:** New public calls to action should open Pharmacy App registration, and future auth-related screens should be implemented in the Pharmacy App rather than duplicated in Marketing.