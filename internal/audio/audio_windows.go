//go:build windows

package audio

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	waveMapper    = 0xFFFFFFFF
	callbackEvent = 0x00050000
	whdrDone      = 0x00000001
	pollMillis    = 20
	bufCount      = 8
	bufMillis     = 50
)

var (
	winmm = windows.NewLazySystemDLL("winmm.dll")

	procWaveInOpen             = winmm.NewProc("waveInOpen")
	procWaveInPrepareHeader    = winmm.NewProc("waveInPrepareHeader")
	procWaveInUnprepareHeader  = winmm.NewProc("waveInUnprepareHeader")
	procWaveInAddBuffer        = winmm.NewProc("waveInAddBuffer")
	procWaveInStart            = winmm.NewProc("waveInStart")
	procWaveInReset            = winmm.NewProc("waveInReset")
	procWaveInClose            = winmm.NewProc("waveInClose")
	procWaveOutOpen            = winmm.NewProc("waveOutOpen")
	procWaveOutPrepareHeader   = winmm.NewProc("waveOutPrepareHeader")
	procWaveOutUnprepareHeader = winmm.NewProc("waveOutUnprepareHeader")
	procWaveOutWrite           = winmm.NewProc("waveOutWrite")
	procWaveOutReset           = winmm.NewProc("waveOutReset")
	procWaveOutClose           = winmm.NewProc("waveOutClose")
)

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	Size           uint16
}

type waveHdr struct {
	Data          uintptr
	BufferLength  uint32
	BytesRecorded uint32
	User          uintptr
	Flags         uint32
	Loops         uint32
	Next          uintptr
	Reserved      uintptr
}

var hdrSize = unsafe.Sizeof(waveHdr{})

func (f Format) wave() *waveFormatEx {
	return &waveFormatEx{
		FormatTag:      f.Tag,
		Channels:       uint16(f.Channels),
		SamplesPerSec:  uint32(f.SampleRate),
		AvgBytesPerSec: uint32(f.bytesPerSecond()),
		BlockAlign:     uint16(f.blockAlign()),
		BitsPerSample:  uint16(f.BitsPerSample),
	}
}

func (h *waveHdr) done() bool { return atomic.LoadUint32(&h.Flags)&whdrDone != 0 }

func mmErr(op string, code uintptr) error {
	switch code {
	case 0:
		return nil
	case 2, 6: // MMSYSERR_BADDEVICEID, MMSYSERR_NODRIVER
		return fmt.Errorf("audio: %s: no audio device found", op)
	case 4: // MMSYSERR_ALLOCATED
		return fmt.Errorf("audio: %s: device is in use", op)
	case 32: // WAVERR_BADFORMAT
		return fmt.Errorf("audio: %s: format not supported by the device", op)
	}
	return fmt.Errorf("audio: %s: winmm error %d", op, code)
}

// Record starts capturing 16-bit PCM from the default microphone.
func Record(sampleRate, channels int) (*Recording, error) {
	f := PCM16(sampleRate, channels)
	ev, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("audio: create event: %w", err)
	}
	var h uintptr
	r, _, _ := procWaveInOpen.Call(uintptr(unsafe.Pointer(&h)), waveMapper, uintptr(unsafe.Pointer(f.wave())), uintptr(ev), 0, callbackEvent)
	if err := mmErr("open microphone", r); err != nil {
		windows.CloseHandle(ev)
		return nil, err
	}

	size := f.bytesPerSecond() * bufMillis / 1000
	bufs := make([][]byte, bufCount)
	hdrs := make([]*waveHdr, bufCount)
	rec := newRecording(f)
	cleanup := func(prepared int) {
		procWaveInReset.Call(h)
		for _, hd := range hdrs[:prepared] {
			procWaveInUnprepareHeader.Call(h, uintptr(unsafe.Pointer(hd)), hdrSize)
		}
		procWaveInClose.Call(h)
		windows.CloseHandle(ev)
		runtime.KeepAlive(bufs)
	}
	for i := range hdrs {
		bufs[i] = make([]byte, size)
		hdrs[i] = &waveHdr{Data: uintptr(unsafe.Pointer(&bufs[i][0])), BufferLength: uint32(size)}
		r, _, _ := procWaveInPrepareHeader.Call(h, uintptr(unsafe.Pointer(hdrs[i])), hdrSize)
		if err := mmErr("prepare buffer", r); err != nil {
			cleanup(i)
			return nil, err
		}
		r, _, _ = procWaveInAddBuffer.Call(h, uintptr(unsafe.Pointer(hdrs[i])), hdrSize)
		if err := mmErr("queue buffer", r); err != nil {
			cleanup(i + 1)
			return nil, err
		}
	}
	r, _, _ = procWaveInStart.Call(h)
	if err := mmErr("start recording", r); err != nil {
		cleanup(bufCount)
		return nil, err
	}

	go func() {
		defer close(rec.done)
		defer cleanup(bufCount)
		next, stopping := 0, false
		for {
			windows.WaitForSingleObject(ev, pollMillis)
			select {
			case <-rec.stop:
				if !stopping {
					stopping = true
					procWaveInReset.Call(h) // synchronously marks every queued buffer done
				}
			default:
			}
			for hd := hdrs[next]; hd.done(); hd = hdrs[next] {
				chunk := bufs[next][:atomic.LoadUint32(&hd.BytesRecorded)]
				rec.pcm = append(rec.pcm, chunk...)
				rec.lvl.Store(math.Float64bits(level(chunk)))
				atomic.StoreUint32(&hd.Flags, atomic.LoadUint32(&hd.Flags)&^whdrDone)
				if !stopping {
					r, _, _ := procWaveInAddBuffer.Call(h, uintptr(unsafe.Pointer(hd)), hdrSize)
					if err := mmErr("queue buffer", r); err != nil && rec.err == nil {
						rec.err = err
					}
				}
				next = (next + 1) % bufCount
			}
			if stopping {
				return
			}
		}
	}()
	return rec, nil
}

// Play plays PCM on the default output device, blocking until it finishes or ctx is done.
func Play(ctx context.Context, f Format, pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	ev, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return fmt.Errorf("audio: create event: %w", err)
	}
	defer windows.CloseHandle(ev)

	var h uintptr
	r, _, _ := procWaveOutOpen.Call(uintptr(unsafe.Pointer(&h)), waveMapper, uintptr(unsafe.Pointer(f.wave())), uintptr(ev), 0, callbackEvent)
	if err := mmErr("open speaker", r); err != nil {
		return err
	}
	defer procWaveOutClose.Call(h)

	hd := &waveHdr{Data: uintptr(unsafe.Pointer(&pcm[0])), BufferLength: uint32(len(pcm))}
	defer runtime.KeepAlive(pcm)
	r, _, _ = procWaveOutPrepareHeader.Call(h, uintptr(unsafe.Pointer(hd)), hdrSize)
	if err := mmErr("prepare buffer", r); err != nil {
		return err
	}
	defer procWaveOutUnprepareHeader.Call(h, uintptr(unsafe.Pointer(hd)), hdrSize)
	r, _, _ = procWaveOutWrite.Call(h, uintptr(unsafe.Pointer(hd)), hdrSize)
	if err := mmErr("play", r); err != nil {
		return err
	}
	for !hd.done() {
		select {
		case <-ctx.Done():
			procWaveOutReset.Call(h)
			return ctx.Err()
		default:
		}
		windows.WaitForSingleObject(ev, pollMillis)
	}
	return nil
}
