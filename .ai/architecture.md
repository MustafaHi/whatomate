# Architecture

Whatomate: open-source WhatsApp Business platform. Single Go binary with an embedded Vue SPA, Postgres, Redis.

Verified against the code — trust this over comments in the code where they conflict (several comments are stale).

## Stack (the part agents guess wrong)

- **HTTP framework is `zerodha/fastglue` on `valyala/fasthttp`** — NOT Gin/Echo/net-http. Handlers are methods `func (a *handlers.App) X(r *fastglue.Request) error`; middleware is `func(r *fastglue.Request) *fastglue.Request`, and returning `nil` from middleware stops the chain.
- Logging: `zerodha/logf`. ORM: GORM + Postgres. Cache/queue/pubsub: `redis/go-redis/v9`.
- Frontend: Vue 3 + Vite, embedded into the binary via `internal/frontend` (see `.ai/frontend.md`).
- CLI is hand-rolled stdlib `flag` in `cmd/whatomate/main.go` (no cobra). Subcommands: `server` (HTTP + optional embedded workers via `-workers N`), `worker` (queue consumers only, no HTTP), `version`, `help`.

## Boot sequence (`runServer` in cmd/whatomate/main.go)

1. logger → `config.Load(path)`
2. production guards (JWT secret ≥32 chars required, `server.allowed_origins` required; debug mode only warns)
3. `database.NewPostgres` → if `-migrate`: `database.RunMigrationWithProgress` + `handlers.BackfillChatbotFlowGraph` (legacy v1 chatbot flows → v2 graphs)
4. `database.NewRedis` → `queue.NewRedisQueue`
5. `fastglue.NewGlue()`, `whatsapp.NewWithBaseURL`, `websocket.NewHub` + `go hub.Run()`
6. shared outbound `http.Client` with `handlers.SSRFSafeDialer()` (SSRF protection for chatbot api_call/webhook nodes and outbound webhooks)
7. `handlers.App{...}` + `assignment.Assigner` + `calling.Manager` (only when org calling enabled) + `storage.S3Client` (only when call recordings enabled) + `tts.PiperTTS` (only when piper configured)
8. `app.StartCampaignStatsSubscriber()` (Redis pub/sub → WebSocket bridge from workers)
9. global middleware `g.Before`: SecurityHeaders → RequestLogger → Recovery → CSRFProtection
10. `setupRoutes(...)` (same file — ALL route registration lives here)
11. fasthttp server: `corsWrapper` around the glue handler, `MaxRequestBodySize` 15MB, then SLA processor goroutine (1-min ticker), embedded workers, graceful shutdown on SIGINT/SIGTERM

## Routing

- Everything is flat under `/api` — **there is no `/api/v1` and never invent one**. Fastglue path params use `{id}`, read via `r.RequestCtx.UserValue("id")` (helper `parsePathUUID`).
- Auth is a `g.Before` closure in `setupRoutes` with a **hardcoded skip-list of public paths** (`/health`, `/ready`, `/api/auth/login|register|refresh|logout`, `/api/webhook`, `/ws`, `/api/auth/sso/*`, `/api/custom-actions/redirect`). A new public route must be added there or it 401s.
- There is **no route-level RBAC** — a `g.Before` "role-based access control" closure is a deliberate no-op. Permission checks happen inside handlers via `a.requireAuth(r, resource, action)`. Forgetting it silently exposes an endpoint to any authenticated user.
- CORS is implemented in `corsWrapper()` in main.go at the raw fasthttp level; `middleware.CORS` exists but is unused. Empty allowed_origins = reflect any origin (dev).
- SPA catch-all served by `internal/frontend.Handler(basePath)` when embedded.

## Response envelope (exact shapes)

- Success: `{"status":"success","data":{...}}` — always HTTP 200 from `r.SendEnvelope`.
- Error: `{"status":"error","message":"...","data":null}` via `r.SendErrorEnvelope(status, msg, nil, "")`.
- Lists: `{"status":"success","data":{"<resource>":[...],"total":N,"page":1,"limit":50}}` (`listEnvelope` in handlers/helpers.go).
- Handler pattern: on auth/validation failure the helper writes the envelope and returns sentinel `errEnvelopeSent`; the handler then does `return nil`.

