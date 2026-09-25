# AGENTS.md

Whatomate: open-source WhatsApp Business platform. Go backend + Vue frontend, shipped as a single binary with the frontend embedded.

## Commands

All via Makefile:

- `make build` — build backend only
- `make run` — run backend (needs `config.toml`; copy from `config.example.toml`)
- `make run-migrate` — run backend and apply schema migrations
- `make dev` — backend + frontend dev servers together
- `make test` — all Go tests (`go test ./...`, uses gotestsum if installed)
- `make test-coverage` — tests + coverage.html
- `make lint` — golangci-lint
- `make fmt` — go fmt
- `make frontend-install` / `frontend-dev` / `frontend-build`
- `make build-prod` — production binary with embedded frontend

Frontend (run inside `frontend/`, or use the make targets):

- `npm run dev` — Vite dev server
- `npm run typecheck` — vue-tsc
- `npm run test:unit` — vitest
- `npm run lint` — eslint
- `npm run test:e2e` — Playwright (expects backend on localhost:8080)

## Layout

- `cmd/whatomate/` — entrypoint (CLI: `server`, etc.)
- `internal/handlers/` — HTTP API handlers; tests co-located as `*_test.go` next to the handler file
- `internal/models/` — GORM models. **Schema = GORM AutoMigrate; there are no SQL migration files.** Changing a model changes the schema (applied via `-migrate` flag).
- `internal/database/` — Postgres + Redis setup
- `internal/worker/`, `internal/queue/` — background jobs
- `internal/websocket/` — realtime chat
- `internal/flowgraph/` — chatbot flow engine
- `internal/calling/`, `internal/tts/`, `internal/storage/`, `internal/crypto/`, `internal/audit/`, `internal/middleware/`, `internal/config/`
- `internal/frontend/` — embed target for `frontend/dist`
- `pkg/whatsapp/` — WhatsApp Cloud API client
- `frontend/` — Vue 3 + Vite + Tailwind + shadcn-vue (reka-ui), Pinia, TanStack vue-query
- `docker/` — docker compose stack

## Deep docs (read the relevant one before touching that area)

All verified against the code; where code comments contradict them, trust these:

- `.ai/architecture.md` — stack (fastglue/fasthttp, NOT Gin), boot sequence, routing, config, tenancy, cross-process limits, storage/crypto
- `.ai/backend.md` — auth (cookies/API keys/WS), permissions, handler pattern, models/migrations, Redis Streams queue, worker, cache, audit, dead-code traps
- `.ai/messaging.md` — WhatsApp client, account connection, send flow, Meta webhooks, outbound webhooks, templates, campaigns, realtime events
- `.ai/chatbot-calling.md` — flow graph engine, chatbot pipeline + AI providers, WhatsApp Calling/WebRTC, IVR/TTS, assignment/SLA, analytics
- `.ai/frontend.md` — Vue app structure, cookie auth + CSRF, stores, dark-first UI conventions, WebSocket service, Playwright/Vitest, flow editors

## Gotchas

- Backend requires Postgres + Redis; local config from `config.example.toml`.
- Go 1.26. Module: `github.com/shridarpatil/whatomate`.
- New Go tests belong next to the code they test (existing convention).
- PRs: keep small and single-concern (see CONTRIBUTING.md).
- Several code comments and helpers are stale or dead (unused middleware, no-op RBAC, legacy webhook helpers in `pkg/whatsapp`) — the per-topic docs in `.ai/` list them so you don't imitate them.
