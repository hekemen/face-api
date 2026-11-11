// Package config parses environment variables for the face API.
package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RedactURL removes the userinfo (credentials) from a URL so it can be
// written to logs. A URL that cannot be parsed is returned unchanged.
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("redacted")
	return u.String()
}

const (
	envOnnxIntraOpThreads = "ONNX_INTRA_OP_THREADS"
	envFaceAlignment      = "FACE_ALIGNMENT"
	envTriggerBurstMS     = "TRIGGER_BURST_MS"
	envTriggerSampleMS    = "TRIGGER_SAMPLE_MS"
)

const (
	defaultIntraOpThreads = 4
	defaultBurstWindowMS  = 4000
	minBurstWindowMS      = 500
	maxBurstWindowMS      = 10000

	defaultSampleIntervalMS = 200
	minSampleIntervalMS     = 50
	maxSampleIntervalMS     = 1000
)

// IntraOpThreads parses ONNX_INTRA_OP_THREADS: 0 (ORT default) or an integer
// >= 1. An unset or invalid value yields the default (4) together with an error
// so the caller can log it and keep the default.
func IntraOpThreads(getenv func(string) string) (int, error) {
	raw := getenv(envOnnxIntraOpThreads)
	if raw == "" {
		return defaultIntraOpThreads, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultIntraOpThreads, fmt.Errorf("invalid ONNX_INTRA_OP_THREADS %q, using %d", raw, defaultIntraOpThreads)
	}
	return n, nil
}

// Alignment parses FACE_ALIGNMENT: "" (unset) defaults to "bbox"; valid values
// are "bbox" and "arcface5" (case-insensitive, surrounding space trimmed).
// Any other non-empty value is rejected with an error — never silently coerced.
func Alignment(getenv func(string) string) (string, error) {
	raw := strings.TrimSpace(getenv(envFaceAlignment))
	raw = strings.ToLower(raw)
	if raw == "" {
		return "bbox", nil
	}
	switch raw {
	case "bbox", "arcface5":
		return raw, nil
	default:
		return "", fmt.Errorf("invalid FACE_ALIGNMENT %q, want %q or %q", raw, "bbox", "arcface5")
	}
}

// BurstWindow parses TRIGGER_BURST_MS: the burst window in milliseconds,
// defaulting to 4000 ms and clamped to [500, 10000] ms. Unparseable values fall
// back to the default; the result is always a usable duration.
func BurstWindow(getenv func(string) string) time.Duration {
	ms := defaultBurstWindowMS
	if raw := getenv(envTriggerBurstMS); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			ms = n
		}
	}
	if ms < minBurstWindowMS {
		ms = minBurstWindowMS
	}
	if ms > maxBurstWindowMS {
		ms = maxBurstWindowMS
	}
	return time.Duration(ms) * time.Millisecond
}

// SampleInterval parses TRIGGER_SAMPLE_MS: the minimum gap between scored
// frames, defaulting to 200 ms and clamped to [50, 1000] ms. Unparseable values
// fall back to the default; the result is always a usable duration.
func SampleInterval(getenv func(string) string) time.Duration {
	ms := defaultSampleIntervalMS
	if raw := getenv(envTriggerSampleMS); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			ms = n
		}
	}
	if ms < minSampleIntervalMS {
		ms = minSampleIntervalMS
	}
	if ms > maxSampleIntervalMS {
		ms = maxSampleIntervalMS
	}
	return time.Duration(ms) * time.Millisecond
}
