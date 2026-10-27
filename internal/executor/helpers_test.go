package executor

import (
	"os/exec"

	"github.com/khanhicetea/minicrond/internal/model"
)

func buildCommand(d model.Definition, r model.Run) (*exec.Cmd, string, error) {
	return buildCommandWithTrustedPath(d, r, nil)
}
