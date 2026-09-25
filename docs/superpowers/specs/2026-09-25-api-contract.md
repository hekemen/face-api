# Face API — HTTP API Contract

**Status**: implemented + documented
**Date**: 2026-09-25

---

## 1. Base URL

| Environment | Base URL |
|-------------|----------|
| Direct access | `http://<host>:8081` |
| Behind Traefik (stripPrefix: /face) | `http://<host>/face` |

All paths are relative to the base URL. Content-Type is `application/json` unless noted.

## 2. Authentication

No authentication is required for any endpoint. The API is intended for trusted internal use (behind Traefik ingress on a local network).

## 3. Response Format

Every response includes an `duration_ms` integer (wall-clock time of the operation):

```json
{
  "duration_ms": 42
}
```

Error responses use standard HTTP status codes with a JSON body:

```json
{
  "error": "Missing required field: name"
}
```

## 4. Endpoints

### 4.1 `POST /enroll`

Enroll a face from an image upload.

- **Content-Type**: `multipart/form-data`
- **Fields**:
  - `name` (required): User name (string)
  - `image` (required): Image file (JPEG/PNG)
- **Max pictures**: 3 per user. The 4th enrollment for the same name returns 400.

**Success (201):**
```json
{
  "status": "enrolled",
  "name": "Anthony Hopkins",
  "duration_ms": 87
}
```

**Error (400):**
```json
{
  "error": "Missing required field: name"
}
```

**Error (400):**
```json
{
  "error": "Maximum 3 pictures per user reached"
}
```

### 4.2 `POST /enroll-from-stream`

Enroll a face from an RTSP stream. Uses `RTSP_URL` env var (no form field).

- **Content-Type**: `multipart/form-data`
- **Fields**:
  - `name` (required): User name

**Success (201):**
```json
{
  "status": "enrolled",
  "name": "Anthony Hopkins",
  "duration_ms": 342
}
```

### 4.3 `POST /recognize`

Recognize a face from an image upload.

- **Content-Type**: `multipart/form-data`
- **Fields**:
  - `image` (required): Image file

**Success (200):**
```json
{
  "duration_ms": 45,
  "name": "Anthony Hopkins",
  "similarity": 0.873,
  "matched": true
}
```

**Success (200) — unknown:**
```json
{
  "duration_ms": 38,
  "name": "Unknown",
  "similarity": 0.32,
  "matched": false
}
```

### 4.4 `POST /stream-check`

Check an RTSP stream for a recognized face. Single frame.

- **Content-Type**: `multipart/form-data`
- **Fields**:
  - `rtsp_url` (optional): RTSP URL; falls back to `RTSP_URL` env var

**Success (200) — matched:**
```json
{
  "status": "ok",
  "name": "Anthony Hopkins",
  "similarity": 0.873,
  "matched": true,
  "duration_ms": 210
}
```

**Success (200) — no face:**
```json
{
  "status": "not ok",
  "reason": "No face detected",
  "duration_ms": 180
}
```

**Success (200) — not matched:**
```json
{
  "status": "not ok",
  "name": "",
  "similarity": 0.32,
  "reason": "No matching user found",
  "matched": false,
  "duration_ms": 210
}
```

**Error (400):**
```json
{
  "error": "Neither rtsp_url form field nor RTSP_URL environment variable is set"
}
```

### 4.5 `GET /users`

List all enrolled users, sorted by name.

**Success (200):**
```json
{
  "duration_ms": 2,
  "users": [
    {
      "name": "Anthony Hopkins",
      "pictures": [
        "base64-encoded-jpeg...",
        "base64-encoded-jpeg..."
      ],
      "updated_at": "2026-09-25T10:30:00Z"
    },
    {
      "name": "Bruce Willis",
      "pictures": [
        "base64-encoded-jpeg..."
      ],
      "updated_at": "2026-09-24T08:15:00Z"
    }
  ]
}
```

### 4.6 `DELETE /users/<name>`

Delete a user by name.

**Success (200):**
```json
{
  "status": "deleted",
  "name": "Anthony Hopkins",
  "duration_ms": 5
}
```

### 4.7 `GET /audit`

List recent audit entries (newest first, up to 100).

