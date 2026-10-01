// Package audio captures microphone input and plays sound through the default
// devices. It is pure Go: on Windows it calls winmm.dll directly (no cgo).
package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

var ErrUnsupported = errors.New("audio: not supported on this platform")

const (
	formatPCM        = 1
	formatFloat      = 3
	formatExtensible = 0xFFFE
)

type Format struct {
	Tag           uint16 // 1 = PCM, 3 = IEEE float
	Channels      int
	SampleRate    int
	BitsPerSample int
}

// PCM16 is 16-bit signed little-endian PCM, the format speech models expect.
func PCM16(sampleRate, channels int) Format {
	return Format{Tag: formatPCM, Channels: channels, SampleRate: sampleRate, BitsPerSample: 16}
}

func (f Format) blockAlign() int     { return f.Channels * f.BitsPerSample / 8 }
func (f Format) bytesPerSecond() int { return f.SampleRate * f.blockAlign() }

// Duration is the playing time of pcm bytes in format f.
func (f Format) Duration(pcm []byte) time.Duration {
	if bps := f.bytesPerSecond(); bps > 0 {
		return time.Duration(len(pcm)) * time.Second / time.Duration(bps)
	}
	return 0
}

// EncodeWAV wraps raw PCM in a RIFF/WAVE container.
func EncodeWAV(f Format, pcm []byte) []byte {
	b := make([]byte, 44, 44+len(pcm))
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+len(pcm)))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], f.Tag)
	binary.LittleEndian.PutUint16(b[22:], uint16(f.Channels))
	binary.LittleEndian.PutUint32(b[24:], uint32(f.SampleRate))
	binary.LittleEndian.PutUint32(b[28:], uint32(f.bytesPerSecond()))
	binary.LittleEndian.PutUint16(b[32:], uint16(f.blockAlign()))
	binary.LittleEndian.PutUint16(b[34:], uint16(f.BitsPerSample))
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(len(pcm)))
	return append(b, pcm...)
}

// DecodeWAV returns the format and sample data of a WAV file. Streamed WAVs whose
// headers declare an unknown (oversized) data length are accepted.
func DecodeWAV(b []byte) (Format, []byte, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return Format{}, nil, errors.New("audio: not a WAV file")
	}
	var (
		f       Format
		haveFmt bool
	)
	for p := 12; p+8 <= len(b); {
		id, size := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:]))
		body := b[p+8:]
		if size < 0 || size > len(body) {
			size = len(body)
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return Format{}, nil, errors.New("audio: short fmt chunk")
			}
			f = Format{
				Tag:           binary.LittleEndian.Uint16(body[0:]),
				Channels:      int(binary.LittleEndian.Uint16(body[2:])),
				SampleRate:    int(binary.LittleEndian.Uint32(body[4:])),
				BitsPerSample: int(binary.LittleEndian.Uint16(body[14:])),
			}
			if f.Tag == formatExtensible && size >= 26 {
				f.Tag = binary.LittleEndian.Uint16(body[24:]) // first bytes of the SubFormat GUID
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return Format{}, nil, errors.New("audio: data chunk before fmt chunk")
			}
			if f.Tag != formatPCM && f.Tag != formatFloat {
				return Format{}, nil, fmt.Errorf("audio: unsupported WAV encoding %#x", f.Tag)
			}
			if f.Channels < 1 || f.SampleRate < 1 || f.BitsPerSample < 8 {
				return Format{}, nil, errors.New("audio: invalid WAV format")
			}
			pcm := body[:size]
			return f, pcm[:len(pcm)-len(pcm)%f.blockAlign()], nil
		}
		p += 8 + size + size%2
	}
	return Format{}, nil, errors.New("audio: WAV has no data chunk")
}

// level is the RMS of 16-bit PCM, in [0, 1].
func level(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
		sum += s * s
	}
	return math.Sqrt(sum / float64(n))
}

// Recording is an in-progress microphone capture (16-bit PCM).
type Recording struct {
	Format   Format
	start    time.Time
	lvl      atomic.Uint64
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
	pcm      []byte
	err      error
}

func newRecording(f Format) *Recording {
	return &Recording{Format: f, start: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
}

// Level is the loudness of the most recent audio, in [0, 1].
func (r *Recording) Level() float64 { return math.Float64frombits(r.lvl.Load()) }

func (r *Recording) Elapsed() time.Duration { return time.Since(r.start) }

// Stop ends the capture and returns everything recorded. Safe to call more than once.
func (r *Recording) Stop() ([]byte, error) {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
	return r.pcm, r.err
}
