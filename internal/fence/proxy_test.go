package fence

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAllowlistMatchesNamesAndSubdomains(t *testing.T) {
	a := NewAllowlist([]string{"pypi.org", "*.anthropic.com", "Example.COM."})
	for _, ok := range []string{"pypi.org", "files.pypi.org", "api.anthropic.com", "example.com"} {
		if !a.Allows(ok) {
			t.Fatalf("%s should be allowed", ok)
		}
	}
	for _, no := range []string{"evilpypi.org", "pypi.org.evil.com", "", "anthropic.co"} {
		if a.Allows(no) {
			t.Fatalf("%s should be refused", no)
		}
	}
	if !NewAllowlist([]string{"*"}).Allows("anything.at.all") {
		t.Fatal("* allows everything")
	}
}

// captureHello records the ClientHello a real TLS client sends for a name.
func captureHello(t *testing.T, name string) []byte {
	t.Helper()
	server, client := net.Pipe()
	go func() {
		c := tls.Client(client, &tls.Config{ServerName: name, InsecureSkipVerify: true})
		c.SetDeadline(time.Now().Add(time.Second))
		c.Handshake()
	}()
	buf := make([]byte, 4096)
	server.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := io.ReadAtLeast(server, buf, 5)
	for n < 5+int(buf[3])<<8+int(buf[4]) {
		m, err := server.Read(buf[n:])
		if err != nil {
			break
		}
		n += m
	}
	server.Close()
	return buf[:n]
}

func TestSNIFromARealClientHello(t *testing.T) {
	if got := SNI(captureHello(t, "api.openai.com")); got != "api.openai.com" {
		t.Fatalf("SNI = %q", got)
	}
	if SNI([]byte("GET / HTTP/1.1\r\n\r\n")) != "" || SNI(nil) != "" || SNI([]byte{0x16, 3, 1, 0, 10, 1}) != "" {
		t.Fatal("not a hello, no name")
	}
}

func TestHostHeader(t *testing.T) {
	h, p := HostHeader([]byte("GET / HTTP/1.1\r\nUser-Agent: x\r\nHost: pypi.org:8080\r\n\r\n"))
	if h != "pypi.org" || p != 8080 {
		t.Fatalf("%s %d", h, p)
	}
	if h, p = HostHeader([]byte("GET / HTTP/1.1\r\nhost: example.com\r\n\r\n")); h != "example.com" || p != 80 {
		t.Fatalf("%s %d", h, p)
	}
}

func TestDNSStubAnswersAQueriesOnly(t *testing.T) {
	q := []byte{0xab, 0xcd, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 4, 'p', 'y', 'p', 'i', 3, 'o', 'r', 'g', 0, 0, 1, 0, 1}
	a := DNSAnswer(q)
	if a == nil || a[0] != 0xab || a[7] != 1 || !strings.HasSuffix(string(a), string(StubAddress)) {
		t.Fatalf("A answer = %x", a)
	}
	q[len(q)-3] = 28 // AAAA
	if a := DNSAnswer(q); a == nil || a[7] != 0 {
		t.Fatalf("AAAA gets an empty answer, got %x", a)
	}
}

func TestProxyRelaysAllowedAndRefusesOthers(t *testing.T) {
	upstream := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello from "+r.Host) })}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go upstream.Serve(ln)
	defer upstream.Close()
	p := &Proxy{Allow: NewAllowlist([]string{"allowed.test"}),
		Dial: func(n, a string) (net.Conn, error) { return net.Dial(n, ln.Addr().String()) }}
	if err := p.Listen(); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	addr, _ := p.Addrs()
	get := func(host string) string {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		return resp.Status + " " + string(b)
	}
	if got := get("allowed.test"); !strings.HasPrefix(got, "200") || !strings.Contains(got, "hello from allowed.test") {
		t.Fatalf("allowed: %s", got)
	}
	if got := get("blocked.test"); !strings.HasPrefix(got, "403") || !strings.Contains(got, "blocked.test") {
		t.Fatalf("blocked: %s", got)
	}
	if len(p.Refused) != 1 || p.Refused[0] != "blocked.test" || len(p.Relayed) != 1 {
		t.Fatalf("refused=%v relayed=%v", p.Refused, p.Relayed)
	}
}
