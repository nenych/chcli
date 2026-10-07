package auth

import "os/exec"

// configureProcess has nothing to add on Windows: cancellation kills the
// process, and WaitDelay bounds the wait for its descendants' output.
func configureProcess(*exec.Cmd) {}
