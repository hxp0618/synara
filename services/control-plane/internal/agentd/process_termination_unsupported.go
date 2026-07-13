//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package agentd

import (
	"errors"
	"os/exec"
)

type processTree struct{}

func newProcessTree(*exec.Cmd) (*processTree, error) {
	return nil, errors.New("process-tree isolation is unsupported on this operating system")
}

func (*processTree) started() error   { return errors.New("process-tree isolation is unsupported") }
func (*processTree) terminate() error { return nil }
func (*processTree) release() error   { return nil }
