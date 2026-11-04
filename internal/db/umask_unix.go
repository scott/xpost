//go:build unix

package db

import "syscall"

func umask077() int        { return syscall.Umask(0o077) }
func restoreUmask(old int) { syscall.Umask(old) }
