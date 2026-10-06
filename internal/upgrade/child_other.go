//go:build !unix

package upgrade

import "os/exec"

func ownGroup(*exec.Cmd) {}

func textBusy(error) bool { return false }
