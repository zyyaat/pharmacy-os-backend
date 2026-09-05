// Package handlers contains HTTP handlers for the API
package handlers

import (
        "github.com/gin-gonic/gin"
        "github.com/pharmacy-os/backend/internal/config"
)

// Handler holds all dependencies for HTTP handlers
type Handler struct {
        config *config.Config
        // TODO: Add service instances
}

// New creates a new Handler instance
func New(cfg *config.Config) *Handler {
        return &Handler{
                config: cfg,
        }
}

// SetupRoutes configures all API routes
func (h *Handler) SetupRoutes(r *gin.Engine) {
        // API v1 group
        v1 := r.Group("/api/v1")
        
        // Health check (both /health and /api/v1/health for compatibility)
        r.GET("/health", h.HealthCheck)
        v1.GET("/health", h.HealthCheck)
        
        // API routes will be added here
        // TODO: Add protected routes with auth middleware
}

// HealthCheck returns the health status of the API
func (h *Handler) HealthCheck(c *gin.Context) {
        c.JSON(200, gin.H{
                "status": "healthy",
                "service": "pharmacy-os-api",
        })
}

// Run starts the HTTP server
func (h *Handler) Run(addr string) error {
        r := gin.Default()
        h.SetupRoutes(r)
        return r.Run(addr)
}
