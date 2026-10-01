//go:build !windows

package audio

import "context"

func Record(sampleRate, channels int) (*Recording, error) { return nil, ErrUnsupported }

func Play(ctx context.Context, f Format, pcm []byte) error { return ErrUnsupported }
