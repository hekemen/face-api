package rtsp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	rtspTestImage = "https://raw.githubusercontent.com/davidsandberg/facenet/master/data/images/Anthony_Hopkins_0001.jpg"
	rtspURL       = "rtsp://127.0.0.1:18554/face"
)

var (
	rtspBaseURL   string
	rtspCtx       context.Context
	rtspKontainer testcontainers.Container
	rtspMock      *mockRTSP
)

func streamCheck(rtspURL string) (StreamCheckResp, int, string, error) {
	form := url.Values{}
	if rtspURL != "" {
		form.Set("rtsp_url", rtspURL)
	}

	resp, err := http.PostForm(rtspBaseURL+"/stream-check", form)
	if err != nil {
		return StreamCheckResp{}, 0, "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return StreamCheckResp{}, 0, "", err
	}

	var result StreamCheckResp
	if err := json.Unmarshal(body, &result); err != nil {
		return StreamCheckResp{}, 0, "", fmt.Errorf("decode %q: %w", string(body), err)
	}
	return result, resp.StatusCode, string(body), nil
}

type StreamCheckResp struct {
	Status     string  `json:"status"`
	Name       string  `json:"name,omitempty"`
	Similarity float64 `json:"similarity,omitempty"`
	Reason     string  `json:"reason,omitempty"`
	DurationMs int64   `json:"duration_ms"`
}

func enroll(name, imagePath string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormField("name")
	fw.Write([]byte(name))
	fileBytes, err := os.ReadFile(imagePath)
	if err != nil {
		return err
	}
	fw, err = w.CreateFormFile("image", filepath.Base(imagePath))
	if err != nil {
		return err
	}
	fw.Write(fileBytes)
	w.Close()

	req, err := http.NewRequest(http.MethodPost, rtspBaseURL+"/enroll", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("enroll %s: HTTP %d: %s", name, resp.StatusCode, string(body))
	}
	return nil
}

func downloadTestImage(url, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0644)
}

func waitForUsers(baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/users")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("server at %s did not become ready within %s", baseURL, timeout)
}

