//go:build unix

package main

import "syscall"

// detachedProcAttr puts the daemon in its own session.
//
// Setsid, not merely backgrounding. A child left in the harness's process group
// receives the SIGHUP that the terminal sends when its window is closed, and
// that is precisely the case the daemon exists to survive: the harness dies
// without firing SessionEnd, and the daemon's job is to notice and flush what
// the session captured. A daemon killed by the same signal as its harness
// delivers nothing at exactly the moment delivery matters most.
func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
