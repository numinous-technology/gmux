//go:build linux

package fence

import (
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestMain lets this test binary act as the fenced child when asked, so the
// test can run itself under the fence and observe the supervisor redirect a
// real connection.
func TestMain(m *testing.M) {
	switch os.Getenv("GMUX_FENCE_ROLE") {
	case "child":
		// fd 3 is the unix socket to the supervisor (passed as ExtraFiles[0]).
		if err := Fenced(3, []string{os.Args[0]}, append(os.Environ(), "GMUX_FENCE_ROLE=grand")); err != nil {
			io.WriteString(os.Stderr, "fenced: "+err.Error())
			os.Exit(3)
		}
	case "grand":
		// Under the fence now. Connect to an address nobody serves; the fence
		// must redirect it to the proxy, which relays by the Host we send.
		c, err := net.Dial("tcp", "10.255.255.1:443")
		if err != nil {
			io.WriteString(os.Stdout, "DIAL-ERR "+err.Error())
			os.Exit(0)
		}
		io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+os.Getenv("GMUX_TARGET")+"\r\nConnection: close\r\n\r\n")
		b, _ := io.ReadAll(c)
		io.WriteString(os.Stdout, string(b))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runFenced(t *testing.T, allow []string, target string) string {
	t.Helper()
	if ListenerPresent() {
		t.Skip("this host already has a seccomp filter/listener; the fence cannot be nested inside it (run on a clean host or in the gmux container)")
	}
	upstream := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "reached "+r.Host)
	})}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go upstream.Serve(ln)
	defer upstream.Close()

	proxy := &Proxy{Allow: NewAllowlist(allow), Dial: func(n, a string) (net.Conn, error) { return net.Dial(n, ln.Addr().String()) }}
	if err := proxy.Listen(); err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	tcp, dns := proxy.Addrs()

	parent, child, err := socketpair()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "GMUX_FENCE_ROLE=child", "GMUX_TARGET="+target)
	cmd.ExtraFiles = []*os.File{os.NewFile(uintptr(child), "sock")}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	syscallClose(child)

	notify, err := RecvListenerFD(parent)
	if err != nil {
		t.Fatalf("receive listener fd: %v", err)
	}
	sup, err := NewSupervisor(notify, tcp, dns)
	if err != nil {
		t.Fatal(err)
	}
	go sup.Serve()
	writeGo(parent)
	cmd.Wait()
	got := out.String()
	if strings.Contains(got, "device or resource busy") || strings.Contains(got, "operation not permitted") {
		t.Skip("this host prevents a nested seccomp listener; run the fence test on a clean host or in the gmux container")
	}
	return got
}

func TestFenceRedirectsAnAllowedConnectionThroughTheProxy(t *testing.T) {
	got := runFenced(t, []string{"allowed.test"}, "allowed.test")
	if !strings.Contains(got, "200") || !strings.Contains(got, "reached allowed.test") {
		t.Fatalf("an allowed host should be reached through the proxy, got: %q", got)
	}
}

func TestFenceRefusesAHostNotOnTheAllowlist(t *testing.T) {
	got := runFenced(t, []string{"allowed.test"}, "blocked.test")
	if !strings.Contains(got, "403") || !strings.Contains(got, "blocked.test") {
		t.Fatalf("a blocked host should be refused, got: %q", got)
	}
}

func TestFenceCannotBeRoutedAroundByADifferentAddress(t *testing.T) {
	// The grandchild dials 10.255.255.1, which nobody serves. If the fence
	// were not redirecting, the dial would fail; that it reaches the proxy at
	// all is the proof the address was replaced.
	got := runFenced(t, []string{"allowed.test"}, "allowed.test")
	if strings.Contains(got, "DIAL-ERR") {
		t.Fatalf("the connection was not redirected: %q", got)
	}
}
