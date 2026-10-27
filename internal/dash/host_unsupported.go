//go:build !linux

package dash

import "errors"

func newHostSampler() hostSampleFunc {
	return func() (HostSnapshot, error) {
		return HostSnapshot{}, errors.New("dash: host metrics unsupported on this platform")
	}
}
