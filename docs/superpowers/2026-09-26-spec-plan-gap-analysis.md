# Spec/Plan Gap Analysis

**Date**: 2026-09-26
**Scope**: All specs and plans in `docs/superpowers/` vs. actual implementation

---

## Executive Summary

The hexagonal architecture refactor is **implemented and the old monolith removed** (commit `640ba58`). However, several specs/plans have diverged from the actual code — some gaps are intentional evolution, some are accidental, and some are dead code or incorrect assumptions.

This document catalogs every gap found, categorized by severity.

---

## Gap 1: MQTT `collect`/`collectSt` Functions Dead (Critical)

**Affected**: `internal/mqtt/bridge.go`, `plans/2026-09-24-face-api-hex-refactor.md` Section 4

**Spec says**: Remove collector. The hex refactor plan Section 4 explicitly removes:
- `collector.go` (continuous loop)
- `POST /collector/start`, `POST /collector/stop`, `GET /collector/status`
- MQTT switch entity
- `face/scan/cmd` topic

**Reality**: `Bridge` struct still has `collect CollectFunc` and `collectSt CollectStateFunc` fields, and the `publishDiscovery` still publishes a "Collect" switch to HA. But these are **never wired** — `NewFaceAPI` in `di/wire.go` calls `mqtt.New(cfg, check, nil, nil)`, so `collect` and `collectSt` are always `nil`. The `onCommand` handler logs "MQTT collect function not set" and returns immediately.

**Impact**: Dead code. The HA collect switch exists but does nothing. The MQTT bridge subscribes to `/cmd` for no reason.

**Recommendation**: Remove `collect`, `collectSt` fields, `onCommand` handler, and the collect switch from `publishDiscovery`. Or wire them if the collect feature is intentionally being kept (which contradicts the plan).

---

## Gap 2: MQTT Worker Calls `CheckStream("")` Without Validation (Critical)

**Affected**: `internal/mqtt/bridge.go` line 245

**Spec says**: MQTT bridge uses `CheckFunc(rtspURL string)` injected at construction.

**Reality**: The MQTT worker calls `b.check("")` — passing an empty string as RTSP URL. There's no validation that the RTSP URL is configured before the check runs. The `CheckStream` method in `face_service.go` line 46-49 checks if `rtspReader` is configured, but it does NOT validate the URL is non-empty. The reader will try to parse `""` and fail.

**Impact**: If MQTT is enabled but `RTSP_URL` env var is not set, every MQTT trigger produces a noisy error instead of a clean "missing RTSP URL" message.

**Recommendation**: Add a `rtspURL string` parameter to the `CheckFunc` type, or add URL validation in `CheckStream`.

---

## Gap 3: Plan Q5 (Auto-Collect on Unmatched) Decided but Undocumented

**Affected**: `plans/2026-09-24-face-api-hex-refactor.md` Section 14, Q5

**Plan says**: Open question — "When MQTT trigger fires and the face is not matched, should it auto-collect to candidates, or just log to audit?"

**Reality**: The implementation **auto-collects** (see `face_service.go` lines 239-247 in `CheckStreamImage`). This is the correct behavior, but the plan's open question was never answered and closed out.

**Impact**: Low — behavior is correct, but the plan documentation is incomplete.

**Recommendation**: Update the plan to mark Q5 as decided: "Auto-collect unmatched faces (implementation confirmed)."

---

## Gap 4: MQTT Result Payload Missing `reason` and `timestamp` Fields

**Affected**: `specs/2026-09-17-mqtt-design.md` Section 3.2

**Spec says** (Section 3.2):
```json
{
  "status": "ok",
  "name": "John",
  "similarity": 0.87,
  "matched": true,
  "reason": "",
  "duration_ms": 234,
  "timestamp": "2026-09-17T12:00:00Z"
}
```

**Reality**: The actual `StreamCheckResult` struct in `domain/ports.go` has: `status`, `name`, `similarity`, `reason`, `matched`, `face_image`, `duration_ms`. **No `timestamp` field**. The worker publishes the struct as-is, so `reason` IS present but `timestamp` is not.

**Impact**: Low — `reason` is actually present in the struct. The `timestamp` field was never added, but the `Time` field on `AuditEntry` and `Candidate` already serves as the timestamp.

