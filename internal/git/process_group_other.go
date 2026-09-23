//go:build !darwin && !linux

package git

import "os/exec"

func configureProcessGroup(_ *exec.Cmd) {}