## Configuration

- TOML via koanf + env overlay: prefix `WHATOMATE_`, **double underscore as nesting separator** (`WHATOMATE_DATABASE__HOST`, `WHATOMATE_WHATSAPP__APP_ID`) because key names themselves contain single underscores.
- Sections (`internal/config/config.go`): `App` (incl. `EncryptionKey` for at-rest crypto), `Server`, `Database`, `Redis`, `JWT`, `WhatsApp`, `AI`, `Storage`, `DefaultAdmin`, `RateLimit`, `Cookie`, `Calling`, `TTS`.
- `setDefaults` fills sane defaults (port 8080, JWT 15min/1day, storage local `./uploads`, default admin `admin@admin.com`/`admin` — seeded by migrations). `Cookie.Secure` forced on in production.

## Tenancy model

Two scoping axes; both matter:

1. `organization_id` on nearly every table, enforced **in handlers, not middleware** (`Where("organization_id = ?", orgID)`, generics helper `findByIDAndOrg[T]`).
   - Exceptions: `Permission`/`RolePermission` are global; `Tag` uses composite PK `(organization_id, name)`; `BulkMessageRecipient`, `ChatbotSessionMessage`, `ChatbotFlowStep` scope transitively via parent.
2. WhatsApp **account by name** (string column `whats_app_account`), not FK, on ChatbotSettings, KeywordRule, ChatbotFlow, AIContext, ChatbotSession, CallLog, IVRFlow, Contact, Message, Template, campaigns. `''` = org-level default. Account-specific rows shadow global rows (precedence merge in handlers/cache.go).
   - Quirk: chatbot flow *trigger matching* is org-wide (flows cached per org, no account filter), but `goto_flow` refuses cross-account jumps — the two layers intentionally differ; don't "unify" them.

Identity comes from JWT claims / API keys; handlers resolve org via `a.getOrgID(r)`, which honors an `X-Organization-ID` header override (super admin → any org; others → only with `user_organizations` membership).

## Cross-process reality (matters for any realtime feature)

- The WebSocket hub (`internal/websocket`) is **in-process**. Org broadcasts reach only clients connected to the same instance. The sole cross-process bridge is campaign stats over Redis pub/sub (`whatomate:campaign_stats`). Multi-replica API deployments need the pub/sub pattern extended; today it's single-instance software.
- Workers are separate processes (`whatomate worker` or `-workers N`); they never run migrations.

## Storage & crypto

- **S3 is used for call recordings only** (`storage.Upload` / `GetPresignedURL`; key on `call_logs.recording_s3_key`). Everything else (message media, template header media, IVR/TTS audio) is **local disk** under `cfg.Storage.LocalPath` (default `./uploads`). `StorageConfig.Type` (local/s3) is decorative — there is no backend abstraction.
- Secrets at rest: AES-256-GCM, base64, `enc:` prefix, key = `App.EncryptionKey`. `crypto.Encrypt/Decrypt` are **no-ops with an empty key** (dev), and `Decrypt` passes through legacy unencrypted values. `deriveKey` pads/truncates the raw string to 32 bytes — not a KDF.
- Encrypted: `WhatsAppAccount.AccessToken/AppSecret/Pin` (model method `DecryptSecrets`), `SSOProvider.ClientSecret`, org Meta app secret. **API keys are not encrypted** — bcrypt-hashed with a 16-char prefix for lookup.
- `ChatbotSettings.AIConfig.APIKey` has `json:"-"` but is **never actually encrypted** (stored raw, chatbot.go:~435); the Redis cache works around `json:"-"` via side-fields.

## Where to go deeper

- `.ai/backend.md` — auth, permissions, handler patterns, models/migrations, queue/worker, audit, cache, dead code traps
- `.ai/messaging.md` — WhatsApp client, accounts, send flow, webhooks, templates, campaigns, realtime
- `.ai/chatbot-calling.md` — flow graph engine, chatbot AI, WhatsApp Calling/WebRTC, IVR/TTS, assignment, analytics
- `.ai/frontend.md` — Vue app, API client, state, UI conventions, e2e testing, flow editor
