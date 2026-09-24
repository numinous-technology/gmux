// Package fence limits which hosts a job can reach, without root.
//
// It has two halves. The Proxy decides: every connection a fenced job makes
// arrives here, and is relayed only if the name the job itself sent (the TLS
// server name, or the HTTP Host header) is on the job's allowlist. Nothing is
// decrypted. The Supervisor (supervisor_linux.go) enforces: a seccomp filter
// on the job hands the job a socket already connected to the proxy whatever
// address it asked for, so the job cannot route around the check.
package fence

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StubAddress is what the DNS stub answers for every name. Resolution has to
// succeed for a client to reach connect(); the address is useless because
// every connection is redirected to the proxy regardless.
var StubAddress = net.IPv4(127, 0, 0, 2).To4()

// Allowlist holds names a job may reach. "example.com" also allows its
// subdomains; "*" allows everything.
type Allowlist struct {
	mu    sync.RWMutex
	names []string
}

// NewAllowlist builds an allowlist from names.
func NewAllowlist(names []string) *Allowlist {
	a := &Allowlist{}
	a.Set(names)
	return a
}

// Set replaces the names.
func (a *Allowlist) Set(names []string) {
	clean := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(n), "."))
		n = strings.TrimPrefix(n, "*.")
		if n != "" {
			clean = append(clean, n)
		}
	}
	a.mu.Lock()
	a.names = clean
	a.mu.Unlock()
}

// Allows reports whether a host may be reached.
func (a *Allowlist) Allows(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, n := range a.names {
		if n == "*" || host == n || strings.HasSuffix(host, "."+n) {
			return true
		}
	}
	return false
}

// SNI returns the server name in a TLS ClientHello, or "" if this is not one
// or it names no server.
func SNI(b []byte) string {
	if len(b) < 5 || b[0] != 0x16 {
		return ""
	}
	end := 5 + int(binary.BigEndian.Uint16(b[3:5]))
	if end > len(b) {
		end = len(b)
	}
	body := b[:end]
	at := 5 + 4 + 2 + 32
	get := func(n int) (int, bool) {
		if at+n > len(body) {
			return 0, false
		}
		v := 0
		for i := 0; i < n; i++ {
			v = v<<8 | int(body[at+i])
		}
		return v, true
	}
	l, ok := get(1)
	if !ok {
		return ""
	}
	at += 1 + l // session id
	if l, ok = get(2); !ok {
		return ""
	}
	at += 2 + l // cipher suites
	if l, ok = get(1); !ok {
		return ""
	}
	at += 1 + l // compression
	at += 2     // extensions length
	for at+4 <= len(body) {
		kind := int(binary.BigEndian.Uint16(body[at:]))
		size := int(binary.BigEndian.Uint16(body[at+2:]))
		start := at + 4
		at = start + size
		if kind != 0 || at > len(body) {
			continue
		}
		chunk := body[start:at]
		c := 2
		for c+3 <= len(chunk) {
			n := int(binary.BigEndian.Uint16(chunk[c+1:]))
			if chunk[c] == 0 && c+3+n <= len(chunk) {
				name := string(chunk[c+3 : c+3+n])
				for _, r := range name {
					if r > 127 {
						return "" // a name we cannot read is a name we must not relay
					}
				}
				return name
			}
			c += 3 + n
		}
	}
	return ""
}

// HostHeader returns the Host of an HTTP request and the port it names.
func HostHeader(b []byte) (string, int) {
	head, _, _ := bytes.Cut(b, []byte("\r\n\r\n"))
	lines := bytes.Split(head, []byte("\r\n"))
	for _, line := range lines[1:] {
		if len(line) > 5 && strings.EqualFold(string(line[:5]), "host:") {
			v := strings.TrimSpace(string(line[5:]))
			name, port, found := strings.Cut(v, ":")
			if p, err := strconv.Atoi(port); found && err == nil {
				return name, p
			}
			return name, 80
		}
	}
	return "", 80
}

// Proxy relays allowed connections and refuses the rest.
type Proxy struct {
	Allow *Allowlist
	// Dial reaches the real destination; tests replace it.
	Dial func(network, addr string) (net.Conn, error)

	tcp net.Listener
	udp net.PacketConn

	mu      sync.Mutex
	Refused []string
	Relayed []string
}

