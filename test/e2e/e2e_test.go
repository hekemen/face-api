package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	testImage1 = "https://raw.githubusercontent.com/davidsandberg/facenet/master/data/images/Anthony_Hopkins_0001.jpg"
	testImage2 = "https://raw.githubusercontent.com/davidsandberg/facenet/master/data/images/Anthony_Hopkins_0002.jpg"
	testPort   = "8081/tcp"
)

var (
	baseURL       string
	testCtx       context.Context
	testContainer testcontainers.Container
)

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

func multipartBody(fieldName, filePath string) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile(fieldName, filepath.Base(filePath))
	data, _ := os.ReadFile(filePath)
	fw.Write(data)
	w.Close()
	return &buf, w.FormDataContentType()
}

func multipartBodyWithName(name, fieldName, filePath string) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormField("name")
	fw.Write([]byte(name))
	fw, _ = w.CreateFormFile(fieldName, filepath.Base(filePath))
	data, _ := os.ReadFile(filePath)
	fw.Write(data)
	w.Close()
	return &buf, w.FormDataContentType()
}

func TestMain(m *testing.M) {
	if err := downloadTestImage(testImage1, "test_hopkins_1.jpg"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to download test image 1: %v\n", err)
		os.Exit(1)
	}
	if err := downloadTestImage(testImage2, "test_hopkins_2.jpg"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to download test image 2: %v\n", err)
		os.Exit(1)
	}

	testCtx = context.Background()

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
			Tag:        "e2e",
		},
	}

	_, err = testcontainers.GenericContainer(testCtx, testcontainers.GenericContainerRequest{
		ContainerRequest: imgBuildReq,
		Started:          false,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build Docker image: %v\n", err)
		os.Exit(1)
	}

	imgName := "face-api-test:e2e"

	imgStartReq := testcontainers.ContainerRequest{
		Image:        imgName,
		WaitingFor:   wait.ForHTTP("/users").WithPort(testPort),
		ExposedPorts: []string{testPort},
	}

	testContainer, err = testcontainers.GenericContainer(testCtx, testcontainers.GenericContainerRequest{
		ContainerRequest: imgStartReq,
		Started:          true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start container: %v\n", err)
		os.Exit(1)
	}

	time.Sleep(2 * time.Second)

	host, err := testContainer.Host(testCtx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get container host: %v\n", err)
		os.Exit(1)
	}

	port, err := testContainer.MappedPort(testCtx, "8081/tcp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get container port: %v\n", err)
		os.Exit(1)
	}

	baseURL = fmt.Sprintf("http://%s:%s", host, port.Port())

	code := m.Run()

	if err := testContainer.Terminate(testCtx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to terminate container: %v\n", err)
	}

	os.Remove("test_hopkins_1.jpg")
	os.Remove("test_hopkins_2.jpg")
	os.Exit(code)
}

