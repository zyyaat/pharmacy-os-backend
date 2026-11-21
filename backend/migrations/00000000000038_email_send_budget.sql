-- Migration 38 — fair-use budget for outbound transactional email (Task 148).
-- The 60s resend cooldown (Task 131) only spaces consecutive sends; it never
-- bounds VOLUME: a determined client could still mint one email per minute
-- (60/hour — exactly the free provider tier) for any address, and the
-- forgot-password path had NO send-side bound at all. Global platforms bound
-- outbound email the same way they bound traffic: a per-RECIPIENT budget
-- (the inbox that feels the spam is the unit of fairness) plus a
-- platform-wide daily ceiling that protects the provider quota shared by
-- every feature (verification, reset, support).
--
-- auth_email_send_budget — per-recipient counters with two fixed windows
-- (rolling hour + calendar day). Keyed by lowercased EMAIL, not principal
-- id: the same address reached through several principals (company user +
-- employee) shares one budget, because the recipient's inbox is what feels
-- the spam. Counters count ATTEMPTS consumed at dispatch time — a send that
-- later fails upstream still counted, so retry storms against a broken
-- provider cannot amplify; denied requests also tick (an abuser hammering a
-- denied wall must not bank a clean slate for the next window; the day
-- window eventually heals everything).
--
-- platform_email_sends — one row per day, the platform-wide send ceiling
-- (default 280, under the 300/day free provider tier so chat/support mail
-- keeps headroom). Superseded days simply accumulate (~365 tiny rows/year).
CREATE TABLE IF NOT EXISTS auth_email_send_budget (
    email                  TEXT PRIMARY KEY,
    hour_window_started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    hour_count             INT NOT NULL DEFAULT 0,
    day_window_started_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    day_count              INT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS platform_email_sends (
    day        DATE PRIMARY KEY,
    send_count INT NOT NULL DEFAULT 0
);