// Listen starts the proxy and the DNS stub on loopback ports.
func (p *Proxy) Listen() error {
	if p.Dial == nil {
		p.Dial = func(n, a string) (net.Conn, error) { return net.DialTimeout(n, a, 20*time.Second) }
	}
	var err error
	if p.tcp, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		return err
	}
	if p.udp, err = net.ListenPacket("udp", "127.0.0.1:0"); err != nil {
		p.tcp.Close()
		return err
	}
	go p.serveTCP()
	go p.serveDNS()
	return nil
}

// Addrs are the proxy's and the DNS stub's addresses.
func (p *Proxy) Addrs() (tcp, dns string) { return p.tcp.Addr().String(), p.udp.LocalAddr().String() }

// Close stops both listeners.
func (p *Proxy) Close() {
	if p.tcp != nil {
		p.tcp.Close()
	}
	if p.udp != nil {
		p.udp.Close()
	}
}

func (p *Proxy) serveTCP() {
	for {
		c, err := p.tcp.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *Proxy) handle(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	head, err := readHead(c)
	if len(head) == 0 || err != nil && len(head) == 0 {
		return
	}
	var name string
	port, isHTTP := 443, false
	if head[0] == 0x16 {
		name = SNI(head)
	} else {
		isHTTP = true
		name, port = HostHeader(head)
		if name == "" {
			io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
			return
		}
	}
	if !p.Allow.Allows(name) {
		p.note(&p.Refused, name)
		if isHTTP {
			body := "blocked by gmux egress policy: " + name
			fmt.Fprintf(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		}
		return
	}
	up, err := p.Dial("tcp", net.JoinHostPort(name, strconv.Itoa(port)))
	if err != nil {
		if isHTTP {
			io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		}
		return
	}
	defer up.Close()
	p.note(&p.Relayed, name)
	c.SetReadDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, io.MultiReader(bytes.NewReader(head), c)); closeWrite(up); done <- struct{}{} }()
	go func() { io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

// readHead reads until it has a whole TLS record or a whole set of HTTP
// headers, whichever this connection starts with, reading only what the
// client has sent (a short request never fills a fixed-size buffer).
func readHead(c net.Conn) ([]byte, error) {
	buf := make([]byte, 0, 16384)
	chunk := make([]byte, 4096)
	for len(buf) < 16384 {
		n, err := c.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if len(buf) >= 5 && buf[0] == 0x16 {
			if len(buf) >= 5+int(binary.BigEndian.Uint16(buf[3:5])) {
				return buf, nil
			}
		} else if bytes.Contains(buf, []byte("\r\n\r\n")) {
			return buf, nil
		}
		if err != nil {
			return buf, err
		}
	}
	return buf, nil
}

func closeWrite(c net.Conn) {
	if t, ok := c.(interface{ CloseWrite() error }); ok {
		t.CloseWrite()
	}
}

func (p *Proxy) note(list *[]string, name string) {
	p.mu.Lock()
	*list = append(*list, name)
	p.mu.Unlock()
}

func (p *Proxy) serveDNS() {
	buf := make([]byte, 4096)
	for {
		n, peer, err := p.udp.ReadFrom(buf)
		if err != nil {
			return
		}
		if a := DNSAnswer(buf[:n]); a != nil {
			p.udp.WriteTo(a, peer)
		}
	}
}

// DNSAnswer answers an A query for any name with StubAddress, and any other
// query type with an empty answer.
func DNSAnswer(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	end := 12
	for end < len(q) && q[end] != 0 {
		end += 1 + int(q[end])
	}
	if end+5 > len(q) {
		return nil
	}
	question := q[12 : end+5]
	qtype := binary.BigEndian.Uint16(q[end+1:])
	var out bytes.Buffer
	out.Write(q[:2])
	answers := uint16(0)
	if qtype == 1 {
		answers = 1
	}
	binary.Write(&out, binary.BigEndian, []uint16{0x8180, 1, answers, 0, 0})
	out.Write(question)
	if answers == 1 {
		out.Write([]byte{0xc0, 0x0c})
		binary.Write(&out, binary.BigEndian, []uint16{1, 1})
		binary.Write(&out, binary.BigEndian, uint32(60))
		binary.Write(&out, binary.BigEndian, uint16(4))
		out.Write(StubAddress)
	}
	return out.Bytes()
}
