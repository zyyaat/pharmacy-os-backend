// Background jobs wiring (api_level 118) — the process owner side of
// internal/jobs. The River daemon runs INSIDE the API server process (one
// binary, one pool): HTTP keeps answering while workers pull the imports
// and mail queues. Everything here is nil-safe: a bare test handler (no
// pool), the JOBS_ENABLED=0 kill switch, or a construction failure all
// leave jobsClient nil and the enqueue paths answer 503 — the API never
// dies because the queue engine could not start.
package handlers

import (
        "context"
        "log"

        "github.com/jackc/pgx/v5/pgxpool"

        "github.com/pharmacy-os/backend/internal/jobs"
        "github.com/pharmacy-os/backend/internal/mailx"
        "github.com/pharmacy-os/backend/internal/models"
        "github.com/pharmacy-os/backend/internal/services"
)

// AttachBackgroundJobs builds and starts the River client. Call once from
// the server entry point with a long-lived context; the caller owns the
// lifecycle (jobs.Stop after the HTTP drain).
func (h *Handler) AttachBackgroundJobs(ctx context.Context, pool *pgxpool.Pool) error {
        if !jobs.Enabled() || pool == nil {
                return nil
        }
        deps := jobs.Deps{
                Pool: pool,
                Plan: h.planForCompany,
                Mail: h.mailDispatcher(),
        }
        client, err := jobs.Start(ctx, deps)
        if err != nil {
                return err
        }
        h.jobsClient = client
        h.jobsDeps = deps
        log.Printf("[JOBS] River queue engine started (imports: 1 worker, mail: 4 workers)")
        return nil
}

// JobsClient exposes the handle for the graceful-stop call in main.
func (h *Handler) JobsClient() *jobs.Client { return h.jobsClient }

// planForCompany re-reads the tenant's product plan at WORK time — the same
// semantics as resolveProductPlanState minus the gin context: a gate error
// degrades to unlimited exactly like the synchronous path's contract
// (ExecutePharmacyImport re-validates the hard work either way).
func (h *Handler) planForCompany(ctx context.Context, companyID string) services.PlanState {
        if h.subs == nil || companyID == "" {
                return services.PlanState{Used: 0, Limit: -1}
        }
        used, limit, _, err := h.subs.CheckLimit(ctx, companyID, models.LimitKeyProducts)
        if err != nil {
                return services.PlanState{Used: 0, Limit: -1}
        }
        return services.PlanState{Used: used, Limit: limit}
}

// mailDispatcher resolves the operator's transport per send. Readiness is
// checked here (silently skipping when no mail is configured) so a tenant
// without Brevo/SMTP never produces worker error noise — the run row and
// the status endpoint carry the truth regardless.
func (h *Handler) mailDispatcher() jobs.MailDispatch {
        return func(ctx context.Context, to, subject, html, text string) error {
                if !h.mailTransportReady() {
                        return nil
                }
                cfg := h.config
                return mailx.Dispatch(ctx, mailx.DispatchConfig{
                        Transport:    cfg.ResolvedMailTransport(),
                        BrevoAPIKey:  cfg.BrevoAPIKey,
                        FromEmail:    cfg.MailFromEmail,
                        FromName:     cfg.MailFromName,
                        SMTPHost:     cfg.MailSMTPHost,
                        SMTPPort:     cfg.MailSMTPPort,
                        SMTPUsername: cfg.MailSMTPUsername,
                        SMTPPassword: cfg.MailSMTPPassword,
                }, to, subject, html, text)
        }
}
