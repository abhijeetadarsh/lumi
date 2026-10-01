package audio

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestWAVRoundTrip(t *testing.T) {
	f := PCM16(16000, 1)
	pcm := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	got, data, err := DecodeWAV(EncodeWAV(f, pcm))
	if err != nil {
		t.Fatal(err)
	}
	if got != f || !bytes.Equal(data, pcm) {
		t.Errorf("got %+v %v, want %+v %v", got, data, f, pcm)
	}
}

func TestDecodeStreamedWAVWithUnknownLength(t *testing.T) {
	wav := EncodeWAV(PCM16(24000, 1), []byte{1, 0, 2, 0, 3})
	binary.LittleEndian.PutUint32(wav[40:], 0xFFFFFFFF)
	_, data, err := DecodeWAV(wav)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 4 {
		t.Errorf("data length = %d, want 4 (trimmed to whole frames)", len(data))
	}
}

func TestDecodeRejectsNonWAV(t *testing.T) {
	if _, _, err := DecodeWAV([]byte("ID3 mp3 data")); err == nil {
		t.Error("want error")
	}
}

func TestDuration(t *testing.T) {
	if d := PCM16(16000, 1).Duration(make([]byte, 16000)); d != 500*time.Millisecond {
		t.Errorf("duration = %v", d)
	}
}

func TestLevel(t *testing.T) {
	pcm := make([]byte, 4)
	binary.LittleEndian.PutUint16(pcm, uint16(16384))
	binary.LittleEndian.PutUint16(pcm[2:], uint16(0xC000)) // -16384
	if l := level(pcm); l != 0.5 {
		t.Errorf("level = %v, want 0.5", l)
	}
}
