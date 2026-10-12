# Design: Settings Page (Match Sensitivity)

**Date:** 2026-10-12
**Status:** Approved
**Author:** AI agent

## 1. Problem

The face-match sensitivity (recognition threshold, default 0.45) is currently only configurable via the `THRESHOLD` env var at process start. Operators cannot adjust it at runtime, and any change requires editing deployment config and restarting the pod. There is no in-app way to see or tune it.

## 2. Goals

- Add a **Settings page** (`/ui/settings`) reachable from a `⚙️` button in the page header, placed next to the `⟳ Auto` auto-reload control (both `subpageHeader` and `dashHeader`).
- The page lets the user change the **match sensitivity** (cosine-similarity threshold) and includes a plain-English description of what it does.
- Changes take effect **immediately** (live in-memory update) and **persist across restarts** in bbolt (new `Settings` bucket), overriding the `THRESHOLD` env var. The env var remains the initial default when no persisted value exists.
- Expose the value via API: `GET /settings` and `POST /settings`.

## 3. Non-Goals

- No other settings on the page for now (the data model is extensible for future settings).
- No per-user thresholds.
- No UI for the MQTT / RTSP config.

## 4. Domain Changes (`internal/domain/`)

### 4.1 `Settings` (models.go)

```go
// Settings holds runtime-configurable application settings.
type Settings struct {
    Threshold float32 `json:"threshold"` // face-match cosine-similarity threshold
}
```

### 4.2 `SettingsRepository` port (ports.go)

```go
// SettingsRepository persists runtime settings.
type SettingsRepository interface {
    // Get returns the stored settings, or nil when none are stored yet.
    Get() (*Settings, error)
    // Save stores the settings (upsert).
    Save(s *Settings) error
}
```

### 4.3 `FaceService` interface additions

```go
// GetSettings returns the current runtime settings (live values).
GetSettings() (*Settings, error)
// SetThreshold updates the face-match threshold. The value must be in
// (0, 1). It is persisted before the in-memory value is updated.
SetThreshold(v float32) error
```

## 5. Repository (`internal/repository/settings_repo.go`)

- bbolt bucket `"Settings"` (added to `EnsureBuckets` in `di/wire.go`).
- Single JSON blob under key `"settings"` — extensible for future settings.
- `Get`: missing key → `nil, nil`; present → unmarshal.
- `Save`: marshal + write inside one `db.Update`.

## 6. Service (`internal/service/face_service.go`)

- New field `settings domain.SettingsRepository`.
- `WithSettingsRepo(repo *repository.SettingsRepository)`-style setter (interface type, matching `WithLogger`/`WithRTSPReader` pattern). When called, it reads the stored settings and, if present and valid, overrides the constructor/env-var threshold in memory (no write-back).
- `GetSettings()`: returns `&Settings{Threshold: s.threshold}` under the existing mutex.
- `SetThreshold(v)`:
  1. Validate `0 < v < 1` → error `threshold must be between 0 and 1` otherwise.
  2. Persist via settings repo (error → return error, memory untouched).
  3. Update `s.threshold` under `mu`.

## 7. HTTP API (`internal/http/handlers.go`)

Registered on the main mux, so the existing auth middleware (API token / UI cookie) applies automatically.

- `GET /settings` → `200 {"threshold": 0.45, "duration_ms": N}`
- `POST /settings` (form field `threshold`, house style is form-data) →
  - valid: `200 {"status": "updated", "threshold": 0.5, "duration_ms": N}`
  - missing/invalid/out-of-range: `400` with error message
- Response types: `SettingsResponse{OperationDuration, Threshold float32 \`json:"threshold"\`}` and an update result struct in `domain/ports.go` (response types live there).

## 8. UI (`internal/http/ui.go`, `templates/`)

### 8.1 Header button

- In **both** `subpageHeader` and `dashHeader` (shared.html), add to `.header-controls` after the auto toggle:
  `<a class="settings-link" href="settings" title="Settings">⚙️</a>`
- Relative `href` (stripPrefix constraint). New CSS `.settings-link` styled like `.theme-toggle`/`.auto-toggle` (bordered pill, hover highlight).

### 8.2 `settings.html` page

- `{{template "subpageHeader" .}}` … `{{template "foot" .}}`.
- Title "Settings".
- **Match sensitivity** section:
  - Description: face matching compares the detected face against every enrolled photo and computes a similarity score between 0 and 1; the best score must meet or exceed the threshold to count as a match. Lower = more permissive (more matches, more false positives); higher = stricter (fewer matches, fewer false positives). Default 0.45.
  - Form (POST, `action="settings"`): number input `threshold` (min 0.01, max 0.99, step 0.01) prefilled with the current value, Save button.
  - `.result` / `.result.error` feedback area (same convention as other pages).

### 8.3 `handleUISettings`

- GET: `svc.GetSettings()` → render `settings.html` with current threshold.
- POST: parse `threshold` form value; call `svc.SetThreshold` directly (same pattern as user delete — no proxy needed); re-render with success or error message.
- Dispatch: add `"settings"` case to `handleUI` switch.

## 9. Wiring (`internal/di/wire.go`)

- `EnsureBuckets` gains `"Settings"`.
- `settingsRepo := repository.NewSettingsRepository(cfg.DB)`; `faceService.WithSettingsRepo(settingsRepo)` (applies persisted override at startup).

## 10. Validation & Precedence

- Threshold bounds: `0 < v < 1` (env var parsing keeps its existing `0..1` bounds at startup; a persisted value is only applied if valid).
- Precedence at startup: persisted `Settings` value > `THRESHOLD` env var > default 0.45.

## 11. Tests

**Unit**

- `settings_repo_test.go`: `Get` on empty DB → `nil, nil`; Save → Get round-trip.
- `face_service_test.go`: `SetThreshold` rejects `0`, `1`, `1.5`, `-0.1`; accepts valid value and updates `GetSettings`; `WithSettingsRepo` applies a stored override; no stored value keeps constructor threshold.
- `ui_test.go`: settings page renders (form input prefilled, description text present, `⚙️` header link present in both headers); POST valid → success message + updated value; POST invalid → error message.
- `handlers_test.go` (or existing handler tests): `GET /settings` 200 with threshold; `POST /settings` valid → 200 updated; out-of-range → 400.

**E2E** (`test/e2e/e2e_test.go`)

- `GET /settings` → 200, `threshold` == 0.45 (fresh DB).
- `POST /settings` `threshold=0.5` → 200 `status=updated`; `GET /settings` → 0.5.
- `POST /settings` `threshold=1.5` → 400.
- `GET /ui/settings` (with auth cookie) → contains the form, the description, and the header settings link.

## 12. Docs

- AGENTS.md: add `GET /settings` / `POST /settings` to the endpoint list; note the settings page + header button; note persisted-threshold precedence over `THRESHOLD`.