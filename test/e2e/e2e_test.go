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
	var usersRespBody map[string][]string
	if err := json.NewDecoder(usersResp.Body).Decode(&usersRespBody); err != nil {
		t.Fatalf("failed to decode users response: %v", err)
	}
	found := false
	for _, u := range usersRespBody["users"] {
		if u == "anthony" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'anthony' in users list, got: %v", usersRespBody["users"])
	}
	t.Logf("Users: %v", usersRespBody["users"])

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

	t.Log("=== PASS: Same person correctly recognized ===")
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
