# Messaging domain: WhatsApp Cloud API, webhooks, campaigns, realtime

## pkg/whatsapp — Cloud API client

- `Client` is stateless (`pkg/whatsapp/client.go`); per-call credentials come from a `whatsapp.Account` value built from the DB model via `(*models.WhatsAppAccount).ToWAAccount()` **after** `DecryptSecrets()`. Tokens live in the DB, never in the client.
- **Graph API version is per-account** (`account.APIVersion`, default `v21.0` in the DB column); every URL embeds it. (Config fallback says `v18.0` — another default exists in handlers; config wins when set.)
- Errors: non-200 → `ParseMetaAPIError` (surfaces Meta `code`, `message`, `error_data.details`). Message methods in `pkg/whatsapp/message.go`: text (variadic reply-to → `context.message_id`), interactive buttons (**1–3 buttons = `button` format, 4–10 = `list` format, >10 rejected**; titles truncated to 20 chars), CTA URL button, voice-call button, flow message, template.
- Template component building (`BuildTemplateComponents`, `AutoButtonComponents`, `BodyParamsToComponents`): positional keys must sort **numerically** ("1","2","10" — fixed issue #354); TEXT headers allow ≤1 variable; DOCUMENT headers require `filename` (issue #351, Meta error 132012 otherwise).
- Media: `UploadMedia` (multipart), `ResumableUpload` (AppID-based 2-step, used only for template header samples), `DownloadMedia` (Bearer needed for CDN too).
- Account mgmt: `ValidateCredentials` (3-step), `RegisterPhoneNumber` (2FA PIN), `SubscribeApp` (required after registration to receive webhooks), `ExchangeCodeForToken`, `GetSharedWABA`.

## Accounts (connecting a number)

Model `models.WhatsAppAccount` — `Name` unique per org; secrets (`AccessToken`, `AppSecret`, `Pin`) encrypted (`enc:`); `WebhookVerifyToken` plaintext and exposed in API responses (intentional). Two connection paths (`handlers/accounts.go`):

1. **Manual token** — `POST /api/accounts`.
2. **Meta Embedded Signup** — `POST /api/accounts/exchange-token` → code exchange → WABA/phone discovery via `/debug_token` scopes → reuse soft-deleted row (`pending_registration`) → auto-register with generated 6-digit PIN (returned **once** in plaintext) → `SubscribeApp` → encrypt → save + audit. Org-level Meta app creds live in `organizations.Settings` (meta_app_id/meta_config_id/meta_app_secret_encrypted), falling back to global config.

## Sending a message

Unified sender `SendOutgoingMessage` (`handlers/messages.go`) used by agent chat, REST API, chatbot, SLA notifier:

1. Message row persisted immediately with `status: pending`.
2. `sendFn` dispatches on type (text/media/interactive/template/flow).
3. Async by default: goroutine + 30s timeout → `finalizeMessageSend`: success sets `status='sent'` + `whats_app_message_id`; failure sets `failed` + error and broadcasts WS `status_update`.
4. WS `new_message` may arrive **before** the send resolves.

Option presets define per-caller behavior (`DefaultSendOptions` agent UI: async+WS+webhook; `ChatbotSendOptions`: sync, no webhook, SLA tracking; `APISendOptions`; `SLASendOptions`).

**Statuses are monotonic** (`statusPriority` in handlers/webhook.go): pending 0 → sent 1 → delivered 2 → read 3, but **failed = 4 overrides anything**, including read. Regressions ignored. Campaign linkage: if `Metadata["campaign_id"]` is set, status webhooks atomically increment campaign stats and recipient timestamps.

Account resolution order (agent send): explicit request → `contact.WhatsAppAccount` → org `is_default_outgoing` → first account. Agent visibility on contacts is restricted by `a.scopeAssignedContact` (agents without contact-read permission only see assigned contacts).

Phone numbers are stored **without the leading `+`** (`contactutil.GetOrCreateContact` strips it; lookup checks both forms). Trap: `SendTemplateMessage` matches `phone_number = ?` on raw input — a `+`-prefixed API call can create a duplicate contact.

## Webhooks — inbound (Meta → Whatomate)

`GET /api/webhook` verify: accepts the global `Config.WhatsApp.WebhookVerifyToken` **or any account's** `webhook_verify_token`; echoes `hub.challenge`.
`POST /api/webhook` (`handlers/webhook.go`):

- Signature: HMAC-SHA256 of raw body, `X-Hub-Signature-256: sha256=<hex>`, constant-time compare, secret = account `AppSecret` looked up by first `phone_number_id` in the payload. **If the header is absent or the account has no AppSecret, verification is silently skipped.**
- Field routing: `messages` (+ `statuses`), `message_template_status_update` (template approval sync), `user_preferences` (marketing opt-out → `contacts.marketing_opt_out`), `calls`, `smb_message_echoes` (mobile-app sends stored as outgoing), `smb_app_state_sync`.
- Each message/status processed in a **fresh goroutine** with WAMID dedup — ordering is not guaranteed. BSUID-only users (no phone) skipped with a warning.

## Webhooks — outbound (Whatomate → customer URLs)

Model `models.Webhook` (URL, Events, Headers, auto-generated HMAC Secret, IsActive); CRUD `/api/webhooks` (`handlers/webhook_dispatch.go`). Dispatch: async, configs Redis-cached, semaphore 10 concurrent, **3 retries 1s/2s/4s**, 2-min context. Envelope `{event, timestamp, data}`. Signature header is **`X-Webhook-Signature: sha256=<hmac>`** — NOT Meta-style `X-Hub-Signature-256`. SSRF-protected (blocks private IPs post-DNS). Events: `message.incoming`, `message.sent`, `message.outgoing` (incl. mobile echoes), `contact.created`, `transfer.created/assigned/resumed`.

## Templates

Lifecycle: local `DRAFT` row → `SubmitTemplate` posts to Meta → `PENDING` → approval arrives via `message_template_status_update` webhook, matched by `business_id` + `name` + `language` (natural key alongside org+account). Editing an APPROVED/REJECTED template locally resets to DRAFT; name/language/category are immutable at Meta (updates resend components only). `SyncTemplates` upserts `Unscoped` (restores soft-deleted) — **fetches limit=100 with no pagination**, large WABAs silently miss templates. Template analytics is **Meta-side** (`handlers/meta_analytics.go` proxies `/{waba}/template_analytics`, Redis-cached, 90-day cap).

## Campaigns

`StartCampaign` (handlers/campaigns.go) sets `processing`, converts pending `BulkMessageRecipient` rows to `RecipientJob`s, enqueues to Redis Streams; worker pipeline in `.ai/backend.md`. Workers **bypass the unified sender** — campaign messages get no `new_message` WS broadcast and no `message.sent` webhook; only later Meta status webhooks touch them. `RetryFailed` resets failed recipients+messages to `pending` and re-enqueues only failures. Recipients carry separate `TemplateParams`/`HeaderParams` so header `{{1}}` doesn't collide with body `{{1}}`.

## Realtime (WebSocket)

`internal/websocket` hub: `map[orgID]map[userID]map[*Client]` (multi-tab). `BroadcastToOrg` / `BroadcastToContact` (only clients that sent `set_contact` for that chat) / `BroadcastToUser(s)`. Full buffers drop messages (no backpressure). Event types (`internal/websocket/messages.go`): `new_message`, `status_update`, `contact_update`, `agent_transfer`/`_resume`/`_assign`, `transfer_escalation`/`_expired`/`_escalated`, `campaign_stats_update`, `permissions_updated`, `conversation_note_*`, `call_*`, `outgoing_call_*`. Payload detail: `status_update` uses the DB UUID as `message_id`; `new_message` uses `id`. `reaction_update` is emitted as an ad-hoc literal in handlers/messages.go, not a constant.

## Quick trap list

- Two webhook payload type families exist; the live one is in `handlers/` (richer), `pkg/whatsapp/webhook.go` is legacy.
- `flows.go` / `models.WhatsAppFlow` = Meta **WhatsApp Flows** (forms). Not chatbot flows (`models.ChatbotFlow`).
- Campaign sends have no throttle knob; worker pacing only.
- WS hub is per-process — campaign stats are the only Redis-bridged event.
