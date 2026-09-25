# Frontend (frontend/ — Vue 3 + Vite + TS)

## Shape

- Entry `src/main.ts`: Pinia, router, VueQueryPlugin, i18n. Views in `src/views/` by feature (`chat/`, `chatbot/`, `calling/`, `settings/`, `auth/`, `analytics/`, `dashboard/`, `profile/`). **Route→dir mismatch trap:** `/campaigns`, `/templates`, `/contacts` views live under `views/settings/`.
- One router (`src/router/index.ts`), one API module (`src/services/api.ts`, ~1250 lines — ALL ~30 service objects + shared TS interfaces), one websocket module (`src/services/websocket.ts`).
- Route meta carries `permission` (dotted resource, checked as `read`); the guard redirects unauthorized users to the first accessible route from a hardcoded `navigationOrder`. Custom `meta.stableKey` keeps `<component :is>` alive across param changes (e.g. `/canned-responses/new` → `/:id`).

## API client (`src/services/api.ts`)

- **Auth is httpOnly cookies — no bearer tokens anywhere.** Axios instance `baseURL = VITE_API_URL || \`${window.__BASE_PATH__}/api\``, `withCredentials: true`, 30s timeout.
- Request interceptor adds `X-CSRF-Token` (from `whm_csrf` cookie) on mutations and `X-Organization-ID` (from `localStorage.selected_organization_id`) — same logic in `getRequestHeaders()` for raw `fetch` calls.
- 401 → one retry after `POST /auth/refresh`; refresh is **serialized across tabs via the Web Locks API** (`navigator.locks.request('whm-token-refresh', ...)`) because refresh tokens are single-use/rotating. Failure → clear localStorage, hard redirect to login.
- Envelopes are inconsistent (`{data:{data:T}}` vs `{data:T}`) — use `unwrapResponse`/`unwrapListResponse` from `src/lib/api-utils.ts` or the `response.data.data || response.data` idiom. Backend leaks PascalCase aliases in places (`flow.Name`, `flow.IsEnabled`) — the flow builder tolerates both.
- `VITE_WS_URL` is documented in `.env.example` but **nothing reads it** — WS URL derives from `window.location`. `__BASE_PATH__` is server-injected at runtime; all URLs must respect it.

## State

- 11 Pinia setup-style stores in `src/stores/`: `auth` (user, `hasPermission(resource, action)`, break/availability), `contacts` (contacts **and** messages for the open chat — there is no separate messages store; WS-fed with duplicate checking), `transfers` (SLA fields), `calling` (608 lines — logs, IVR, WebRTC peer connections, central `handleCallEvent`), `notes`, `organizations` (`selectedOrgId`, `isMultiOrg`), plus CRUD wrappers `roles/tags/teams/users`.
- **TanStack vue-query is registered but completely unused** — zero `useQuery` anywhere. The pattern is stores + composables + manual fetch (`useCrudState`, `useSearchPagination`). Don't introduce query-key conventions.

## Auth & permissions in UI

- Login → cookies set by server → only the user object goes to `localStorage['user']`. `restoreSession()` is a synchronous localStorage read; expired sessions are handled by the axios interceptor.
- Permission checks are **inline**: `v-if="authStore.hasPermission('organizations', 'assign')"` (26+ files). No `v-permission` directive, no wrapper component — don't invent one. Sidebar gating is data-driven via `NavItem.permission` in `components/layout/navigation.ts`.
- `hasPermission` expects `role.permissions` entries as `{resource, action}` objects, while the roles API interface uses `"resource:action"` strings — two shapes exist on purpose.
- `permissions_updated` WS event → `refreshUserData()` + full page reload.

## UI conventions

