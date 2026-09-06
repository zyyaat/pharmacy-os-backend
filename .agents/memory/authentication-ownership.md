---
name: Authentication ownership
description: Product boundary between the public Marketing site and the authenticated Pharmacy App.
---

Marketing should guide users into the Pharmacy App, while the Pharmacy App owns registration, login, session handling, and authenticated account flows.

**Why:** Splitting account creation across two frontends caused redirect, cookie, and unverified-account confusion; keeping the full entry flow in one app gives the session a single origin and clearer user experience.

**How to apply:** New public calls to action should open Pharmacy App registration, and future auth-related screens should be implemented in the Pharmacy App rather than duplicated in Marketing.

The Pharmacy App accepts both `company_user` owners and `employee` identities for pharmacy-scoped reads. The pharmacy scope must come from the authenticated principal; employee-only mutations remain separately protected.

**Why:** Registration in the Pharmacy App creates a company owner account, so rejecting all company users at the pharmacy route boundary made the product reject the very account it had just created.

**How to apply:** Keep owner access tenant-scoped through the principal's assigned pharmacy, and do not broaden employee-only inventory or operational mutations without an explicit company permission model.