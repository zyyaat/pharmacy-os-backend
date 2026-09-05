// Package main is the entry point for Pharmacy OS backend
package main

import (
	"log"

	"github.com/pharmacy-os/backend/internal/config"
	"github.com/pharmacy-os/backend/internal/handlers"
)

func main() {
	// Load configuration
	cfg := config.Load()

	// Initialize handlers
	h := handlers.New(cfg)

	// Start server
	log.Printf("Starting server on port %s", cfg.Port)
	if err := h.Run(":" + cfg.Port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
