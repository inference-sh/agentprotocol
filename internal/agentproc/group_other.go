//go:build !unix

package agentproc

import (
	"errors"
	"os"
	"os/exec"
)

func setGroup(*exec.Cmd) {}

func kill(p *os.Process) error {
	err := p.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
