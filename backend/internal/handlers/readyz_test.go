package handlers

// Regression lock for AUD T-A6 (MEDIUM, truthful readiness) — /health is a
// static liveness signal, so the orchestrator (and any uptime monitor) saw
// "healthy" while the database was down; the outage was invisible until a
// user complained. /readyz is the load-bearing probe: it answers 200 only
// when the database dependency answers, and fails CLOSED (503) when the
// pool is missing or the ping does not return within its 2s budget.
//
// These probes run through the REAL SetupRoutes chain (the same
// registration production boots) with no database wired — the fail-closed
// branch. The DB-up branch needs a real PostgreSQL and is covered by the
// same handler against TEST_DATABASE_URL when provided.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pharmacy-os/backend/internal/config"
)

func readyzRouter(t *testing.T, withDB bool) *gin.Engine {
	t.Helper()
	cfg := &config.Config{
		TrustedProxies: "127.0.0.1/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16",
	}
	h := New(cfg)
	if withDB {
		dsn := os.Getenv("TEST_DATABASE_URL")
		if dsn == "" {
			t.Skip("TEST_DATABASE_URL not set — the ready path needs a real PostgreSQL")
		}
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(pool.Close)
		h.db = pool
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.SetupRoutes(r)
	return r
}

func TestReadyzFailsClosedWithoutDatabase(t *testing.T) {
	r := readyzRouter(t, false)

	for _, path := range []string{"/readyz", "/api/v1/readyz"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET %s without a database: got %d want 503 (readiness must fail closed, AUD T-A6)", path, w.Code)
		}
		if !strings.Contains(w.Body.String(), "unready") {
			t.Fatalf("GET %s body: %q want unready", path, w.Body.String())
		}
	}
}

func TestHealthStaysStaticLiveness(t *testing.T) {
	// The contract that justifies /readyz existing: /health answers 200
	// even with no database — it means "the process is up", nothing more.
	r := readyzRouter(t, false)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "healthy") {
		t.Fatalf("GET /health: got %d %q want 200 healthy (liveness contract broken)", w.Code, w.Body.String())
	}
}

func TestReadyzAnswersReadyWithLiveDatabase(t *testing.T) {
	r := readyzRouter(t, true)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ready") {
		t.Fatalf("GET /readyz with a live database: got %d %q want 200 ready", w.Code, w.Body.String())
	}
	// The ping budget is bounded: a hung database must not hang readiness.
	start := time.Now()
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("readyz took %v — the 2s ping budget is not enforced", elapsed)
	}
}
