package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestIntraOpThreads(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{"unset defaults to 4", "", 4, false},
		{"valid value 8", "8", 8, false},
		{"zero means ORT default", "0", 0, false},
		{"invalid value falls back to 4", "abc", 4, true},
		{"negative falls back to 4", "-2", 4, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := map[string]string{"ONNX_INTRA_OP_THREADS": tt.raw}
			getenv := func(k string) string { return m[k] }

			got, err := IntraOpThreads(getenv)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("IntraOpThreads(%q) expected error, got nil (value %d)", tt.raw, got)
				}
				if got != defaultIntraOpThreads {
					t.Errorf("IntraOpThreads(%q) on error = %d, want default %d", tt.raw, got, defaultIntraOpThreads)
				}
				if wantMsg := fmt.Sprintf("invalid ONNX_INTRA_OP_THREADS %q, using %d", tt.raw, defaultIntraOpThreads); err.Error() != wantMsg {
					t.Errorf("IntraOpThreads(%q) error = %q, want %q", tt.raw, err.Error(), wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("IntraOpThreads(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("IntraOpThreads(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

func TestAlignment(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"empty defaults to bbox", "", "bbox", false},
		{"valid bbox", "bbox", "bbox", false},
		{"valid arcface5", "arcface5", "arcface5", false},
		{"upper case normalized and rejected", "ARC", "", true},
		{"padded value normalized", "  BBOX  ", "bbox", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := map[string]string{"FACE_ALIGNMENT": tt.raw}
			getenv := func(k string) string { return m[k] }

			got, err := Alignment(getenv)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Alignment(%q) expected error, got nil (value %q)", tt.raw, got)
				}
				if got != "" {
					t.Errorf("Alignment(%q) on error = %q, want empty", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Alignment(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("Alignment(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestBurstWindow(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"empty defaults to 4s", "", 4 * time.Second},
		{"unparseable defaults to 4s", "abc", 4 * time.Second},
		{"below minimum clamps to 500ms", "100", 500 * time.Millisecond},
		{"above maximum clamps to 10s", "99999", 10 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := map[string]string{"TRIGGER_BURST_MS": tt.raw}
			getenv := func(k string) string { return m[k] }

			if got := BurstWindow(getenv); got != tt.want {
				t.Errorf("BurstWindow(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestSampleInterval(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"empty defaults to 200ms", "", 200 * time.Millisecond},
		{"unparseable defaults to 200ms", "abc", 200 * time.Millisecond},
		{"below minimum clamps to 50ms", "10", 50 * time.Millisecond},
		{"above maximum clamps to 1s", "5000", time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := map[string]string{"TRIGGER_SAMPLE_MS": tt.raw}
			getenv := func(k string) string { return m[k] }

			if got := SampleInterval(getenv); got != tt.want {
				t.Errorf("SampleInterval(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "with credentials", in: "rtsp://user:pass@cam:554/stream", want: "rtsp://redacted@cam:554/stream"},
		{name: "without credentials", in: "rtsp://cam:554/stream", want: "rtsp://cam:554/stream"},
		{name: "not a url", in: "not a url", want: "not a url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactURL(tt.in); got != tt.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A redacted URL must never contain the original password.
func TestRedactURLRemovesPassword(t *testing.T) {
	got := RedactURL("rtsp://admin:IzmirAlmere123!@192.168.178.23:554/h264Preview_01_sub")
	if strings.Contains(got, "IzmirAlmere123") {
		t.Fatalf("RedactURL leaked the password: %q", got)
	}
	if !strings.Contains(got, "192.168.178.23:554/h264Preview_01_sub") {
		t.Fatalf("RedactURL dropped the host/path: %q", got)
	}
}