func TestMain(m *testing.M) {
	if err := downloadTestImage(rtspTestImage, "test_rtsp_hopkins_1.jpg"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to download rtsp test image: %v\n", err)
		os.Exit(1)
	}

	rtspCtx = context.Background()

	// hosting the RTSP server in-process; face-api container must share the
	// host network namespace to reach it via 127.0.0.1.
	rtspMock = &mockRTSP{}
	if err := rtspMock.start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start mock rtsp server: %v\n", err)
		os.Exit(1)
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get cwd: %v\n", err)
		os.Exit(1)
	}
	projectRoot := filepath.Dir(filepath.Dir(cwd))

	imgBuildReq := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    projectRoot,
			Dockerfile: "Dockerfile",
			KeepImage:  true,
			Repo:       "face-api-test",
			Tag:        "rtsp",
		},
	}

	_, err = testcontainers.GenericContainer(rtspCtx, testcontainers.GenericContainerRequest{
		ContainerRequest: imgBuildReq,
		Started:          false,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build Docker image: %v\n", err)
		os.Exit(1)
	}

	imgStartReq := testcontainers.ContainerRequest{
		Image:       "face-api-test:rtsp",
		NetworkMode: "host",
		Env: map[string]string{
			"RTSP_URL": rtspURL,
		},
		WaitingFor: wait.ForLog("Face API Server running"),
	}

	rtspKontainer, err = testcontainers.GenericContainer(rtspCtx, testcontainers.GenericContainerRequest{
		ContainerRequest: imgStartReq,
		Started:          true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start container: %v\n", err)
		os.Exit(1)
	}

	rtspBaseURL = "http://127.0.0.1:8081"
	if err := waitForUsers(rtspBaseURL, 30*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := rtspKontainer.Terminate(rtspCtx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to terminate container: %v\n", err)
	}
	rtspMock.stop()

	os.Remove("test_rtsp_hopkins_1.jpg")
	os.Exit(code)
}

func TestStreamCheckMatched_E2E(t *testing.T) {
	t.Log("=== Step 1: Enrolling Anthony Hopkins ===")
	if err := enroll("anthony", "test_rtsp_hopkins_1.jpg"); err != nil {
		t.Fatalf("enroll failed: %v", err)
	}

	t.Log("=== Step 2: Streaming a face via mock RTSP ===")
	raw, err := os.ReadFile("test_rtsp_hopkins_1.jpg")
	if err != nil {
		t.Fatalf("read test image: %v", err)
	}
	frame, err := padJPEGToSide(raw, 256)
	if err != nil {
		t.Fatalf("prepare stream frame: %v", err)
	}
	rtspMock.setFrame(frame)

	t.Log("=== Step 3: Checking the RTSP stream ===")
	result, status, body, err := streamCheck(rtspURL)
	if err != nil {
		t.Fatalf("stream-check failed: %v", err)
	}
	t.Logf("HTTP %d: %+v", status, result)

	if result.Status != "ok" {
		t.Errorf("expected status='ok', got %q (resp: %s)", result.Status, body)
	}
	if result.Name != "anthony" {
		t.Errorf("expected name='anthony', got %q", result.Name)
	}
	if result.Similarity < 0.45 {
		t.Errorf("expected similarity >= 0.45, got %.4f", result.Similarity)
	}
	if result.DurationMs <= 0 {
		t.Errorf("expected duration_ms > 0, got %d", result.DurationMs)
	}
	t.Log("=== PASS: matched face recognized from mock RTSP stream ===")
}

func TestStreamCheckConfigFallback_E2E(t *testing.T) {
	t.Log("=== Config fallback: request omits rtsp_url, uses RTSP_URL from config ===")
	if err := enroll("anthony", "test_rtsp_hopkins_1.jpg"); err != nil {
		t.Fatalf("enroll failed: %v", err)
	}

	raw, err := os.ReadFile("test_rtsp_hopkins_1.jpg")
	if err != nil {
		t.Fatalf("read test image: %v", err)
	}
	frame, err := padJPEGToSide(raw, 256)
	if err != nil {
		t.Fatalf("prepare stream frame: %v", err)
	}
	rtspMock.setFrame(frame)

	result, status, body, err := streamCheck("")
	if err != nil {
		t.Fatalf("stream-check failed: %v", err)
	}
	t.Logf("HTTP %d: %+v", status, result)

	if result.Status != "ok" {
		t.Errorf("expected status='ok', got %q (resp: %s)", result.Status, body)
	}
	if result.Name != "anthony" {
		t.Errorf("expected name='anthony', got %q", result.Name)
	}
	if result.Similarity < 0.45 {
		t.Errorf("expected similarity >= 0.45, got %.4f", result.Similarity)
	}
	if result.DurationMs <= 0 {
		t.Errorf("expected duration_ms > 0, got %d", result.DurationMs)
	}
	t.Log("=== PASS: RTSP url taken from config when request omits rtsp_url ===")
}

func TestStreamCheckNoFace_E2E(t *testing.T) {
	t.Log("=== Step 1: Streaming a solid-color (no-face) frame ===")
	solid, err := solidJPEG(640, 480)
	if err != nil {
		t.Fatalf("build solid frame: %v", err)
	}
	rtspMock.setFrame(solid)

	t.Log("=== Step 2: Checking the RTSP stream (expects timeout) ===")
	start := time.Now()
	result, status, body, err := streamCheck(rtspURL)
	if err != nil {
		t.Fatalf("stream-check failed: %v", err)
	}
	t.Logf("HTTP %d: %+v (took %s)", status, result, time.Since(start))

	if result.Status != "not ok" {
		t.Errorf("expected status='not ok', got %q (resp: %s)", result.Status, body)
	}
	if result.Reason != "No face detected within 3 seconds" {
		t.Errorf("expected reason='No face detected within 3 seconds', got %q", result.Reason)
	}
	t.Log("=== PASS: no face in stream correctly reported ===")
}

func TestStreamCheckAudit_E2E(t *testing.T) {
	t.Log("=== Step 1: Enrolling a unique user for stream-check audit ===")
	name := fmt.Sprintf("audit_%d", time.Now().UnixNano())
	if err := enroll(name, "test_rtsp_hopkins_1.jpg"); err != nil {
		t.Fatalf("enroll failed: %v", err)
	}

	t.Log("=== Step 2: Streaming the matching face via mock RTSP ===")
	raw, err := os.ReadFile("test_rtsp_hopkins_1.jpg")
	if err != nil {
		t.Fatalf("read test image: %v", err)
	}
	frame, err := padJPEGToSide(raw, 256)
	if err != nil {
		t.Fatalf("prepare stream frame: %v", err)
	}
	rtspMock.setFrame(frame)

	result, status, body, err := streamCheck(rtspURL)
	if err != nil {
		t.Fatalf("stream-check failed: %v", err)
	} else if status != http.StatusOK {
		t.Fatalf("stream-check: expected 200, got %d (%s)", status, body)
	} else if result.Name == "" {
		t.Fatalf("stream-check: expected a matched name, got %+v", result)
	}

	t.Log("=== Step 3: Verifying stream-check scans are in the audit log ===")
	auditResp, err := http.Get(rtspBaseURL + "/audit")
	if err != nil {
		t.Fatalf("audit request failed: %v", err)
	}
	defer auditResp.Body.Close()
	if auditResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(auditResp.Body)
		t.Fatalf("expected 200 from /audit, got %d: %s", auditResp.StatusCode, string(body))
	}

	var out struct {
		Entries []struct {
			Time     string `json:"time"`
			Endpoint string `json:"endpoint"`
			Name     string `json:"name"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(auditResp.Body).Decode(&out); err != nil {
		t.Fatalf("failed to decode /audit response: %v", err)
	}

	var streamCheckFound bool
	for _, e := range out.Entries {
		if e.Endpoint != "stream-check" {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, e.Time); err != nil {
			t.Errorf("stream-check audit entry has invalid time %q: %v", e.Time, err)
			continue
		}
		if e.Name != "" && e.Name == result.Name {
			streamCheckFound = true
		}
	}
	if !streamCheckFound {
		t.Errorf("expected a stream-check audit entry for %q with timestamp in: %+v", result.Name, out.Entries)
	}
	t.Log("=== PASS: stream-check face scans recorded in audit log ===")
}

func TestStreamCheckUnreachable_E2E(t *testing.T) {
	t.Log("=== Checking against an unreachable RTSP endpoint ===")
	result, _, body, err := streamCheck("rtsp://127.0.0.1:9/nope")
	if err != nil {
		t.Fatalf("stream-check failed: %v", err)
	}
	t.Logf("result: %+v", result)

	if result.Status != "not ok" {
		t.Errorf("expected status='not ok', got %q (resp: %s)", result.Status, body)
	}
	if result.Reason != "Failed to connect to RTSP stream" {
		t.Errorf("expected reason='Failed to connect to RTSP stream', got %q", result.Reason)
	}
	t.Log("=== PASS: unreachable stream correctly reported ===")
}
