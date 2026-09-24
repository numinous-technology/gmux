//go:build linux

package fence

import "syscall"

func socketpair() (int, int, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return 0, 0, err
	}
	return fds[0], fds[1], nil
}

func syscallClose(fd int) { syscall.Close(fd) }
func writeGo(fd int)      { syscall.Write(fd, []byte("go")) }
