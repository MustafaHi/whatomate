# Backend: auth, permissions, handlers, data

All file paths relative to repo root. Read `.ai/architecture.md` first for stack/envelope/tenancy basics.

## Auth (three ways in)

1. **JWT cookies** (humans): HS256 via `golang-jwt/v5`. Login (`handlers/auth.go`) sets httpOnly cookies: `whm_access` (path `{base_path}/api`), `whm_refresh` (path `.../api/auth/refresh` only), `whm_csrf` (readable, for double-submit). **No tokens in JSON bodies.**
2. **Refresh rotation**: single-use JTIs stored in Redis (`refresh:<jti>`); refresh consumes the key (missing = revoked) and re-issues. Refresh carries forward a switched org. Logout deletes the key.
3. **API keys** (machines): `whm_<32 hex>` sent as `X-API-Key`, bcrypt-hashed with a 16-char `KeyPrefix` for lookup. **The key authenticates as its creator** — an admin's key carries admin power, including `is_super_admin`.
4. **WebSocket**: `/ws` upgrades unauthenticated; client must send `{"type":"auth","payload":{"token":"<jwt>"}}` within 5s. Short-lived (30s) tokens from `GET /api/auth/ws-token`.

Claims: `{user_id, organization_id, email, role_id, is_super_admin}` — stashed as fasthttp UserValues under string keys `"user_id"`, `"organization_id"`, etc. Handlers read via `a.getOrgID(r)` / `a.getOrgAndUserID(r)` (handlers/app.go). Org switching (`POST /api/auth/switch-org`) re-issues both tokens; non-super-admins take the target org's role from `user_organizations`.

CSRF (`middleware/csrf.go`): double-submit `whm_csrf` cookie vs `X-CSRF-Token` header on POST/PUT/DELETE/PATCH, **skipped when Authorization/X-API-Key is present** (machine clients exempt).

## Permissions

- Storage is **strings, not bitmask**: `Permission` rows are `(Resource, Action)` pairs; `CustomRole` (per-org, table `custom_roles`) joins via `RolePermission`. Keys format `"resource:action"`.
- Resources are dotted strings (`users`, `contacts`, `templates`, `settings.chatbot`, `chat.assign`, `analytics.agents`, ...) — see `internal/models/roles.go`. Actions: `read, write, delete, sync, execute, import, export, pickup, assign`.
- System roles seeded per org: `admin` (all), `manager` (broad minus users/roles/api-keys/audit), `agent` (read-mostly) — `SystemRolePermissions()` in models/roles.go.
- **Super admin is a boolean** (`users.is_super_admin`), not a role; `HasPermission` short-circuits true. Org admin is just a role holding all permissions.
- Resolution is **org-aware**: `getUserPermissionsCached(userID, orgID)` takes the role from the `user_organizations` row for the *target* org (falls back to `users.role_id` only for the home org) — deliberately prevents carrying home-org admin powers across tenants. Redis-cached 6h; invalidated via `InvalidateRolePermissionsCache` / `InvalidateOrgPermissionsCache` + WS `permissions_updated` push.
- In handlers: `a.requireAuth(r, models.ResourceX, models.ActionY)` (resolves org+user, writes 401/403 envelope, returns `errEnvelopeSent`), or `a.HasPermission(...)` inline for conditional logic.
- Some permission resources exist only as audit-log labels, never checked (`settings.chatbot.messages/agents/hours/sla/ai`).

## Handler pattern (copy this)

See `handlers/apikeys.go` for a clean example:

1. `orgID, userID, err := a.requireAuth(r, resource, action)` → on failure `return nil` (envelope already written)
2. `parsePathUUID(r, "id", "...")`, `parsePagination(r)` (default 50, max 100), `a.decodeRequest(r, &req)` for JSON bodies
3. Every query scoped: `a.DB.Where("id = ? AND organization_id = ?", id, orgID)` or `findByIDAndOrg[T]` / `a.ScopedQuery` / `a.ScopeToOrg` (handlers/cache.go)
4. Mutations call `a.logAudit(...)` (wraps `audit.LogAudit`)
5. `return r.SendEnvelope(payload)` / `r.SendErrorEnvelope(...)`

## Models & schema

