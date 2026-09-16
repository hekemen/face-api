# WebUI Implementation Plan

## Current State Assessment

The WebUI is **already implemented** at ~90% confidence. Here's what exists:

### Already Done
1. **Go templates** — `internal/server/templates/` contains 7 HTML templates (shared, index, enroll, recognize, stream-check, users, audit)
2. **Enable/disable by configuration** — `ENABLE_UI` env var (line 81 of `main.go`), passed to `NewFaceServer` as `enableUI` bool
3. **`/ui` path** — `RegisterUIHandlers` mounts at `/ui/` (line 29 of `ui.go`)
4. **All endpoints consumed**:
   - `GET/POST /ui/enroll` → proxies to `POST /enroll`
   - `GET/POST /ui/recognize` → proxies to `POST /recognize`
   - `GET/POST /ui/stream-check` → proxies to `POST /stream-check`
   - `GET /ui/users` → proxies to `GET /users`
   - `GET /ui/audit` → proxies to `GET /audit`
5. **Template rendering** — `renderTemplate` helper loads `shared.html` + page template via `template.ParseFS`
6. **UI proxy** — `proxyAPI` helper forwards requests to internal API endpoints

### What's Missing / Needs Verification

1. **`/ui/enroll` form action** — The form posts to `/enroll` (API root) instead of `/ui/enroll`. This works because the server proxies both, but the form action should be `/ui/enroll` for consistency.
   - File: `internal/server/templates/enroll.html:4`
   - Change: `action="/enroll"` → `action="/ui/enroll"`

2. **`/ui/recognize` form action** — Same issue, posts to `/recognize` instead of `/ui/recognize`.
   - File: `internal/server/templates/recognize.html:4`
   - Change: `action="/recognize"` → `action="/ui/recognize"`

3. **`/ui/stream-check` form action** — Posts to `/stream-check` instead of `/ui/stream-check`.
   - File: `internal/server/templates/stream-check.html:4`
   - Change: `action="/stream-check"` → `action="/ui/stream-check"`

4. **Dockerfile** — The Dockerfile copies `models/` but the templates are embedded via `//go:embed`, so they're compiled into the binary. No change needed.

5. **Helm chart** — No env var for `ENABLE_UI` in `values.yaml` or `values.pi.yaml`. By default, the UI will be disabled unless `ENABLE_UI=true` is set. Should we enable it by default?

## Questions (to reach 100% confidence)

1. **[Should ENABLE_UI default to true or false?]** Currently it defaults to `false` (only enabled when `ENABLE_UI=true`). For production deployments (Helm), should we enable it by default? yes

2. **[Should the form actions point to `/ui/*` or `/`?]** The current forms post to the API root (`/enroll`, `/recognize`, `/stream-check`). This works because the server handles both paths, but for UI consistency, they should post to `/ui/enroll`, `/ui/recognize`, `/ui/stream-check`. Confirm this is the desired behavior. /ui

3. **[Any styling improvements needed?]** The current templates use inline CSS with a basic layout. Should we add:
   - Dark mode support?
   - Responsive design improvements?
   - Face thumbnail previews on enroll/recognize pages?
   - Auto-refresh for the audit log or stream-check page?
add all
## Implementation Steps (if questions resolved)

1. Fix form actions in templates to use `/ui/*` paths
2. Add `ENABLE_UI` to Helm chart default values (if desired)
3. Run `make test` to verify nothing breaks
4. Update `.env` with `ENABLE_UI=true` for local development
