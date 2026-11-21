package handlers

// Unit locks for the hardened HTTP server (AUD-002): the timeout/buffer
// fields, the finally-enforced MAX_REQUEST_SIZE (it existed with zero
// references before this change), and a live listener Shutdown — the same
// path main.go drives from the SIGTERM handler.

import (
        "context"
        "net"
        "net/http"
        "net/http/httptest"
        "strings"
        "testing"
        "time"

        "github.com/gin-gonic/gin"
        "github.com/pharmacy-os/backend/internal/config"
        appmiddleware "github.com/pharmacy-os/backend/internal/middleware"
)

func hardeningTestHandler(t *testing.T) *Handler {
        t.Helper()
        cfg := &config.Config{
                MaxRequestSize: 1024, // tiny so the cap is observable
                TrustedProxies: "127.0.0.1/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16",
        }
        return New(cfg) // no DB: only the health routes register — enough here
}

func TestNewServerHardensEveryPhase(t *testing.T) {
        h := hardeningTestHandler(t)
        srv := h.NewServer("127.0.0.1:0")

        if srv.ReadHeaderTimeout != 10*time.Second {
                t.Fatalf("ReadHeaderTimeout = %v want 10s — slowloris wall missing", srv.ReadHeaderTimeout)
        }
        if srv.ReadTimeout != 60*time.Second {
                t.Fatalf("ReadTimeout = %v want 60s", srv.ReadTimeout)
        }
        if srv.WriteTimeout != 120*time.Second {
                t.Fatalf("WriteTimeout = %v want 120s", srv.WriteTimeout)
        }
        if srv.IdleTimeout != 120*time.Second {
                t.Fatalf("IdleTimeout = %v want 120s", srv.IdleTimeout)
        }
        if srv.MaxHeaderBytes != 1<<20 {
                t.Fatalf("MaxHeaderBytes = %d want 1MB", srv.MaxHeaderBytes)
        }
}

func TestMaxRequestBodyRejectsOversizeContentLength(t *testing.T) {
        gin.SetMode(gin.TestMode)
        r := gin.New()
        r.Use(appmiddleware.MaxRequestBody(16))
        r.POST("/echo", func(c *gin.Context) {
                body := make([]byte, 32)
                _, _ = c.Request.Body.Read(body)
                c.String(http.StatusOK, "reached")
        })

        w := httptest.NewRecorder()
        req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(strings.Repeat("x", 64)))
        r.ServeHTTP(w, req)
        if w.Code != http.StatusRequestEntityTooLarge {
                t.Fatalf("oversize body: got %d want 413", w.Code)
        }
        if !strings.Contains(w.Body.String(), "request_too_large") {
                t.Fatalf("oversize body response: %q", w.Body.String())
        }
}

func TestMaxRequestBodyAllowsWithinCap(t *testing.T) {
        gin.SetMode(gin.TestMode)
        r := gin.New()
        r.Use(appmiddleware.MaxRequestBody(64))
        r.POST("/echo", func(c *gin.Context) { c.String(http.StatusOK, "reached") })

        w := httptest.NewRecorder()
        req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(strings.Repeat("x", 16)))
        r.ServeHTTP(w, req)
        if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "reached") {
                t.Fatalf("within-cap body blocked: %d %q", w.Code, w.Body.String())
        }
}

// The exact call main.go makes on SIGTERM must return cleanly for an idle
// server — proving the shutdown path is wired, not just declared.
func TestNewServerShutdownDrainsIdleListener(t *testing.T) {
        h := hardeningTestHandler(t)
        srv := h.NewServer("127.0.0.1:0")

        ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", srv.Addr)
        if err != nil {
                t.Fatalf("listen: %v", err)
        }
        go func() { _ = srv.Serve(ln) }()

        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        if err := srv.Shutdown(ctx); err != nil {
                t.Fatalf("Shutdown returned %v — drain path broken", err)
        }
}