func TestEnrollAndRecognize_E2E(t *testing.T) {
	t.Log("=== Step 1: Enrolling Anthony Hopkins with image 1 ===")
	enrollBuf, enrollContentType := multipartBodyWithName("anthony", "image", "test_hopkins_1.jpg")
	enrollReq, err := http.NewRequest(http.MethodPost, baseURL+"/enroll", enrollBuf)
	if err != nil {
		t.Fatalf("failed to create enroll request: %v", err)
	}
	enrollReq.Header.Set("Content-Type", enrollContentType)
	resp, err := http.DefaultClient.Do(enrollReq)
	if err != nil {
		t.Fatalf("enroll request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d. Response: %s", resp.StatusCode, string(body))
	}
	t.Log("Enrollment successful")

	t.Log("=== Step 2: Listing enrolled users ===")
	usersResp, err := http.Get(baseURL + "/users")
	if err != nil {
		t.Fatalf("users request failed: %v", err)
	}
	defer usersResp.Body.Close()
	type e2eUser struct {
		Name      string   `json:"name"`
		Pictures  []string `json:"pictures"`
		UpdatedAt string   `json:"updated_at"`
	}
	var usersRespBody struct {
		Users []e2eUser `json:"users"`
	}
	if err := json.NewDecoder(usersResp.Body).Decode(&usersRespBody); err != nil {
		t.Fatalf("failed to decode users response: %v", err)
	}
	var anthony *e2eUser
	for i := range usersRespBody.Users {
		if usersRespBody.Users[i].Name == "anthony" {
			anthony = &usersRespBody.Users[i]
		}
	}
	if anthony == nil {
		t.Fatalf("expected 'anthony' in users list, got: %v", usersRespBody.Users)
	}
	if len(anthony.Pictures) == 0 {
		t.Fatalf("expected 'anthony' to have at least one enrolled picture, got %d", len(anthony.Pictures))
	}
	if anthony.UpdatedAt == "" {
		t.Fatal("expected 'anthony' to have an updated_at timestamp")
	}
	if _, err := time.Parse(time.RFC3339, anthony.UpdatedAt); err != nil {
		t.Fatalf("updated_at is not RFC3339: %q", anthony.UpdatedAt)
	}
	t.Logf("Users: %+v", usersRespBody.Users)

	t.Log("=== Step 3: Recognizing Anthony Hopkins with image 2 ===")
	recognizeBuf, recognizeContentType := multipartBody("image", "test_hopkins_2.jpg")
	recognizeReq, err := http.NewRequest(http.MethodPost, baseURL+"/recognize", recognizeBuf)
	if err != nil {
		t.Fatalf("failed to create recognize request: %v", err)
	}
	recognizeReq.Header.Set("Content-Type", recognizeContentType)
	resp2, err := http.DefaultClient.Do(recognizeReq)
	if err != nil {
		t.Fatalf("recognize request failed: %v", err)
	}
	defer resp2.Body.Close()

	var result struct {
		Matched    bool    `json:"matched"`
		Name       string  `json:"name"`
		Similarity float64 `json:"similarity"`
		DurationMs int64   `json:"duration_ms"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode recognition response: %v", err)
	}

	t.Logf("Recognition result: %+v", result)

	if !result.Matched {
		t.Errorf("expected match (matched=true), got matched=false with similarity=%.4f", result.Similarity)
	}
	if result.Name != "anthony" {
		t.Errorf("expected name='anthony', got name='%s'", result.Name)
	}
	if result.Similarity < 0.45 {
		t.Errorf("expected similarity >= 0.45, got %.4f", result.Similarity)
	}
	if result.DurationMs <= 0 {
		t.Errorf("expected duration_ms > 0, got %d", result.DurationMs)
	}

	t.Log("=== PASS: Same person correctly recognized ===")
}

func TestEnrollMultiplePictures_E2E(t *testing.T) {
	t.Log("=== Step 1: Enrolling 'multi' with image 1 ===")
	if code := enrollTo(baseURL, "multi", "test_hopkins_1.jpg"); code != http.StatusCreated {
		t.Fatalf("first enroll: expected 201, got %d", code)
	}

	t.Log("=== Step 2: Appending image 2 to 'multi' ===")
	if code := enrollTo(baseURL, "multi", "test_hopkins_2.jpg"); code != http.StatusCreated {
		t.Fatalf("second enroll: expected 201, got %d", code)
	}

	t.Log("=== Step 3: Appending image 1 again (3 pictures total) ===")
	if code := enrollTo(baseURL, "multi", "test_hopkins_1.jpg"); code != http.StatusCreated {
		t.Fatalf("third enroll: expected 201, got %d", code)
	}

	t.Log("=== Step 4: Fourth enroll must be rejected (max 3) ===")
	if code := enrollTo(baseURL, "multi", "test_hopkins_2.jpg"); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for 4th picture, got %d", code)
	}

	t.Log("=== Step 4b: /users reports all 3 pictures for 'multi' ===")
	usersResp2, err := http.Get(baseURL + "/users")
	if err != nil {
		t.Fatalf("users request failed: %v", err)
	}
	defer usersResp2.Body.Close()
	var usersResp2Body struct {
		Users []struct {
			Name     string   `json:"name"`
			Pictures []string `json:"pictures"`
		} `json:"users"`
	}
	if err := json.NewDecoder(usersResp2.Body).Decode(&usersResp2Body); err != nil {
		t.Fatalf("decode users response: %v", err)
	}
	for _, u := range usersResp2Body.Users {
		if u.Name == "multi" {
			if len(u.Pictures) != 3 {
				t.Fatalf("expected 'multi' to have 3 pictures via /users, got %d", len(u.Pictures))
			}
			break
		}
	}

	t.Log("=== Step 5: Recognizing with image 2 (multi now has 3 embeddings) ===")
	recognizeBuf, recognizeContentType := multipartBody("image", "test_hopkins_2.jpg")
	recognizeReq, err := http.NewRequest(http.MethodPost, baseURL+"/recognize", recognizeBuf)
	if err != nil {
		t.Fatalf("failed to create recognize request: %v", err)
	}
	recognizeReq.Header.Set("Content-Type", recognizeContentType)
	resp, err := http.DefaultClient.Do(recognizeReq)
	if err != nil {
		t.Fatalf("recognize request failed: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Matched    bool  `json:"matched"`
		DurationMs int64 `json:"duration_ms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode recognition response: %v", err)
	}
	if !result.Matched {
		t.Errorf("expected match with multi-embedding user, got matched=false")
	}
	if result.DurationMs <= 0 {
		t.Errorf("expected duration_ms > 0, got %d", result.DurationMs)
	}
	t.Log("=== PASS: multi-picture enroll + 3 max cap verified ===")
}

