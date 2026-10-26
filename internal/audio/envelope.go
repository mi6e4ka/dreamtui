// Package audio decodes audio to a low-rate envelope used for alignment.
package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strings"
	"time"
)

const (
	// EnvelopeRate is the number of envelope samples per second.
	EnvelopeRate = 100
	// FrameSamples is 10 ms at 8 kHz.
	FrameSamples = 80
	envelopeSR   = 8000
)

// Envelope is a 100 Hz RMS envelope of the L-R side signal. Stereo music and
// effects survive, while centered dialogue is largely suppressed.
type Envelope struct {
	Values   []float64
	Duration float64
}

// FromFile decodes a local audio file into an envelope.
func FromFile(ctx context.Context, path string) (*Envelope, error) {
	return runFFmpegEnvelope(ctx, []string{
		"-nostdin", "-v", "error", "-i", path,
		"-vn", "-ac", "2", "-ar", fmt.Sprint(envelopeSR), "-f", "s16le", "-",
	})
}

// Reference decodes a short window of a remote media stream into an envelope.
// streamIndex is an absolute ffmpeg container stream index (for -map 0:N).
func Reference(ctx context.Context, mediaURL string, streamIndex int, start, duration float64) (*Envelope, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return runFFmpegEnvelope(ctx, []string{
		"-nostdin", "-v", "error",
		"-rw_timeout", "30000000",
		"-ss", fmt.Sprintf("%.3f", start),
		"-t", fmt.Sprintf("%.3f", duration),
		"-i", mediaURL,
		"-map", fmt.Sprintf("0:%d", streamIndex),
		"-vn", "-ac", "2", "-ar", fmt.Sprint(envelopeSR), "-f", "s16le", "-",
	})
}

func runFFmpegEnvelope(ctx context.Context, args []string) (*Envelope, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w", err)
	}
	env, readErr := envelopeFromReader(stdout)
	if readErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, readErr
	}
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ffmpeg: %s", msg)
	}
	if len(env.Values) == 0 {
		return nil, fmt.Errorf("ffmpeg не вернул аудиоданные")
	}
	return env, nil
}

func envelopeFromReader(r io.Reader) (*Envelope, error) {
	raw := make([]float64, 0, 2*60*EnvelopeRate)
	var pendingBytes []byte
	var frameSum float64
	var frameCount int
	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := buf[:n]
			if len(pendingBytes) > 0 {
				data = append(append([]byte(nil), pendingBytes...), data...)
				pendingBytes = nil
			}
			usable := len(data) / 4 * 4
			if usable < len(data) {
				pendingBytes = append(pendingBytes, data[usable:]...)
			}
			for i := 0; i < usable; i += 4 {
				l := float64(int16(binary.LittleEndian.Uint16(data[i:i+2]))) / 32768
				rr := float64(int16(binary.LittleEndian.Uint16(data[i+2:i+4]))) / 32768
				side := l - rr
				frameSum += side * side
				frameCount++
				if frameCount == FrameSamples {
					raw = append(raw, math.Sqrt(frameSum/float64(FrameSamples)))
					frameSum = 0
					frameCount = 0
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("чтение PCM: %w", err)
		}
	}
	// Discard a partial final frame; it is < 10 ms.
	smoothed := movingAverage(raw, 5)
	return &Envelope{
		Values:   smoothed,
		Duration: float64(len(smoothed)) / EnvelopeRate,
	}, nil
}

func movingAverage(x []float64, width int) []float64 {
	if width <= 1 || len(x) == 0 {
		return x
	}
	if width%2 == 0 {
		width++
	}
	out := make([]float64, len(x))
	half := width / 2
	for i := range x {
		lo := i - half
		if lo < 0 {
			lo = 0
		}
		hi := i + half + 1
		if hi > len(x) {
			hi = len(x)
		}
		var sum float64
		for _, v := range x[lo:hi] {
			sum += v
		}
		out[i] = sum / float64(hi-lo)
	}
	return out
}
