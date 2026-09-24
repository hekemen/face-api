package server

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// startCollector launches a background goroutine that continuously reads the
// RTSP stream, detects faces that don't match any enrolled user, and stores
// them as candidates. If a collector is already running, it stops it first.
func (s *FaceServer) startCollector(rtspURL string) error {
	s.collectMu.Lock()
	defer s.collectMu.Unlock()

	// Stop existing collector if running
	if s.collecting {
		s.stopCollectorLocked()
	}

	if rtspURL == "" {
		rtspURL = s.rtspURL
	}
	if rtspURL == "" {
		return fmt.Errorf("no RTSP URL configured for collector")
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.collectCancel = cancel
	s.collectDone = make(chan struct{})
	s.collecting = true
	s.collectStartedAt = time.Now()

	s.log.Info().Str("rtsp_url", rtspURL).Msg("collector started")
	go s.runCollectorLoop(ctx, rtspURL)
	return nil
}

// stopCollector signals the collector goroutine to stop.
func (s *FaceServer) stopCollector() {
	s.collectMu.Lock()
	if !s.collecting {
		s.collectMu.Unlock()
		return
	}
	s.stopCollectorLocked()
	s.collectStartedAt = time.Time{}
	s.collectMu.Unlock()
	s.log.Info().Msg("collector stopped")
}

// stopCollectorLocked stops the collector without acquiring the lock.
// Must be called with s.collectMu held.
func (s *FaceServer) stopCollectorLocked() {
	if s.collectCancel != nil {
		s.collectCancel()
		s.collectCancel = nil
	}
	if s.collectDone != nil {
		<-s.collectDone
		s.collectDone = nil
	}
	s.collecting = false
}

// IsCollectorRunning returns true if the collector is currently running.
func (s *FaceServer) IsCollectorRunning() bool {
	s.collectMu.Lock()
	defer s.collectMu.Unlock()
	return s.collecting
}

// CollectorStartedAt returns the time the collector was started (zero value if not running).
func (s *FaceServer) CollectorStartedAt() time.Time {
	s.collectMu.Lock()
	defer s.collectMu.Unlock()
	return s.collectStartedAt
}

// runCollectorLoop continuously reads RTSP frames, detects faces, and stores
// unknown faces as candidates. It also logs recognized faces to the audit trail.
// It reconnects on failure with exponential backoff.
func (s *FaceServer) runCollectorLoop(ctx context.Context, rtspURL string) {
	defer func() {
		s.collectMu.Lock()
		s.collecting = false
		s.collectMu.Unlock()
		close(s.collectDone)
	}()

	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second
	frameCount  := 0
	faceCount   := 0
	candidateCount := 0
	recognizedCount := 0

	for {
		// Throttle the loop so we don't hammer the Pi's CPU when nothing is happening.
		// 500ms is enough to catch people passing by without running ONNX on every single frame.
		time.Sleep(500 * time.Millisecond)

		select {
		case <-ctx.Done():
			s.log.Info().Int("frames", frameCount).Int("faces", faceCount).Int("candidates", candidateCount).Int("recognized", recognizedCount).Msg("collector stopped")
			return
		default:
		}

		img, err := readRTSPFrame(rtspURL, 3*time.Second)
		if err != nil {
			s.log.Error().Err(err).Str("rtsp_url", rtspURL).Msg("collector: failed to read RTSP frame")
			time.Sleep(backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		backoff = 1 * time.Second // reset on success

		cropped, _, err := s.detectAndCrop112(img)
		if err != nil {
			// No face detected, skip silently
			frameCount++
			continue
		}
		faceCount++

		queryVec, err := s.extractEmbedding(cropped)
		if err != nil {
			s.log.Error().Err(err).Msg("collector: failed to extract embedding")
			frameCount++
			continue
		}

		// Check against enrolled users — find best match
		s.mu.RLock()
		bestScore := float32(-1)
		bestName := ""
		for name, user := range s.dbMap {
			score := bestEmbeddingScore(queryVec, user.Embeddings)
			if score > bestScore {
				bestScore = score
				bestName = name
			}
		}
		s.mu.RUnlock()

		// If matched to an enrolled user, log to audit and skip
		if bestScore >= s.threshold {
			faceImage, imgErr := encodeFaceToBase64(cropped)
			if imgErr == nil {
				_ = s.storeAudit([]AuditEntry{{
					Time:       time.Now(),
					Endpoint:   "collector",
					Name:       bestName,
					Similarity: bestScore,
					Matched:    true,
					FaceImage:  faceImage,
				}})
			}
			recognizedCount++
			if recognizedCount%10 == 0 {
				s.log.Info().Str("user", bestName).Float32("similarity", bestScore).Msg("collector: recognized user")
			}
			frameCount++
			continue
		}

		// Check against existing candidates to avoid duplicates
		candidates, err := s.readCandidates()
		if err != nil {
			s.log.Error().Err(err).Msg("collector: failed to read candidates")
			frameCount++
			continue
		}

		isDuplicate := false
		for _, c := range candidates {
			if cosineSimilarity(queryVec, c.Embedding) >= s.threshold {
				isDuplicate = true
				break
			}
		}
		if isDuplicate {
			frameCount++
			continue
		}

		// Store as candidate
		faceImage, imgErr := encodeFaceToBase64(cropped)
		if imgErr != nil {
			s.log.Error().Err(imgErr).Msg("collector: failed to encode face image")
		}

		candidate := &Candidate{
			ID:        uuid.New().String(),
			Embedding: queryVec,
			FaceImage: faceImage,
			Time:      time.Now(),
			StreamURL: rtspURL,
		}

		if err := s.storeCandidate(candidate); err != nil {
			s.log.Error().Err(err).Msg("collector: failed to store candidate")
		} else {
			candidateCount++
			if candidateCount%10 == 0 {
				s.log.Info().Int("candidates", candidateCount).Msg("collector: new candidate stored")
			}
		}

		frameCount++
	}
}
