//go:build !unix

package main

import "syscall"

// detachedProcAttr has no portable equivalent outside unix. The agent ships for
// macOS and Linux; this keeps the package buildable elsewhere and says plainly
// that a daemon started on such a platform is not protected from a signal sent
// to its parent's process group.
func detachedProcAttr() *syscall.SysProcAttr { return nil }
