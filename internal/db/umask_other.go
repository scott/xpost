//go:build !unix

package db

func umask077() int        { return 0 }
func restoreUmask(old int) {}
