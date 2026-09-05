// Package handlers contains HTTP handlers for the API
package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pharmacy-os/backend/internal/auth"
	"github.com/pharmacy-os/backend/internal/config"
	appmiddleware "github.com/pharmacy-os/backend/internal/middleware"
)

// Handler holds all dependencies for HTTP handlers
type Handler struct {
	config *config.Config
	db     *pgxpool.Pool
	auth   *auth.Handler
}

// New creates a new Handler instance
func New(cfg *config.Config, db ...*pgxpool.Pool) *Handler {
	h := &Handler{config: cfg}
	if len(db) > 0 && db[0] != nil {
		h.db = db[0]
		h.auth = auth.NewHandler(db[0], auth.Config{
			AccessTTL:     cfg.AuthAccessTTL,
			RefreshTTL:    cfg.AuthRefreshTTL,
			CookieSecure:  cfg.CookieSecure,
			CookieDomain:  cfg.CookieDomain,
			BrevoAPIKey:   cfg.BrevoAPIKey,
			MailFromEmail: cfg.MailFromEmail,
			MailFromName:  cfg.MailFromName,
			PublicAppURL:  cfg.PublicAppURL,
		})
	}
	return h
}

// SetupRoutes configures all API routes
func (h *Handler) SetupRoutes(r *gin.Engine) {
	// Allow the configured frontend origins to call the API.
	r.Use(appmiddleware.CORS(h.config.GetCorsOrigins()...))

	// Root health endpoint for deployment startup probes.
	r.GET("/", h.HealthCheck)

	// API v1 group
	v1 := r.Group("/api/v1")

	// Health check (both /health and /api/v1/health for compatibility)
	r.GET("/health", h.HealthCheck)
	v1.GET("/health", h.HealthCheck)

	if h.auth != nil {
		h.auth.RegisterRoutes(v1)
	}
}

// HealthCheck returns the health status of the API
func (h *Handler) HealthCheck(c *gin.Context) {
	c.JSON(200, gin.H{
		"status":  "healthy",
		"service": "pharmacy-os-api",
	})
}

// Run starts the HTTP server
func (h *Handler) Run(addr string) error {
	r := gin.Default()
	h.SetupRoutes(r)
	return r.Run(addr)
}