func enrollTo(bURL, name, imagePath string) int {
	buf, contentType := multipartBodyWithName(name, "image", imagePath)
	req, err := http.NewRequest(http.MethodPost, bURL+"/enroll", buf)
	if err != nil {
		return -1
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestEnrollMissingName_E2E(t *testing.T) {
	t.Log("using shared container at", baseURL)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("image", "test.jpg")
	fw.Write([]byte("fake"))
	w.Close()

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/enroll", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing name, got %d", resp.StatusCode)
	}
}

func containerLogs(t *testing.T) string {
	t.Helper()
	rc, err := testContainer.Logs(testCtx)
	if err != nil {
		t.Fatalf("failed to get container logs: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("failed to read container logs: %v", err)
	}
	return string(data)
}

// TestNoFaceNotAudited_E2E verifies a no-face scan (recognition with no
// detected face) does not produce an audit entry.
func TestNoFaceNotAudited_E2E(t *testing.T) {
	t.Log("=== Step 1: reading audit count ===")
	countBefore := auditCount(t)

	t.Log("=== Step 2: recognizing with a solid-color (no-face) image ===")
	solidJPG := []byte{
		0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x00, 0x01,
		0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xff, 0xdb, 0x00, 0x43,
		0x00, 0x08, 0x06, 0x06, 0x07, 0x06, 0x05, 0x08, 0x07, 0x07, 0x07, 0x09,
		0x09, 0x08, 0x0a, 0x0c, 0x14, 0x0d, 0x0c, 0x0b, 0x0b, 0x0c, 0x19, 0x12,
		0x13, 0x0f, 0x14, 0x1d, 0x1a, 0x1f, 0x1e, 0x1d, 0x1a, 0x1c, 0x1c, 0x20,
		0x24, 0x2e, 0x27, 0x20, 0x22, 0x2c, 0x23, 0x1c, 0x1c, 0x28, 0x37, 0x29,
		0x2c, 0x30, 0x31, 0x34, 0x34, 0x34, 0x1f, 0x27, 0x39, 0x3d, 0x38, 0x32,
		0x3c, 0x2e, 0x33, 0x34, 0x32, 0xff, 0xc0, 0x00, 0x0b, 0x08, 0x00, 0x01,
		0x00, 0x01, 0x01, 0x01, 0x11, 0x00, 0xff, 0xc4, 0x00, 0x1f, 0x00, 0x00,
		0x01, 0x05, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0xff, 0xda, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3f,
		0x00, 0xfb, 0xd5, 0xdb, 0xc0, 0xff, 0xd9,
	}

	tmpFile := "test_solid_noaudit.jpg"
	if err := os.WriteFile(tmpFile, solidJPG, 0644); err != nil {
		t.Fatalf("failed to write solid jpg: %v", err)
	}
	defer os.Remove(tmpFile)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("image", "solid.jpg")
	fw.Write(solidJPG)
	w.Close()

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/recognize", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for no face detected, got %d", resp.StatusCode)
	}

	t.Log("=== Step 3: verifying audit count is unchanged ===")
	countAfter := auditCount(t)
	if countAfter != countBefore {
		t.Errorf("no-face recognize must not create audit entries: count before=%d after=%d", countBefore, countAfter)
	}
	t.Log("=== PASS: no-face scan produces no audit entry ===")
}