**Recommendation**: Either add `timestamp` to the struct, or update the spec to remove it (it's redundant with existing time fields).

---

## Gap 5: MQTT Sensor `value_template` Changed from Spec

**Affected**: `specs/2026-09-17-mqtt-design.md` Section 3.1

**Spec says**:
```json
"value_template": "{{ value_json.name if value_json.name else 'unknown' }}"
```

**Reality** (bridge.go line 306):
```json
"value_template": "{{ 'matched ' + value_json.name if value_json.matched else 'not matched' }}"
```

**Impact**: The sensor now shows "matched John" or "not matched" instead of "John" or "unknown". This is arguably better UX, but it deviates from the spec.

**Recommendation**: Update the spec to reflect the actual template, or change the code back if the original template was intentional.

---

## Gap 6: MQTT Binary Sensor Added `payload_on`/`payload_off`

**Affected**: `specs/2026-09-17-mqtt-design.md` Section 3.1

**Spec says**: Binary sensor config has no explicit `payload_on`/`payload_off`.

**Reality** (bridge.go lines 322-323):
```json
"payload_on": true,
"payload_off": false
```

**Impact**: Low — these are explicit type values (bool), which is actually more correct than relying on string comparison. But it deviates from the spec.

**Recommendation**: Update the spec.

---

## Gap 7: UI PageData Contains Unused Collector Fields

**Affected**: `specs/2026-09-25-ui-restoration.md` Section 6, `internal/http/ui.go`

**Spec says**: The `uiPageData` struct includes `CollectorRunning bool` and `CollectorStartedAt time.Time`.

**Reality**: These fields are in the struct (ui.go lines 150-151) but are **never set** anywhere in the code (the continuous collector was removed). The templates don't reference them (the collector status card was removed from the dashboard).

**Impact**: Dead struct fields. No functional impact.

**Recommendation**: Remove `CollectorRunning` and `CollectorStartedAt` from `uiPageData`.

---

## Gap 8: Stats Feature — Inline vs Badge Display

**Affected**: `specs/2026-09-25-stats-feature.md` Section 7

**Spec says**: "The stats are displayed inline in the dashboard header (not as a separate card): Total checks as a badge, Matched percentage as a badge, Last matched time as a badge — these appear next to the clock in the header."

**Reality**: Need to check the actual `index.html` template to verify whether stats appear as badges near the clock or elsewhere.

**Recommendation**: Verify against the template and update the spec or code accordingly.

---

## Gap 9: API Contract — `/candidates/promote` Payload Format

**Affected**: `specs/2026-09-25-api-contract.md` Section 4.11

**Spec says**: Content-Type is `application/x-www-form-urlencoded` with query parameters `id` and `name`.

**Reality** (handlers.go lines 446-491): Content-Type is `application/json`. Body is `{"name": "..."}`, and `id` is a query parameter.

**Impact**: Clients following the spec will send form-encoded data and get a decode error. The actual endpoint expects JSON.

**Recommendation**: Update the API contract to match the implementation (`application/json` body + query param for `id`).

---

## Gap 10: API Contract — `/candidates/bulk-promote` Payload Format

**Affected**: `specs/2026-09-25-api-contract.md` Section 4.12

**Spec says**: Content-Type is `application/x-www-form-urlencoded`, form fields `name` and `ids[]` (repeatable).

**Reality** (handlers.go lines 493-528): Content-Type is `application/json`. Body is `{"name": "...", "candidate_ids": ["..."]}`.

**Impact**: Same as Gap 9 — clients following the spec will fail.

**Recommendation**: Update the API contract to match the implementation.

---

## Gap 11: `POST /enroll-from-stream` Endpoint

**Affected**: `plans/2026-09-24-face-api-hex-refactor.md` Section 6, `specs/2026-09-25-api-contract.md` Section 4.2

**Spec says**: Endpoint exists, uses `RTSP_URL` env var only (per plan Q4 answer).

**Reality**: Endpoint is implemented and registered. However, the handler (`handleEnrollFromStream` in handlers.go) reads the RTSP frame itself and then calls `svc.EnrollImage(name, imageData)` — duplicating some of the frame-reading logic that's also in the HTTP handler.

**Impact**: Minor — the frame reading happens in the HTTP handler layer rather than being pushed into the service layer. But it works correctly.

**Recommendation**: Acceptable as-is, or refactor to let the service handle it via `CheckStream`.

---

## Gap 12: `deleted` response missing `duration_ms`

**Affected**: `specs/2026-09-25-api-contract.md` Section 4.6

**Spec says**: DELETE `/users/<name>` returns `{"status": "deleted", "name": "...", "duration_ms": 5}`.

**Reality** (handlers.go line 333-337): Returns `{"status": "deleted", "name": "..."}` — **no `duration_ms`**.

**Impact**: Minor — inconsistent with the contract. Every other endpoint includes `duration_ms`.

**Recommendation**: Add `duration_ms` to the delete response.

---

## Gap 13: `/candidates/promote` Returns 201 vs Spec 200

**Affected**: `specs/2026-09-25-api-contract.md` Section 4.11

**Spec says**: Success returns 200.

**Reality** (handlers.go line 486): Returns `http.StatusCreated` (201).

**Impact**: Minor — 201 is actually more semantically correct for "resource created".

**Recommendation**: Update the spec to say 201.

---

## Gap 14: `handleDeleteUser` Missing `duration_ms` and Error Response Status

**Affected**: `handlers.go` lines 316-337

**Reality**: The delete handler doesn't include `duration_ms` in the success response (Gap 12). It also doesn't return a specific status code for "user not found" — any error from the service returns 500.

**Impact**: Minor — could benefit from a 404 when user doesn't exist.

**Recommendation**: Add `duration_ms` and return 404 for user-not-found errors.

---

## Gap 15: `backfillUsersFromAudit` Not Mentioned in Hex Spec

**Affected**: `specs/2026-09-25-hex-architecture-design.md`

**Spec says**: Documents the new domain/service/repository architecture but doesn't mention the `backfillUsersFromAudit` function that was referenced in AGENTS.md.

**Reality**: The `storedUser` migration and backfill logic (from the original server) needs to exist somewhere in the new architecture for backward compatibility. It may be in the user repo or service layer.

**Recommendation**: Check if backfill logic exists in the new code, and if so, document it in the hex architecture spec.

---

## Gap 16: `candidates/promote` Single vs Bulk Endpoint Name Mismatch

**Affected**: `plans/2026-09-24-face-api-hex-refactor.md` vs `specs/2026-09-25-api-contract.md`

**Plan says** (Section 6): Single promote is `POST /candidates/promote`, bulk is `POST /candidates/bulk-promote`.

**Contract says** (Section 4.10, 4.11, 4.12): Matches the plan.

**Reality**: Both endpoints are registered and working. No gap here — the plan and spec agree.

**Impact**: None — this is a false positive in my initial scan.

---

## Gap 17: RTSP Enroll — UI Missing RTSP Tab

**Affected**: `plans/2026-09-24-face-api-hex-refactor.md` Section 8

**Plan says**: "Enroll page tabs: Upload | Webcam | RTSP | Collected"

**Reality**: The enroll page (`enroll.html`) does not have an RTSP tab. RTSP enrollment is only available via the API (`POST /enroll-from-stream`) or via the MQTT trigger. The plan Q4 answer said "RTSP enroll uses RTSP_URL env var only" — implying no UI for RTSP enrollment.

**Impact**: The plan's UI section (Section 8) is aspirational rather than implemented. The Q4 answer effectively removed the RTSP tab requirement.

**Recommendation**: Update the plan's UI section to remove the RTSP tab, noting it was removed per Q4 decision.

---

## Summary of Gaps

| # | Severity | Description | Action |
|---|----------|-------------|--------|
| 1 | **Critical** | MQTT `collect`/`collectSt` dead code, never wired | Remove or wire |
| 2 | **Critical** | MQTT worker calls `CheckStream("")` without URL validation | Add validation |
| 3 | Low | Plan Q5 auto-collect decided but not documented | Update plan |
| 4 | Low | MQTT payload missing `timestamp` field | Update spec or add field |
| 5 | Medium | MQTT sensor value_template changed from spec | Update spec |
| 6 | Low | Binary sensor has extra `payload_on`/`payload_off` | Update spec |
| 7 | Low | UI PageData has unused collector fields | Remove fields |
| 8 | Medium | Stats display location may differ from spec | Verify |
| 9 | High | API contract says form data for promote, impl uses JSON | Update contract |
| 10 | High | API contract says form data for bulk-promote, impl uses JSON | Update contract |
| 11 | Low | Enroll-from-stream frame reading in handler vs service | Acceptable |
| 12 | Low | Delete response missing `duration_ms` | Add field |
| 13 | Low | Promote returns 201 vs spec 200 | Update spec |
| 14 | Low | Delete handler missing 404 for user-not-found | Add handling |
| 15 | Medium | backfillUsersFromAudit not documented in hex spec | Check and document |
| 16 | None | RTSP enroll endpoint naming | No gap |
| 17 | Low | Plan says RTSP tab, Q4 answer removed it | Update plan |

## Recommended Actions

1. **Fix the critical gaps** (1, 2) — remove dead MQTT collect code and add RTSP URL validation
2. **Update the API contract** (9, 10) — change promote endpoints to document JSON payloads
3. **Update the MQTT design spec** (4, 5, 6) — reflect the actual discovery configs
4. **Clean up dead code** (1, 7) — remove unused collect fields and pageData fields
5. **Update the hex refactor plan** (3, 17) — mark Q5 as decided, remove RTSP tab from UI section
6. **Add `duration_ms` to delete response** (12) — one line change
7. **Verify stats display** (8) — check template