- shadcn-vue, style `new-york`, primitives are **reka-ui** (not radix-vue). Generated components in `src/components/ui/` (48 dirs); shared app components in `src/components/shared/` (DataTable, CrudFormDialog, ConfirmDialog, FlowCanvas, ...).
- **Dark-first theming**: Tailwind `darkMode: ["class"]` plus a custom `light:` variant — templates write plain classes for dark and `light:` prefixes for light mode, **not the usual `dark:`**. `:root` CSS vars hold dark values; `.light` overrides. Default mode is dark (`useColorMode` composable, FOUC script in index.html).
- Toasts: `vue-sonner` (`<Toaster>` in App.vue) — not the shadcn `ui/toast` dir. Icons: lucide-vue-next. Drawer: vaul-vue. Charts: chart.js + vue-chartjs. Emoji: vue3-emoji-picker.
- Forms: vee-validate only exists inside the generated `ui/form` shim; real views use plain `ref` formData + `useCrudState` (dialog/delete state machine) + `useSearchPagination` (debounced search + page). Radix `<SelectItem>` can't take `value=""` — sentinel `'__all'` used.
- Vite: `base: './'`, dev proxy `/api` and `/ws` → localhost:8080, port 3000, console/debugger dropped in build.

## Realtime (`src/services/websocket.ts`)

Singleton `wsService`. Connect flow: `AppLayout` onMounted → `GET /auth/ws-token` → token sent as **first WS message** `{type:'auth', payload:{token}}` (never a query param). Ping/pong every 30s. Reconnect: exponential backoff 1s→30s cap, never gives up; plus visibility/online/pageshow instant reconnects; after reconnect, stale contacts+transfers are refetched. `new_message` handling is subtle: insert only when viewing that contact; toast/sound only when assigned to me; auto-mark-read **only when the tab is visible AND focused** (avoids fake blue ticks).

## i18n

`src/i18n/index.ts` auto-discovers `locales/*.json` via `import.meta.glob` — dropping a new file adds it to the switcher. `en.json` is the schema (`MessageSchema`). Crowdin syncs via `frontend/crowdin.yml`. Key trees: `common.*`, `auth.*`, `nav.*`, `resources.*`.

## Testing

- **Vitest** (`npm run test:unit`): `include: ['src/**/*.spec.ts']`, **environment `node`** (not happy-dom) — DOM-dependent unit tests need a config change; browser behavior belongs to Playwright. Only two specs exist (`lib/whatsappButtons.spec.ts`, `stores/calling.spec.ts`).
- **Playwright** (`npm run test:e2e`, `BASE_URL=http://localhost:8080` against the Go backend): 54 specs under `e2e/tests/`, Page Object Model in `e2e/pages/`, helpers in `e2e/helpers/` (ApiHelper handles cookie+CSRF). **`e2e/framework/scope.ts` `createTestScope(specName)`** mints a run-unique `E2E-<spec>-<runid>` prefix for every name/email/phone — there is no per-test cleanup, so new specs must use it. `global-setup.ts` cleans the dev DB, logs in as `admin@admin.com`/`admin`, extracts CSRF from Set-Cookie. Read `e2e/ARCHITECTURE.md` first — "if code contradicts the doc, the doc wins".
- Typecheck: `npm run typecheck` = `vue-tsc --noEmit`, strict, covers `src/` only (not `e2e/`).

## Flow editors (@vue-flow)

- Shared canvas wrapper `src/components/shared/FlowCanvas.vue` (snap grid, delete-key handling) used by **both** the chatbot editor (`src/views/chatbot/ChatbotFlowBuilderView.vue`) and the IVR editor (`src/views/calling/IVRFlowEditorView.vue`).
- Chatbot save payload: `{name, description, trigger_keywords, initial_message, completion_message, on_complete_action, completion_config, panel_config, enabled, graph}` where `graph = {version: 2, nodes[{id,type,label,position,config}], edges[{from,to,condition}], entry_node}`. Entry point is a fixed `__start__` sentinel node; legacy graphs without it are self-healed on load. Edge `condition` comes from the vue-flow sourceHandle (`default`, `button:<id>`, `true/false`, `in_hours/out_of_hours`); one edge per source handle.
- **IVR editor saves the same v2 graph under the legacy key `menu`** (`{name, is_active, is_call_start, is_outgoing_end, menu: IVRFlowData}`) — keep that key when touching IVR persistence.
- Live simulation for previews: `useFlowSimulation` / `useFlowGraphSimulation` / `useConditionEvaluator` / `useApiMocker`; history via `useFlowHistory`.
