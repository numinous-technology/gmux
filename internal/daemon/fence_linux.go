//go:build linux

package daemon

import (
	"github.com/numinous-technology/gmux/internal/fence"
)

// startFence serves the network fence for a job. It creates the proxy with the
// job's allowlist, receives the seccomp listener fd the fenced child sends over
// the socket, and serves the supervisor until the job is gone. It returns once
// the child is running so the caller can track the process.
func (d *Daemon) startFence(sockFD int, allow []string) error {
	proxy := &fence.Proxy{Allow: fence.NewAllowlist(allow)}
	if err := proxy.Listen(); err != nil {
		return err
	}
	tcp, dns := proxy.Addrs()
	notify, err := fence.RecvListenerFD(sockFD)
	if err != nil {
		proxy.Close()
		return err
	}
	sup, err := fence.NewSupervisor(notify, tcp, dns)
	if err != nil {
		proxy.Close()
		return err
	}
	go func() {
		sup.Serve()
		proxy.Close()
	}()
	// tell the child to proceed past its filter install
	writeGo(sockFD)
	return nil
}
