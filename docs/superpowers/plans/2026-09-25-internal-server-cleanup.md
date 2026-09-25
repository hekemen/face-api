# Clean Up Old `internal/server/` Package

**Status**: planned
**Date**: 2026-09-25
**Related Plan**: `plans/2026-09-24-face-api-hex-refactor.md` (Phase 7: Tests + Cleanup)

---

## 1. Intent

Remove the old monolithic `internal/server/` package that coexists alongside the new hexagonal architecture packages. The hex packages have been verified by passing all tests (internal 51/51, E2E 10/10, RTSP 5/5).

## 2. Current State

### Files to Remove

| File | Lines | Replaced By |
|------|-------|-------------|
| `internal/server/collector.go` | ~200 | Removed (no continuous collector) |
| `internal/server/face.go` | ~200 | `internal/service/face_processor.go` |
| `internal/server/img.go` | ~100 | Moved into `internal/service/face_processor.go` |
| `internal/server/rtsp.go` | ~150 | `internal/rtsp/reader.go` |
| `internal/server/rtsp_h264_fixture_test.go` | ~50 | Test fixtures moved |
| `internal/server/server.go` | ~700 | Split across `internal/http/`, `internal/service/`, `internal/domain/` |
| `internal/server/types.go` | ~50 | `internal/domain/models.go` + `ports.go` |
| `internal/server/ui.go` | ~450 | `internal/http/ui.go` |

### Files to Keep (reference only)

| File | Reason |
|------|--------|
| `internal/server/candidates_test.go` | Old test — may be useful as reference |
| `internal/server/img_test.go` | Old test — may be useful as reference |
| `internal/server/rtsp_test.go` | Old test — replaced by `test/rtsp/` |
| `internal/server/ui_test.go` | Old test — replaced by hex-aware tests |
| `internal/server/users_test.go` | Old test — replaced by domain/service tests |

### Test Status (all passing before cleanup)

| Suite | Tests | Status |
|-------|-------|--------|
| `internal/` unit | 51 | ✅ PASS |
| `test/e2e` | 10 | ✅ PASS |
| `test/rtsp` | 5 | ✅ PASS |

## 3. Cleanup Steps

### Step 1: Verify Tests Pass

```bash
cd /home/hekemen/face-api
go test ./... -count=1 -v 2>&1 | tail -20
```

### Step 2: Remove Old Implementation Files

```bash
rm internal/server/collector.go
rm internal/server/face.go
rm internal/server/img.go
rm internal/server/rtsp.go
rm internal/server/rtsp_h264_fixture_test.go
rm internal/server/server.go
rm internal/server/types.go
rm internal/server/ui.go
```

### Step 3: Keep Test Files as Reference (Optional)

If useful for reference, rename:
```bash
mv internal/server/candidates_test.go internal/server/candidates_test.go.bak
mv internal/server/img_test.go internal/server/img_test.go.bak
mv internal/server/rtsp_test.go internal/server/rtsp_test.go.bak
mv internal/server/ui_test.go internal/server/ui_test.go.bak
mv internal/server/users_test.go internal/server/users_test.go.bak
```

Or delete them entirely.

### Step 4: Re-run All Tests

```bash
go test ./... -count=1
```

### Step 5: Update Documentation

Update this plan to mark as complete. Update hex architecture spec if any paths changed.

## 4. Risk Assessment

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| Old code contains logic not migrated | Low | High | Tests cover all endpoints; diff old vs new before delete |
| bbolt compatibility issues | Low | Medium | bbolt code in new repos uses same bucket names/keys |
| ONNX inference regression | Low | High | E2E tests cover detection + recognition |
| UI regression | Low | Medium | UI handlers tested separately |

## 5. Rollback Plan

If tests fail after deletion:
1. `git checkout HEAD -- internal/server/` to restore old code
2. Re-run tests to verify old code works
3. Investigate what was missed in the migration
4. Fix and re-attempt

## 6. Post-Cleanup State

```
internal/
├── domain/       ✅ pure models + interfaces
├── service/      ✅ business logic
├── repository/   ✅ bbolt persistence
├── http/         ✅ HTTP + UI handlers
├── rtsp/         ✅ RTSP transport
├── mqtt/         ✅ MQTT bridge
├── di/           ✅ dependency injection
└── server/       ❌ empty (or .bak files for reference)
```

## 7. Checklist

- [ ] All tests pass (`go test ./...`)
- [ ] Old `server.go` logic diff-checked against new packages
- [ ] Remove old implementation files
- [ ] Remove or rename old test files
- [ ] Re-run all tests after deletion
- [ ] Update this plan to mark complete
- [ ] Push to main branch
