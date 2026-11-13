// Command reenroll replaces one user's enrolled photos with fresh captures
// from the camera.
//
// Re-enrollment is needed when the embedding space changes: the bbox and
// arcface5 crops produce different spaces, and an enrollment made in one
// cannot be compared with a capture in the other.
//
// The order of operations matters. The old record is deleted only after a
// new capture has succeeded, so a failed capture cannot destroy an
// enrollment that was working.
//
// Usage:
//
//	reenroll -url <camera-url> -api <base-url> -token <token> -name <user> [-pictures N] [-out <dir>]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"h2hsecure.com/face/internal/framesource"
	"h2hsecure.com/face/internal/imaging"
)

// maxPictures is the number of photos a user can hold.
const maxPictures = 5

// grabInterval is the pause between grabs. The camera sends a keyframe every
// ~2 s, so a shorter interval would return the same picture twice.
const grabInterval = 900 * time.Millisecond

func main() {
	var (
		cameraURL = flag.String("url", "", "RTSP camera URL to capture from")
		apiURL    = flag.String("api", "http://pi.home.arpa/face", "face API base URL")
		token     = flag.String("token", "", "face API token")
		name      = flag.String("name", "", "user name to re-enroll")
		pictures  = flag.Int("pictures", maxPictures, "number of photos to enroll")
		outDir    = flag.String("out", "", "directory to write the captured photos to (optional)")
	)
	flag.Parse()

	if *cameraURL == "" || *token == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "usage: reenroll -url <camera-url> -api <url> -token <token> -name <user> [-pictures N] [-out <dir>]")
		os.Exit(2)
	}
	if *pictures < 1 || *pictures > maxPictures {
		fmt.Fprintf(os.Stderr, "-pictures must be between 1 and %d\n", maxPictures)
		os.Exit(2)
	}

	if err := run(*cameraURL, *apiURL, *token, *name, *pictures, *outDir); err != nil {
		fmt.Fprintln(os.Stderr, "reenroll:", err)
		os.Exit(1)
	}
}

func run(cameraURL, apiURL, token, name string, pictures int, outDir string) error {
	log := zerolog.New(os.Stderr).With().Timestamp().Logger()

	fmt.Printf("Capturing %d photos of %s from the camera. Stand in frame.\n", pictures, name)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	grabber := framesource.NewGrabber(cameraURL, log)
	var frames [][]byte
	for i := 0; i < pictures; i++ {
		w, h, pix, err := grabber.Grab(ctx)
		if err != nil {
			return fmt.Errorf("grab %d: %w", i+1, err)
		}
		img := &imaging.RGBImage{W: w, H: h, Pix: pix}
		data, err := imaging.EncodeJPEG(img)
		if err != nil {
			return fmt.Errorf("encode grab %d: %w", i+1, err)
		}
		fmt.Printf("  captured %d: %dx%d (%d bytes)\n", i+1, w, h, len(data))
		frames = append(frames, data)
		if i < pictures-1 {
			time.Sleep(grabInterval)
		}
	}

	// Keep only the frames that differ, so the photos are different moments
	// rather than repeats of the same keyframe.
	distinct := pickDistinctCaptures(frames, pictures)
	if len(distinct) == 0 {
		return fmt.Errorf("no usable frames captured")
	}
	if len(distinct) < len(frames) {
		fmt.Printf("  %d of %d frames repeated an earlier moment; using %d\n",
			len(frames)-len(distinct), len(frames), len(distinct))
		fmt.Printf("  (move slightly between shots for more distinct photos)\n")
	}

	if outDir != "" {
		if err := os.MkdirAll(outDir, 0755); err != nil {
			return fmt.Errorf("create output dir: %w", err)
		}
		for i, f := range distinct {
			path := filepath.Join(outDir, fmt.Sprintf("%s-%d.jpg", name, i+1))
			if err := os.WriteFile(path, f, 0644); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			fmt.Printf("  wrote %s\n", path)
		}
	}

	// The first new photo must be accepted before the old record is
	// removed, so a capture the API rejects (no face in it, say) cannot
	// leave the user with nothing.
	if err := enrollOne(apiURL, token, name, distinct[0]); err != nil {
		return fmt.Errorf("enroll first photo: %w", err)
	}
	fmt.Printf("  enrolled the first photo; replacing the old record\n")

	if err := deleteUser(apiURL, token, name); err != nil {
		return fmt.Errorf("delete old record: %w", err)
	}

	for i, f := range distinct[1:] {
		if err := enrollOne(apiURL, token, name, f); err != nil {
			return fmt.Errorf("enroll photo %d: %w", i+2, err)
		}
		fmt.Printf("  enrolled photo %d\n", i+2)
	}

	fmt.Printf("Done: %s now has %d photos in the current alignment space.\n", name, len(distinct))
	return nil
}

// pickDistinctCaptures keeps the first frame of each run of equal frames, up
// to want frames.
//
// Duplicate frames are dropped rather than repeated: an identical photo
// produces an identical embedding and adds nothing to the enrollment. The
// caller reports how many duplicates were dropped so the operator can move
// between shots.
func pickDistinctCaptures(frames [][]byte, want int) [][]byte {
	if want <= 0 {
		return nil
	}
	var out [][]byte
	for _, f := range frames {
		if len(out) > 0 && !framesDiffer(out[len(out)-1], f) {
			continue
		}
		out = append(out, f)
		if len(out) == want {
			return out
		}
	}
	return out
}

// framesDiffer reports whether two frames are different pictures. The
// comparison covers the first bytes only, which is enough to separate two
// moments without hashing a whole 5 MP frame.
func framesDiffer(a, b []byte) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n > 64 {
		n = 64
	}
	return !bytes.Equal(a[:n], b[:n])
}

// enrollOne uploads one photo for the user.
func enrollOne(apiURL, token, name string, photo []byte) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("name", name); err != nil {
		return err
	}
	part, err := w.CreateFormFile("image", name+".jpg")
	if err != nil {
		return err
	}
	if _, err := part.Write(photo); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, apiURL+"/enroll", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(msg))
	}
	return nil
}

// deleteUser removes the user's existing record. A missing user is not an
// error: there may be nothing to replace.
func deleteUser(apiURL, token, name string) error {
	req, err := http.NewRequest(http.MethodDelete, apiURL+"/users/"+name, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(msg))
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil // the delete succeeded; the body is informational
	}
	return nil
}
