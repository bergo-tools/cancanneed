//go:build !darwin && !linux

package agent

import "os/exec"

// CommandContext's platform-default cancellation is retained on platforms
// where cancanneed does not yet provide process-group termination.
func configureProcessGroup(_ *exec.Cmd) {}
