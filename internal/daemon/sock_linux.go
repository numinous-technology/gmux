//go:build linux

package daemon

import "syscall"

func writeGo(fd int) { syscall.Write(fd, []byte("go")) }