- `internal/models/models.go` holds the core models (Organization, User, UserOrganization, Team/TeamMember, APIKey, SSOProvider, Webhook, CustomAction, WhatsAppAccount, Contact, Message, Template, WhatsAppFlow, Widget, AuditLog + JSONB custom types); `roles.go`, `bulk.go` (campaigns), `chatbot.go`, `call.go`, `catalog.go`, `tags.go`, `canned_responses.go`, `conversation_notes.go`, `constants.go` (all string enums).
- `BaseModel`: UUID PK (`gen_random_uuid()`), CreatedAt/UpdatedAt, **soft deletes via `gorm.DeletedAt`** (default). No BaseModel: AuditLog, UserAvailabilityLog, Tag, RolePermission.
- **Schema = GORM AutoMigrate; there are no SQL migration files.** The list is `GetMigrationModels()` in `internal/database/postgres.go` (38 models). Deliberately excluded: `ChatbotFlowStep` (legacy v1 chatbot, kept only for the `-migrate` backfill; table can be dropped later). `RolePermission` is created implicitly by the many2many tag.
- Indexes/DDL fixes are hand-written SQL in `getIndexes()` (same file): partial unique indexes `WHERE deleted_at IS NULL`, GIN index on `contacts.tags`, etc. Migration order: AutoMigrate → indexes → seed permissions/roles → seed system roles for orgs → migrate user-orgs → create default admin → seed widgets → backfill `last_inbound_at`. All idempotent.
- Column traps: `messages.whats_app_message_id` (DB) vs JSON `whatsapp_message_id` — note the underscore placement (`models.go`). Cross-references to WhatsApp accounts are by **name string**, not FK.
- Queries that must see soft-deleted rows use `Unscoped` deliberately (template sync restores soft-deleted rows; account embedded-signup reuses soft-deleted rows). Don't remove these.

## Queue & worker (campaigns)

- `internal/queue` is a **hand-rolled Redis Streams** queue (not asynq): stream `whatomate:campaigns`, consumer group `campaign-workers`, exactly one job type `recipient` (`RecipientJob{CampaignID, RecipientID, OrganizationID, PhoneNumber, TemplateParams, HeaderParams, ...}`). Producers: only `StartCampaign` and `RetryFailed` (handlers/campaigns.go).
- **No DLQ, no bounded retries.** Failed jobs are not ACKed; `claimPendingMessages` reclaims after 5min idle but runs **once at consumer startup** — a mid-run failure isn't redelivered until a worker restarts. Most per-recipient failures are terminal (status persisted as `failed`, error stored).
- `internal/worker/worker.go` `HandleRecipientJob`: reload campaign (skip silently if paused/cancelled) → decrypt account secrets → get-or-create contact → skip MARKETING for opted-out contacts → send template → persist Message with `Metadata["campaign_id"]` → update counters atomically → `checkCampaignCompletion` publishes stats to Redis pub/sub.
- Pacing = natural XReadGroup `Count: 1` per worker; **there is no send-rate throttle config** — don't invent one. Scale via more workers.
- Scheduled work (SLA escalation/auto-close of transfers) is the `SLAProcessor` ticker in the API server, not the worker.

## Cache (Redis)

`internal/handlers/cache.go` is the cache layer: chatbot settings, keyword rules, flows (per-org!), AI contexts, WhatsApp account by phoneID, user permissions, webhooks. TTLs ~6h; invalidation by SCAN-pattern delete (`InvalidateXxxCache`). Encrypted fields are cached **still-encrypted** via wrapper structs that bypass `json:"-"` (do not simplify by decrypting before caching). Team config cache lives in `internal/assignment`.

## Audit

Explicit calls in handlers (~54 sites), never middleware/hooks. `audit.LogAudit(...)` computes a JSON diff (with a skip-list for ids/timestamps/tokens) and inserts an `AuditLog` row fire-and-forget. Read side `handlers/audit_logs.go`, always org-scoped.

## Rate limiting

`middleware/ratelimit.go`: fixed-window per-IP or per-user (Redis INCR/EXPIRE), global API limit default 200 req/60s, **fails open when Redis is down**. Applied selectively to public auth endpoints via `withRateLimit` in main.go, not globally by default.

## Dead code / traps — do not imitate or "wire up"

- `middleware.CORS` — unused (CORS is `corsWrapper` in main.go).
- `middleware.OrganizationContext`, `RequirePermission`, `RequireAnyPermission` — never wired into routes.
- `middleware.RequestLogger` only records a start time; it logs nothing.
- `validate:"required..."` tags on request structs are dead — go-playground/validator is not a dependency and `decodeRequest` never validates. Validate manually.
- `handlers/stubs.go`: MarkMessageRead / GetMessageAnalytics / GetChatbotAnalytics are 501 placeholders.
- `pkg/whatsapp/webhook.go` helpers (`VerifyWebhook`, `ParseWebhook`, `ExtractMessages`) — legacy; the live webhook handler uses its own richer types in `handlers/webhook.go` / `chatbot_processor.go`. Edit those.
- A second `g.Before` "RBAC" closure in main.go is an intentional no-op.