**Success (200):**
```json
{
  "duration_ms": 3,
  "count": 100,
  "entries": [
    {
      "time": "2026-09-25T10:45:00Z",
      "endpoint": "recognize",
      "name": "Anthony Hopkins",
      "similarity": 0.873,
      "matched": true,
      "duration_ms": 42,
      "face_image": "base64-encoded-jpeg"
    },
    {
      "time": "2026-09-25T10:44:30Z",
      "endpoint": "stream-check",
      "name": "",
      "similarity": 0.22,
      "matched": false,
      "duration_ms": 180,
      "face_image": ""
    }
  ]
}
```

### 4.8 `GET /api/audit`

Paginated audit entries with filtering support.

**Query Parameters:**
- `page` (default: 1): Page number
- `per_page` (default: 50): Items per page
- `name` (optional): Substring match on name (case-insensitive)
- `endpoint` (optional): Exact match on endpoint
- `matched` (optional): "yes", "no", or "" for all

**Success (200):**
```json
{
  "duration_ms": 5,
  "entries": [...],
  "total_count": 234,
  "page": 1,
  "per_page": 50,
  "total_pages": 5
}
```

### 4.9 `GET /stats`

Aggregate statistics from the audit log.

**Success (200):**
```json
{
  "duration_ms": 2,
  "total_checks": 456,
  "total_matched": 234,
  "total_no_face": 89,
  "total_not_matched": 133,
  "last_matched": "2026-09-25T10:45:00Z"
}
```

### 4.10 `GET /candidates`

List collected candidate faces, grouped by similarity.

**Success (200):**
```json
{
  "duration_ms": 3,
  "groups": [
    {
      "id": "uuid-1",
      "face_count": 3,
      "best_similarity": 0.72,
      "faces": [
        {
          "id": "uuid-1a",
          "embedding": [0.1, 0.2, ...],
          "face_image": "base64-encoded-jpeg",
          "time": "2026-09-25T10:00:00Z",
          "stream_url": "rtsp://..."
        }
      ]
    }
  ]
}
```

### 4.11 `POST /candidates/promote`

Promote a single candidate to an enrolled user.

- **Content-Type**: `application/x-www-form-urlencoded`
- **Query Parameters**:
  - `id` (required): Candidate ID
  - `name` (required): New user name

**Success (200):**
```json
{
  "status": "promoted",
  "name": "New User",
  "duration_ms": 56
}
```

### 4.12 `POST /candidates/bulk-promote`

Promote multiple candidates to a single user.

- **Content-Type**: `application/x-www-form-urlencoded`
- **Form Fields**:
  - `name` (required): New user name
  - `ids[]` (required): Candidate IDs (repeatable)

**Success (200):**
```json
{
  "status": "promoted",
  "name": "New User",
  "duration_ms": 78
}
```

### 4.13 `GET /healthz`

Liveness check. Always returns 200 while the process runs.

**Success (200):**
```json
{
  "status": "ok"
}
```

### 4.14 `GET /readyz`

Readiness check. Returns 200 once bbolt is open and cache is hydrated.

**Success (200):**
```json
{
  "status": "ok"
}
```

## 5. UI Endpoints

All UI endpoints are prefixed with `/ui/` and served from the same base URL.

| Endpoint | Description |
|----------|-------------|
| `GET /ui/` | Dashboard (index) |
| `GET /ui/enroll` | Enroll page |
| `POST /ui/enroll` | Enroll form (proxied to `POST /enroll`) |
| `POST /ui/enroll/delete` | Delete user form (proxied to `DELETE /users/<name>`) |
| `GET /ui/recognize` | Recognize page |
| `POST /ui/recognize` | Recognize form (proxied to `POST /recognize`) |
| `GET /ui/audit` | Audit page |
| `GET /ui/stats` | Stats page |
| `GET /ui/candidates` | Collected faces page |
| `GET /ui/pico.min.css` | Vendored PicoCSS stylesheet |

## 6. Thresholds

| Parameter | Default | Description |
|-----------|---------|-------------|
| Detection threshold | 0.5 | Minimum confidence for face detection |
| Recognition threshold | 0.45 | Minimum cosine similarity for a match |

## 7. Rate Limiting

No rate limiting is implemented. The API is intended for trusted internal use only.
