//go:build !linux

package daemon

import "fmt"

// startFence is unavailable off Linux: the fence needs seccomp.
func (d *Daemon) startFence(sockFD int, allow []string) error {
	return fmt.Errorf("the network fence is only available on Linux")
}
