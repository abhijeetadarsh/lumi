//go:build windows

package systemtts

import (
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"

	"github.com/abhijeetadarsh/lumi/internal/audio"
)

const (
	sampleRate         = 22050
	saft22kHz16BitMono = 22 // SpeechAudioFormatType
	svsfIsNotXML       = 16 // SpeechVoiceSpeakFlags: speak "<" literally
)

// serve owns the COM apartment: SAPI objects must stay on the thread that created them.
func serve(reqs <-chan request) {
	runtime.LockOSThread()
	e := &engine{}
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		e.initErr = fmt.Errorf("system voice: init COM: %w", err)
	}
	for r := range reqs {
		wav, err := e.synthesize(r.req.Voice, r.req.Text, r.req.Params)
		r.resp <- result{wav, err}
	}
}

type engine struct {
	initErr error
	voice   *ole.IDispatch
	current string // voice name requested last
}

func (e *engine) synthesize(voice, text string, params map[string]any) ([]byte, error) {
	if e.initErr != nil {
		return nil, e.initErr
	}
	if e.voice == nil {
		v, err := createDispatch("SAPI.SpVoice")
		if err != nil {
			return nil, err
		}
		e.voice = v
	}
	if voice != e.current {
		if err := e.selectVoice(voice); err != nil {
			return nil, err
		}
		e.current = voice
	}
	rate, _ := intParam(params, "rate") // -10..10
	if _, err := oleutil.PutProperty(e.voice, "Rate", rate); err != nil {
		return nil, fmt.Errorf("system voice: set rate: %w", err)
	}

	stream, err := createDispatch("SAPI.SpMemoryStream")
	if err != nil {
		return nil, err
	}
	defer stream.Release()
	format, err := oleutil.GetProperty(stream, "Format")
	if err != nil {
		return nil, fmt.Errorf("system voice: get format: %w", err)
	}
	f := format.ToIDispatch()
	defer f.Release()
	if _, err := oleutil.PutProperty(f, "Type", saft22kHz16BitMono); err != nil {
		return nil, fmt.Errorf("system voice: set format: %w", err)
	}
	if _, err := oleutil.PutPropertyRef(e.voice, "AudioOutputStream", stream); err != nil {
		return nil, fmt.Errorf("system voice: set output: %w", err)
	}
	if _, err := oleutil.CallMethod(e.voice, "Speak", text, svsfIsNotXML); err != nil {
		return nil, fmt.Errorf("system voice: speak: %w", err)
	}
	data, err := oleutil.CallMethod(stream, "GetData")
	if err != nil {
		return nil, fmt.Errorf("system voice: read audio: %w", err)
	}
	defer data.Clear()
	arr := data.ToArray()
	if arr == nil {
		return nil, errors.New("system voice: no audio returned")
	}
	return audio.EncodeWAV(audio.PCM16(sampleRate, 1), arr.ToByteArray()), nil
}

// selectVoice picks the first installed voice whose description contains name
// (case-insensitive); empty name keeps the system default.
func (e *engine) selectVoice(name string) error {
	if name == "" {
		return nil
	}
	res, err := oleutil.CallMethod(e.voice, "GetVoices")
	if err != nil {
		return fmt.Errorf("system voice: list voices: %w", err)
	}
	tokens := res.ToIDispatch()
	defer tokens.Release()
	count, err := oleutil.GetProperty(tokens, "Count")
	if err != nil {
		return fmt.Errorf("system voice: list voices: %w", err)
	}
	n, _ := count.Value().(int32)
	var names []string
	for i := range int(n) {
		item, err := oleutil.CallMethod(tokens, "Item", i)
		if err != nil {
			return fmt.Errorf("system voice: list voices: %w", err)
		}
		token := item.ToIDispatch()
		desc, err := oleutil.CallMethod(token, "GetDescription")
		if err != nil {
			token.Release()
			return fmt.Errorf("system voice: list voices: %w", err)
		}
		descName := desc.ToString()
		if strings.Contains(strings.ToLower(descName), strings.ToLower(name)) {
			_, err := oleutil.PutPropertyRef(e.voice, "Voice", token)
			token.Release()
			if err != nil {
				return fmt.Errorf("system voice: select %q: %w", descName, err)
			}
			return nil
		}
		token.Release()
		names = append(names, descName)
	}
	return fmt.Errorf("system voice %q is not installed (available: %s)", name, strings.Join(names, ", "))
}

func createDispatch(progID string) (*ole.IDispatch, error) {
	unk, err := oleutil.CreateObject(progID)
	if err != nil {
		return nil, fmt.Errorf("system voice: create %s: %w", progID, err)
	}
	defer unk.Release()
	d, err := unk.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return nil, fmt.Errorf("system voice: %s: %w", progID, err)
	}
	return d, nil
}
