# Pharmacy OS Backend - Standalone Dockerfile
# Clean Go project - no monorepo overhead!

# Stage 1: Build (deps are vendored — the build never touches the network)
FROM golang:1.26-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /app
# Context-agnostic: Coolify/DockHosting pass the REPO ROOT as the build
# context when the dashboard points at backend/Dockerfile, while a manual
# `docker build backend/` hands backend/ itself. COPY . . is valid for both
# (the root .dockerignore shrinks the repo-root context to backend/), and
# the build step locates the module by probing for go.mod.
COPY . .
RUN if [ -f go.mod ]; then \
      CGO_ENABLED=0 GOOS=linux GOFLAGS=-mod=vendor go build -ldflags="-s -w" -o /app/server ./cmd/server ; \
    else \
      cd backend && CGO_ENABLED=0 GOOS=linux GOFLAGS=-mod=vendor go build -ldflags="-s -w" -o /app/server ./cmd/server ; \
    fi

# Stage 2: Runtime
FROM alpine:latest
# AUD-005: a pg_dump client must live inside the image so the
# destructive-migration guard (internal/database) can take a pre-migration
# snapshot. Newer pg_dump reads older servers, so try the newest client
# first and fall back across alpine's versioned package names.
RUN apk --no-cache add ca-certificates curl wget tzdata && \
    (apk --no-cache add postgresql18-client || \
     apk --no-cache add postgresql17-client || \
     apk --no-cache add postgresql16-client || \
     apk --no-cache add postgresql15-client || \
     echo "WARNING: no postgresql client package found — pre-migration snapshots disabled")
WORKDIR /app
COPY --from=builder /app/server .
RUN chmod +x ./server
# DockHosting/Coolify routes the public domain to the service port configured
# in the dashboard (8080 — restored there on 2026-10-30 after the 3000-mismatch
# outage) and the in-container healthcheck knocks on the same port — so the
# app itself must listen there. The server reads PORT with top priority
# (PORT > BACKEND_PORT > 8080); 8080 is also the code default, the
# docker-compose port and the dashboard field — one number everywhere.
# If you ever change the port field in the dashboard, change this line with it.
ENV PORT=8080
EXPOSE 8080
CMD ["./server"]
