//go:build !windows

package systemtts

import "errors"

func serve(reqs <-chan request) {
	for r := range reqs {
		r.resp <- result{err: errors.New("system voice: not supported on this platform yet")}
	}
}
