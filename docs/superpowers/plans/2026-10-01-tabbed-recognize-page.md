# Tabbed Recognize Page Plan

**Created**: 2026-10-01
**Status**: Ready to implement

## Overview

Redesign the recognize page (`recognize.html`) from a single flat form into a tabbed interface with three tabs:
1. **Picture** — upload a picture file
2. **Webcam** — capture from webcam
3. **RTSP** — check an RTSP stream using the configured `RTSP_URL`

The audit page already meets all requirements (no-face filtering, modal popup, promote), so no changes are needed there.

## Current state

- `recognize.html` has a single form with file input + webcam details + submit
- `shared.html` defines `{{template "camera"}}` and `{{template "imagepreview"}}` scripts
- Server already sends `.RTSPURL` in template data; handler supports `rtsp_url` form field
- No backend changes needed

## Tasks

### Task 1: Redesign `recognize.html` with tabbed layout

**File**: `/home/hekemen/face-api/internal/http/templates/recognize.html`

**Changes**:
- Replace the flat form with a `<nav role="tablist">` containing three tab buttons: "Picture", "Webcam", "RTSP"
- Each tab has a `<section role="tabpanel">` containing its form elements:
  - **Picture tab**: file input, preview container, submit button
  - **Webcam tab**: camera button (or active camera stage with capture/stop), submit button
  - **RTSP tab**: label showing the configured URL, hidden `rtsp_url` field, submit button
- The form `action` remains `recognize`, `method="POST"`, `enctype="multipart/form-data"`
- JavaScript handles tab switching: clicking a tab hides all panels, shows the selected one, updates `aria-selected`
- Webcam and picture tabs share the same hidden `input[name="image"]` (the camera script writes to it; picture input writes to it on file select)
- RTSP tab does NOT need the `image` input — the form still sends `rtsp_url`
- Use existing `{{template "camera"}}` and `{{template "imagepreview"}}` from `shared.html`
- RTSP tab shows the URL from `.RTSPURL` template variable (already set by server)

**Key JavaScript**:
```javascript
// Tab switching
document.querySelectorAll('[role="tablist"] [role="tab"]').forEach(btn => {
  btn.addEventListener('click', () => {
    // Hide all panels, show selected; update aria-selected
  });
});

// On page load, show the first tab (Picture)
```

**Template data**: Server sends `.RTSPURL` (string) — already done. The RTSP tab renders:
```html
<div class="rtsp-info">
  <label>RTSP URL:</label>
  <p>{{.RTSPURL}}</p>
</div>
<input type="hidden" name="rtsp_url" value="{{.RTSPURL}}">
```

### Task 2: Verify camera script compatibility

**File**: `/home/hekemen/face-api/internal/http/templates/shared.html`

**Check**: The `{{template "camera"}}` script writes a JPEG `File` to `input[name="image"]` via `DataTransfer`. Verify it still works with the tabbed layout:
- The camera stage div is inside its tab panel (not the main form)
- The hidden `input[name="image"]` should still be accessible
- The submit button in each tab should be in the same `<form>` element

**No changes expected** — but verify the camera script's `document.getElementById('image')` still finds the input when it's in a different tab.

### Task 3: Rebuild and deploy

1. Rebuild Docker image: `make docker-build-pi5` or `docker buildx build --platform linux/arm64 ...`
2. Push to GHCR
3. Deploy to K3s: `helm upgrade face-api deploy/helm/face-api -n face-api -f deploy/helm/face-api/values.pi.yaml --set image.tag=<new-tag>`
4. Verify via `kubectl exec` or port-forward

## Files to change

1. `/home/hekemen/face-api/internal/http/templates/recognize.html` — redesign with tabs
2. (Verification only) `/home/hekemen/face-api/internal/http/templates/shared.html` — check camera compatibility

## No changes needed

- Backend handlers (`handlers.go`) — already support `rtsp_url` form field
- Server template data — already sends `.RTSPURL`
- Audit page — already has all requested features
- shared.html scripts — compatible with new layout
