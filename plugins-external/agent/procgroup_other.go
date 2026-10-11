//go:build !unix

package main

// procgroup_other.go — process-group handling on platforms without POSIX
// process groups. exec.CommandContext still kills the direct child there.

import "os/exec"

func setProcGroup(cmd *exec.Cmd) {}
