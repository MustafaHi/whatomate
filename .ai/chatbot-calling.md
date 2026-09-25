# Chatbot, flow engine, calling, assignment, analytics

## The flow graph engine (`internal/flowgraph`)

Deliberately tiny and generic (~80 lines): `Graph[T ~string]{Version, Nodes, Edges, EntryNode}`; `Node{ID, Type T, Label, Position, Config map[string]any}` — **node behavior is config-driven maps, not typed structs**. `Edge{From, To, Condition}`; `ResolveEdge(from, outcome)` matches exact condition, falls back to `"default"`, empty = terminal. Each domain owns its node vocabulary:

- Chat: 14 types in `handlers/chatbot_graph_types.go` — `start, message, buttons, prompt, api_call, condition, timing, set_variable, ai_response, transfer, webhook, goto_flow, whatsapp_flow, end`. (No "question" node — it's `prompt`.) Edge conditions: `default`, `button:<id>`, `http:2xx/non2xx`, `validation_failed`, `max_retries`, `in_hours/out_of_hours`.
- IVR: 8 types in `internal/calling/session.go` — `greeting, menu, gather, http_callback, transfer, goto_flow, timing, hangup`. Conditions: `digit:N`, `timeout`.

Storage: chatbot → `chatbot_flows.graph` JSONB (`version` must be 2, resolvable `entry_node` required — `parseChatGraph` rejects otherwise). IVR → `ivr_flows.menu` JSONB, same shape (legacy key name `menu`; the frontend still saves under it).

## Inbound chatbot pipeline (`handlers/chatbot_processor.go`)

Meta webhook → `go a.processIncomingMessage` (panic-recovered, WAMID-deduped) → `processIncomingMessageFull`. Dispatch order:

1. reactions (special-cased) → 2. contact get/create, save message → 3. active `AgentTransfer` silences the bot → 4. `ChatbotSettings.IsEnabled` false → transfer to agent queue → 5. business-hours gate → 6. resume in-progress flow if session has `CurrentFlowID` → 7. **flow trigger keywords** — case-insensitive substring match over cached flows (org-wide, not account-scoped) → 8. keyword rules (exact/contains/starts_with/regex) → 9. AI fallback → 10. fallback message.

Trap: the trigger keyword is **not** fed to the entry node — the runner starts fresh, so a flow that begins with a `prompt` re-asks.

## Graph runner semantics (`handlers/chatbot_graph_runner.go`)

- Node executors return `{outcome, yield}`; `yield=true` = blocking (buttons sent / prompt awaiting reply) → persist session and wait; `yield=false` → advance via `ResolveEdge`. Non-blocking chains capped at 100 iterations (runaway guard).
- Session state = `models.ChatbotSession`: `CurrentFlowID`, `CurrentStep` (node ID), `StepRetries`, `SessionData` JSONB variable store, 30-min default timeout. Built-ins: `phone_number`, `contact_name`; audit trail in `SessionData["__path__"]`. Templates render `{{var}}` from SessionData.
- Conditions (`condition` nodes + per-node `skip_condition`) evaluate via **`expr-lang/expr`** with undefined variables allowed.
- `goto_flow` swaps `session.CurrentFlowID`; the runner reloads and continues — no return stack (target flow end = session end). Cross-account jumps are refused.
- **There is no live legacy executor.** A flow with nil Graph just ends the session. `handlers/chatbot_flow_migration.go` (`BackfillChatbotFlowGraph`, run under `-migrate`) converts v1 `chatbot_flow_steps` rows to v2 graphs idempotently; the legacy table is quarantined, not dropped. The stale comment in `parseChatGraph` about "use legacy Steps" is wrong.

## AI (LLM) integration

Raw HTTP, no SDK — all in `chatbot_processor.go`: OpenAI (`/v1/chat/completions`), Anthropic (`/v1/messages`), Google (`generativelanguage...` — API key in query string). Config = `models.AIConfig` embedded in `ChatbotSettings` — **one settings row per (org, WhatsApp account)**; provider/key/model/temperature/system-prompt are account-level, not per node. `ai_api_key` is stored raw despite the `// encrypted` comment. History = last N `chatbot_session_messages` (optional). RAG-ish grounding via `AIContext` rows (`static` text or `api` fetched per message with `{{phone_number}}`/`{{user_message}}` templating).

Naming traps: `handlers/flows.go` = Meta WhatsApp Flows, not chatbot flows. `prompt` and `webhook` are internal-only node types (a Text node with expected response becomes `prompt`).

## Calling (WhatsApp Business Calling + WebRTC)

**The Go server is the media plane** — `pion/webrtc/v4` bridges the WhatsApp caller and the agent's browser. No LiveKit/SFU. Sessions in `calling.Manager.sessions` keyed by WhatsApp `call_id`; `CallSession` holds both PeerConnections (server PC + agent PC), IVR/transfer state, recorders.

Two signaling planes:

- WhatsApp side: HTTP webhooks (`handlers/call_webhook.go`): `ringing | connect | in_call | ended | terminate | missed | unanswered`. SDP offer arrives on `connect`; server answers via `PreAcceptCall` → `AcceptCall` (sequence documented in `calling/webrtc.go`). Hangup via `TerminateCall`.
- Agent side: REST, not WS-SDP — browser gets ICE servers from `GET /api/calls/ice-servers`, POSTs its offer to `ConnectCallTransfer`, gets `{sdp_answer}` back. Atomic `UPDATE ... WHERE status='waiting'` guards two agents claiming one call.
- WebSockets carry UI notifications only (`call_incoming`, `call_transfer_*`, `outgoing_call_*`, `call_hold`, ...).

Behaviors to know: **sticky calls** — outbound `voice_call` buttons carry `agent:<uuid>`, echoed back by Meta as `biz_opaque_callback_data`, plus Redis `vc_sticky:<orgID>:<phone>` (15-min TTL); sticky calls bypass the IVR. Transfer rotation: sticky agent → contact's `assigned_user_id` → team rotation with per-agent 15s timeout (`TriedAgentIDs`). Hold/resume seeds RTP sequence numbers so packets aren't dropped as stale. Recordings: ffmpeg loudnorm+mix → S3 `recordings/<orgID>/<callLogID>.ogg`, presigned download. Recent fixes (Sep 2026): browser DTLS close surfaces as clean hangup (`peerGone`); termination uses the **decrypted** account snapshot taken at session creation (reloading from DB would hand Meta the encrypted token).

Outgoing calls: server creates the WhatsApp-side PC; the SDP answer arrives asynchronously via webhook (`SDPAnswerReady` channel). Edge cases: WhatsApp may skip `ringing`; orphaned `terminate` after cleanup handled; calls with no agent marked `missed` even after IVR answered.

## TTS

Piper via CLI only (`internal/tts/piper.go`): piper → WAV → opusenc → OGG/Opus, content-addressed cache (`tts_<sha256[:16]>.ogg`). Wired only when binary+model configured. **Used only by IVR** (pre-generates greeting audio on save); the calling engine plays pre-existing files, never TTS live.

## Assignment & transfers

- `internal/assignment/assigner.go` dispatches on `teams.assignment_strategy`: `round_robin` (oldest `last_assigned_at`, NULLS FIRST), `load_balanced` (fewest active items via caller-provided counter — chats count AgentTransfers, calls count CallTransfers), `manual`. Availability = `users.is_available AND is_active`. Team config cached in Redis.
- Chats become "assigned" through **AgentTransfer** (`handlers/agent_transfers.go`), not directly: queue transfer (bot off), same-agent reassignment (if `AssignToSameAgent` and agent available), or team transfer. Out-of-hours sends the configured out-of-hours message instead. `contact.assigned_user_id` is pinned/cleared carefully (only when it pointed at the agent being removed).
- SLA: embedded `SLATracking` on AgentTransfer (deadlines, escalation levels 0–3) driven by the in-server `SLAProcessor` 1-min loop; client-inactivity reminders use contact columns `chatbot_last_message_at`/`chatbot_reminder_sent`.
- Each assignment broadcasts WS `agent_transfer_assign` and dispatches `transfer.assigned` webhook.

## Analytics

No rollup tables — **live SQL** everywhere except Meta analytics (proxied + Redis-cached):

- `handlers/analytics.go`: dashboard counts + period-over-period %.
- `handlers/agent_analytics.go`: agent metrics from `agent_transfers` aggregates + `user_availability_logs` break-time overlap math in Go. Quirk: users without `analytics:read` silently get only their own stats from the same endpoint.
- `handlers/meta_analytics.go`: proxies Meta `analytics | pricing_analytics | template_analytics | call_analytics`, granularity auto-adjust (HALF_HOUR ≤7d, MONTH ≥30d), future end-dates clamped to UTC today, TTL-per-granularity Redis cache.
