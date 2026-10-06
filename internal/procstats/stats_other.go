//go:build !linux

package procstats

import "errors"

const Supported = false

func Read(pid int, expectedStartID string) (*Sample, error) {
	return nil, errors.New("live process stats require Linux procfs")
}