func auditCount(t *testing.T) int {
	t.Helper()
	resp, err := http.Get(baseURL + "/audit")
	if err != nil {
		t.Fatalf("audit request failed: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("failed to decode /audit response: %v", err)
	}
	return out.Count
}

// TestProbeEndpointsNotLogged_E2E verifies /healthz and /readyz requests are
// skipped by the request-logging middleware (probes would otherwise pollute
// the log stream), while a regular endpoint is still logged.
func TestProbeEndpointsNotLogged_E2E(t *testing.T) {
	t.Log("=== Triggering probe + regular requests ===")

	get := func(path string) {
		t.Helper()
		resp, err := http.Get(baseURL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		resp.Body.Close()
	}
	get("/healthz")
	get("/readyz")
	get("/users")

	logs := containerLogs(t)

	var probeRequests int
	var usersLogged bool
	for _, line := range strings.Split(logs, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["message"] != "request" {
			continue
		}
		switch entry["path"] {
		case "/healthz", "/readyz":
			probeRequests++
		case "/users":
			usersLogged = true
		}
	}
	if probeRequests > 0 {
		t.Errorf("expected no request-log lines for /healthz or /readyz, found %d:\n%s", probeRequests, logs)
	}
	if !usersLogged {
		t.Errorf("expected a request-log line for GET /users:\n%s", logs)
	}
	t.Log("=== PASS: probes skipped, regular requests still logged ===")
}

// TestRequestLogging_E2E verifies every HTTP request is logged to the
// container console as a JSON zerolog line carrying method, path, status,
// and duration.
func TestRequestLogging_E2E(t *testing.T) {
	t.Log("=== Triggering requests and inspecting container logs ===")

	resp, err := http.Get(baseURL + "/users")
	if err != nil {
		t.Fatalf("users request failed: %v", err)
	}
	resp.Body.Close()

	logs := containerLogs(t)
	var found bool
	for _, line := range strings.Split(logs, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["method"] == "GET" && entry["path"] == "/users" {
			found = true
			if entry["status"] != float64(http.StatusOK) {
				t.Errorf("request log for GET /users: expected status 200, got %v", entry["status"])
			}
			if _, ok := entry["duration_ms"]; !ok {
				t.Errorf("request log for GET /users: missing duration_ms field: %v", entry)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected a JSON request log line for GET /users, got logs:\n%s", logs)
	}
	t.Log("=== PASS: requests logged as JSON with method/path/status/duration ===")
}

// TestAuditEndpoint_E2E verifies face scans (enroll + recognize) are persisted
// to the audit log with a timestamp and returned newest-first by GET /audit.
func TestAuditEndpoint_E2E(t *testing.T) {
	t.Log("=== Step 1: Enrolling a unique user to produce an audit entry ===")
	name := fmt.Sprintf("audit_%d", time.Now().UnixNano())
	if code := enrollTo(baseURL, name, "test_hopkins_1.jpg"); code != http.StatusCreated {
		t.Fatalf("enroll %s: expected 201, got %d", name, code)
	}

	t.Log("=== Step 2: Recognizing to produce a recognize audit entry ===")
	recognizeBuf, recognizeContentType := multipartBody("image", "test_hopkins_2.jpg")
	recognizeReq, _ := http.NewRequest(http.MethodPost, baseURL+"/recognize", recognizeBuf)
	recognizeReq.Header.Set("Content-Type", recognizeContentType)
	recResp, err := http.DefaultClient.Do(recognizeReq)
	if err != nil {
		t.Fatalf("recognize request failed: %v", err)
	}
	recResp.Body.Close()

	t.Log("=== Step 3: Reading /audit ===")
	auditResp, err := http.Get(baseURL + "/audit")
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
			Time       string  `json:"time"`
			Endpoint   string  `json:"endpoint"`
			Name       string  `json:"name"`
			Similarity float32 `json:"similarity"`
			Matched    bool    `json:"matched"`
			DurationMs int64   `json:"duration_ms"`
			FaceImage  string  `json:"face_image"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(auditResp.Body).Decode(&out); err != nil {
		t.Fatalf("failed to decode /audit response: %v", err)
	}
	if len(out.Entries) == 0 {
		t.Fatalf("expected non-empty audit entries, got %+v", out)
	}

	var enrollFound, recognizeFound bool
	for i, e := range out.Entries {
		if ts, err := time.Parse(time.RFC3339Nano, e.Time); err != nil || ts.IsZero() {
			t.Errorf("entry[%d] (%s): invalid time %q: %v", i, e.Endpoint, e.Time, err)
		}
		switch e.Endpoint {
		case "recognize":
			recognizeFound = true
		case "enroll":
			enrollFound = enrollFound || e.Name == name
		}
	}
	if !enrollFound {
		t.Errorf("expected an enroll audit entry for %q in: %+v", name, out.Entries)
	}
	if !recognizeFound {
		t.Errorf("expected a recognize audit entry in: %+v", out.Entries)
	}

	// Check that each entry with a matched face has a non-empty face_image
	for _, entry := range out.Entries {
		if entry.Matched && entry.FaceImage == "" {
			t.Errorf("matched audit entry for %q has empty face_image", entry.Name)
		}
	}

	parsedTimes := make([]time.Time, len(out.Entries))
	for i, e := range out.Entries {
		parsedTimes[i], _ = time.Parse(time.RFC3339Nano, e.Time)
	}
	if len(parsedTimes) > 1 {
		for i := 1; i < len(parsedTimes); i++ {
			if parsedTimes[i].After(parsedTimes[i-1]) {
				t.Errorf("audit entries not newest-first: entry %d time %s after entry %d time %s",
					i-1, parsedTimes[i-1], i, parsedTimes[i])
				break
			}
		}
	}
	t.Log("=== PASS: face scans persisted to audit log with timestamps ===")
}

// TestNoGPUWarning_E2E guards against the benign ONNX Runtime GPU device
// discovery warning being re-introduced into the container logs.
func TestNoGPUWarning_E2E(t *testing.T) {
	logs := containerLogs(t)
	if strings.Contains(logs, "GPU device discovery failed") {
		t.Errorf("ONNX GPU device discovery warning present in container logs:\n%s", logs)
	}
}

func TestRecognizeNoFace_E2E(t *testing.T) {
	t.Log("using shared container at", baseURL)

	solidJPG := []byte{
		0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x00, 0x01,
		0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xff, 0xdb, 0x00, 0x43,
		0x00, 0x08, 0x06, 0x06, 0x07, 0x06, 0x05, 0x08, 0x07, 0x07, 0x07, 0x09,
		0x09, 0x08, 0x0a, 0x0c, 0x14, 0x0d, 0x0c, 0x0b, 0x0b, 0x0c, 0x19, 0x12,
		0x13, 0x0f, 0x14, 0x1d, 0x1a, 0x1f, 0x1e, 0x1d, 0x1a, 0x1c, 0x1c, 0x20,
		0x24, 0x2e, 0x27, 0x20, 0x22, 0x2c, 0x23, 0x1c, 0x1c, 0x28, 0x37, 0x29,
		0x2c, 0x30, 0x31, 0x34, 0x34, 0x34, 0x1f, 0x27, 0x39, 0x3d, 0x38, 0x32,
		0x3c, 0x2e, 0x33, 0x34, 0x32, 0xff, 0xc0, 0x00, 0x0b, 0x08, 0x00, 0x01,
		0x00, 0x01, 0x01, 0x01, 0x11, 0x00, 0xff, 0xc4, 0x00, 0x1f, 0x00, 0x00,
		0x01, 0x05, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0xff, 0xda, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3f,
		0x00, 0xfb, 0xd5, 0xdb, 0xc0, 0xff, 0xd9,
	}

	tmpFile := "test_solid_temp.jpg"
	if err := os.WriteFile(tmpFile, solidJPG, 0644); err != nil {
		t.Fatalf("failed to write solid jpg: %v", err)
	}
	defer os.Remove(tmpFile)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("image", "solid.jpg")
	fw.Write(solidJPG)
	w.Close()

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/recognize", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 400 for no face detected, got %d. Response: %s", resp.StatusCode, string(body))
	}
}
