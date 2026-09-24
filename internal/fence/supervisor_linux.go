//go:build linux

package fence

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// The fence's enforcement half. A seccomp filter on the job notifies a
// supervisor, running outside the filter, for every socket, connect and send.
// The supervisor owns every INET socket in the job: socket() is answered with
// one the supervisor made, and connect() replaces it with a socket the
// supervisor has already connected to the proxy (TCP) or the DNS stub (UDP).
// The job never holds a socket to an address of its own choosing, so the
// allowlist is decided by the proxy on the name the job sends, not on an
// address the job could rewrite after a check.
//
// This ports numinous-gpu's netfence, which ran in production. The design and
// the seccomp programs are unchanged; only the language is.

const (
	sysSeccomp            = 317
	prSetNoNewPrivs       = 38
	setModeFilter         = 1
	filterFlagNewListener = 8
	notifFlagContinue     = 1
	addfdFlagSetFD        = 1
	auditArchX86_64       = 0xC000003E
	auditArchAArch64      = 0xC00000B7
	retKillProcess        = 0x80000000
	retErrno              = 0x00050000
	retAllow              = 0x7FFF0000
	retUserNotif          = 0x7FC00000
	oCloexec              = 0o2000000
	afInet, afInet6       = 2, 10
	afPacket              = 17
	sockTypeMask          = 0xF
	sockCloexec           = 0o2000000
	sockNonblock          = 0o4000
	eperm, econnrefused   = 1, 111
	fDupFD, fDupFDCloexec = 0, 1030
	handoffFD             = 1023
)

// syscall numbers differ by architecture.
type sysnums struct{ socket, connect, sendto, sendmsg, sendmmsg, closeS, dup, dup2, dup3, fcntl uint32 }

func arch() (uint32, sysnums, bool) {
	switch runtime.GOARCH {
	case "amd64":
		return auditArchX86_64, sysnums{41, 42, 44, 46, 307, 3, 32, 33, 292, 72}, true
	case "arm64":
		// arm64 has no socketcall, no dup2; dup3 replaces it.
		return auditArchAArch64, sysnums{198, 203, 206, 211, 269, 57, 23, 0xFFFFFFFF, 24, 25}, true
	}
	return 0, sysnums{}, false
}

type sockFilter struct {
	code uint16
	jt   uint8
	jf   uint8
	k    uint32
}

type sockFprog struct {
	length uint16
	filter *sockFilter
}

func insn(code uint16, jt, jf uint8, k uint32) sockFilter { return sockFilter{code, jt, jf, k} }

const (
	bpfLD   = 0x00
	bpfJMP  = 0x05
	bpfRET  = 0x06
	bpfW    = 0x00
	bpfABS  = 0x20
	bpfJEQ  = 0x10
	bpfK    = 0x00
	offNR   = 0
	offArch = 4
	offArg0 = 16
)

// denyProgram fails socket(AF_INET|AF_INET6|AF_PACKET) with EPERM, allows the
// rest. A wrong architecture kills the process.
func denyProgram(a uint32, s sysnums) []sockFilter {
	return []sockFilter{
		insn(bpfLD|bpfW|bpfABS, 0, 0, offArch),
		insn(bpfJMP|bpfJEQ|bpfK, 1, 0, a),
		insn(bpfRET|bpfK, 0, 0, retKillProcess),
		insn(bpfLD|bpfW|bpfABS, 0, 0, offNR),
		insn(bpfJMP|bpfJEQ|bpfK, 0, 5, s.socket),
		insn(bpfLD|bpfW|bpfABS, 0, 0, offArg0),
		insn(bpfJMP|bpfJEQ|bpfK, 2, 0, afInet),
		insn(bpfJMP|bpfJEQ|bpfK, 1, 0, afInet6),
		insn(bpfJMP|bpfJEQ|bpfK, 0, 1, afPacket),
		insn(bpfRET|bpfK, 0, 0, retErrno|eperm),
		insn(bpfRET|bpfK, 0, 0, retAllow),
	}
}

// allowlistProgram notifies the supervisor for socket, connect and the sends,
// allows sendmsg on the handoff fd (that is how the listener fd is delivered),
// refuses AF_PACKET, and allows everything else.
func allowlistProgram(a uint32, s sysnums) []sockFilter {
	const notify, epermAt, allowAt = 21, 22, 23
	here := func(i, target int) uint8 { return uint8(target - (i + 1)) }
	notifyIf := func(i int, k uint32) sockFilter { return insn(bpfJMP|bpfJEQ|bpfK, here(i, notify), 0, k) }
	return []sockFilter{
		insn(bpfLD|bpfW|bpfABS, 0, 0, offArch),                                 // 0
		insn(bpfJMP|bpfJEQ|bpfK, 1, 0, a),                                      // 1
		insn(bpfRET|bpfK, 0, 0, retKillProcess),                                // 2
		insn(bpfLD|bpfW|bpfABS, 0, 0, offNR),                                   // 3
		notifyIf(4, s.connect),                                                 // 4
		notifyIf(5, s.sendto),                                                  // 5
		insn(bpfJMP|bpfJEQ|bpfK, 0, 3, s.sendmsg),                              // 6
		insn(bpfLD|bpfW|bpfABS, 0, 0, offArg0),                                 // 7
		insn(bpfJMP|bpfJEQ|bpfK, here(8, allowAt), 0, handoffFD),               // 8
		insn(bpfRET|bpfK, 0, 0, retUserNotif),                                  // 9
		notifyIf(10, s.sendmmsg),                                               // 10
		notifyIf(11, s.closeS),                                                 // 11
		notifyIf(12, s.dup),                                                    // 12
		notifyIf(13, s.dup2),                                                   // 13
		notifyIf(14, s.dup3),                                                   // 14
		notifyIf(15, s.fcntl),                                                  // 15
		insn(bpfJMP|bpfJEQ|bpfK, 0, here(16, allowAt), s.socket),               // 16
		insn(bpfLD|bpfW|bpfABS, 0, 0, offArg0),                                 // 17
		insn(bpfJMP|bpfJEQ|bpfK, here(18, epermAt), 0, afPacket),               // 18
		notifyIf(19, afInet),                                                   // 19
		insn(bpfJMP|bpfJEQ|bpfK, here(20, notify), here(20, allowAt), afInet6), // 20
		insn(bpfRET|bpfK, 0, 0, retUserNotif),                                  // 21
		insn(bpfRET|bpfK, 0, 0, retErrno|eperm),                                // 22
		insn(bpfRET|bpfK, 0, 0, retAllow),                                      // 23
	}
}

func setNoNewPrivs() error {
	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("PR_SET_NO_NEW_PRIVS: %v", e)
	}
	return nil
}

func installFilter(prog []sockFilter, flags uintptr) (int, error) {
	fprog := sockFprog{length: uint16(len(prog)), filter: &prog[0]}
	r, _, e := syscall.Syscall(sysSeccomp, setModeFilter, flags, uintptr(unsafe.Pointer(&fprog)))
	if e != 0 {
		return -1, fmt.Errorf("seccomp: %v", e)
	}
	return int(r), nil
}

// RunDenied installs the deny filter on the calling thread and execs the
// command. The caller must have locked the OS thread. It does not return on
// success.
func RunDenied(argv []string, env []string) error {
	a, s, ok := arch()
	if !ok {
		return fmt.Errorf("network fence is not supported on %s", runtime.GOARCH)
	}
	if err := setNoNewPrivs(); err != nil {
		return err
	}
	if _, err := installFilter(denyProgram(a, s), 0); err != nil {
		return err
	}
	path, err := lookPath(argv[0], env)
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, env)
}

// InstallListener installs the allowlist filter on the calling thread and
// returns the notification fd. The caller must have locked the OS thread and
// must exec afterwards on this same thread.
func InstallListener() (int, error) {
	a, s, ok := arch()
	if !ok {
		return -1, fmt.Errorf("network fence is not supported on %s", runtime.GOARCH)
	}
	if err := setNoNewPrivs(); err != nil {
		return -1, err
	}
	return installFilter(allowlistProgram(a, s), filterFlagNewListener)
}

func lookPath(name string, env []string) (string, error) {
	if len(name) > 0 && (name[0] == '/' || name[0] == '.') {
		return name, nil
	}
	var path string
	for _, kv := range env {
		if len(kv) > 5 && kv[:5] == "PATH=" {
			path = kv[5:]
		}
	}
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	for _, dir := range splitList(path) {
		p := dir + "/" + name
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found in PATH", name)
}

func splitList(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ':' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// ioctl request codes.
func ioc(dir, size, nr uintptr) uintptr { return (dir << 30) | (size << 16) | (0x21 << 8) | nr }

var (
	notifRecv  = ioc(3, 80, 0)
	notifSend  = ioc(3, 24, 1)
	notifValid = ioc(1, 8, 2)
	notifAddFD = ioc(1, 24, 3)
)

type notifReq struct {
	id   uint64
	pid  uint32
	flag uint32
	nr   int32
	arch uint32
	ip   uint64
	args [6]uint64
}

type notifResp struct {
	id    uint64
	val   int64
	error int32
	flags uint32
}

type addFD struct {
	id, flags, srcfd, newfd, newfdFlags uint32
	_pad                                uint32
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if e != 0 {
		return e
	}
	return nil
}

// Supervisor answers the fenced job's socket calls against a proxy and a DNS
// stub. It is created with the notification fd and the two addresses.
type Supervisor struct {
	fd       int
	proxy    *net.TCPAddr
	dns      *net.UDPAddr
	sn       sysnums
	table    map[uint32]*entry // job fd -> our socket for it
	Injected int
	Refused  int
}

// entry is what the supervisor holds for one of the job's INET fds: the kind
// it was created with, and our connected socket once connect() has run (fd is
// -1 until then).
type entry struct {
	kind int
	fd   int
}

// NewSupervisor prepares a supervisor. proxyAddr and dnsAddr are host:port.
func NewSupervisor(notifyFD int, proxyAddr, dnsAddr string) (*Supervisor, error) {
	_, sn, ok := arch()
	if !ok {
		return nil, fmt.Errorf("unsupported arch")
	}
	pa, err := net.ResolveTCPAddr("tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	da, err := net.ResolveUDPAddr("udp", dnsAddr)
	if err != nil {
		return nil, err
	}
	return &Supervisor{fd: notifyFD, proxy: pa, dns: da, sn: sn, table: map[uint32]*entry{}}, nil
}

func (s *Supervisor) valid(id uint64) bool {
	return ioctl(s.fd, notifValid, unsafe.Pointer(&id)) == nil
}

func (s *Supervisor) respond(id uint64, errno int32, val int64, flags uint32) {
	r := notifResp{id: id, val: val, error: errno, flags: flags}
	ioctl(s.fd, notifSend, unsafe.Pointer(&r))
}

func (s *Supervisor) cont(id uint64) { s.respond(id, 0, 0, notifFlagContinue) }
func (s *Supervisor) deny(id uint64) { s.Refused++; s.respond(id, -eperm, 0, 0) }

func (s *Supervisor) addfd(id uint64, srcfd int, newfd int, cloexec bool) (int, error) {
	a := addFD{id: uint32(id), srcfd: uint32(srcfd)}
	if newfd >= 0 {
		a.flags = addfdFlagSetFD
		a.newfd = uint32(newfd)
	}
	if cloexec {
		a.newfdFlags = oCloexec
	}
	r, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(s.fd), notifAddFD, uintptr(unsafe.Pointer(&a)))
	if e != 0 {
		return -1, e
	}
	return int(r), nil
}

// Serve answers notifications until the job is gone.
func (s *Supervisor) Serve() {
	for {
		var req notifReq
		if err := ioctl(s.fd, notifRecv, unsafe.Pointer(&req)); err != nil {
			if err == syscall.EINTR || err == syscall.EAGAIN {
				continue
			}
			return
		}
		s.handle(&req)
	}
}

func (s *Supervisor) handle(req *notifReq) {
	nr := uint32(req.nr)
	switch nr {
	case s.sn.socket:
		s.onSocket(req)
	case s.sn.closeS:
		delete(s.table, uint32(req.args[0]))
		s.cont(req.id)
	case s.sn.connect:
		s.onConnect(req)
	case s.sn.dup, s.sn.dup2, s.sn.dup3, s.sn.fcntl:
		s.onDup(req, nr)
	default: // the sends
		e, ok := s.table[uint32(req.args[0])]
		if !ok {
			s.cont(req.id) // not our socket: the kernel judges it
			return
		}
		if e.fd < 0 { // a send before connect on our socket: refuse
			s.deny(req.id)
			return
		}
		s.cont(req.id) // connected by construction (stream->proxy, dgram->stub)
	}
}

func (s *Supervisor) onSocket(req *notifReq) {
	domain := req.args[0]
	kind := int(req.args[1] & sockTypeMask)
	if domain != afInet && domain != afInet6 {
		s.deny(req.id)
		return
	}
	if kind != syscall.SOCK_STREAM && kind != syscall.SOCK_DGRAM {
		s.deny(req.id)
		return
	}
	sock, err := syscall.Socket(syscall.AF_INET, kind, 0)
	if err != nil {
		s.deny(req.id)
		return
	}
	if !s.valid(req.id) {
		syscall.Close(sock)
		return
	}
	fd, err := s.addfd(req.id, sock, -1, req.args[1]&sockCloexec != 0)
	syscall.Close(sock)
	if err != nil {
		s.deny(req.id)
		return
	}
	s.table[uint32(fd)] = &entry{kind: kind, fd: -1}
	s.respond(req.id, 0, int64(fd), 0)
}

func (s *Supervisor) onConnect(req *notifReq) {
	jobfd := uint32(req.args[0])
	e, ok := s.table[jobfd]
	if !ok {
		s.cont(req.id) // a unix socket or file: the kernel judges it
		return
	}
	relay, err := s.dial(e.kind)
	if err != nil {
		s.respond(req.id, -econnrefused, 0, 0)
		return
	}
	if !s.valid(req.id) {
		syscall.Close(relay)
		return
	}
	if _, err := s.addfd(req.id, relay, int(jobfd), false); err != nil {
		syscall.Close(relay)
		s.deny(req.id)
		return
	}
	s.table[jobfd] = &entry{kind: e.kind, fd: relay}
	s.Injected++
	s.respond(req.id, 0, 0, 0)
}

// dial connects to the proxy for a stream socket and to the DNS stub for a
// datagram socket, so a job's TLS or HTTP goes to the name-checking proxy and
// its DNS goes to the stub.
func (s *Supervisor) dial(kind int) (int, error) {
	sock, err := syscall.Socket(syscall.AF_INET, kind, 0)
	if err != nil {
		return -1, err
	}
	var ip net.IP
	var port int
	if kind == syscall.SOCK_DGRAM {
		ip, port = s.dns.IP, s.dns.Port
	} else {
		ip, port = s.proxy.IP, s.proxy.Port
	}
	sa := &syscall.SockaddrInet4{Port: port}
	copy(sa.Addr[:], ip.To4())
	if err := syscall.Connect(sock, sa); err != nil {
		syscall.Close(sock)
		return -1, err
	}
	return sock, nil
}

func (s *Supervisor) onDup(req *notifReq, nr uint32) {
	src := uint32(req.args[0])
	e, ours := s.table[src]
	relay := 0
	if ours {
		relay = e.fd
	}
	if nr == s.sn.fcntl {
		if (req.args[1] != fDupFD && req.args[1] != fDupFDCloexec) || !ours {
			s.cont(req.id)
			return
		}
		s.deny(req.id) // F_DUPFD asks for "at least N"; addfd cannot promise it
		return
	}
	if !ours {
		if nr == s.sn.dup2 || nr == s.sn.dup3 {
			delete(s.table, uint32(req.args[1]))
		}
		s.cont(req.id)
		return
	}
	if !s.valid(req.id) {
		return
	}
	if relay < 0 {
		s.cont(req.id) // duped before connect; nothing to relay yet
		return
	}
	dup, err := syscall.Dup(relay)
	if err != nil {
		s.deny(req.id)
		return
	}
	var newfd int
	if nr == s.sn.dup {
		newfd = -1
	} else {
		newfd = int(req.args[1])
	}
	fd, err := s.addfd(req.id, dup, newfd, false)
	syscall.Close(dup)
	if err != nil {
		s.deny(req.id)
		return
	}
	d, _ := syscall.Dup(relay)
	s.table[uint32(fd)] = &entry{kind: e.kind, fd: d}
	s.respond(req.id, 0, int64(fd), 0)
}

// unused imports guard
var _ = binary.BigEndian

// Fenced is the child side of an allowlisted run. It expects a connected
// AF_UNIX socket to the supervisor at file descriptor sockFD, installs the
// allowlist filter on a locked thread, sends the notification fd to the
// supervisor over that socket, waits for the go-ahead, and execs the command.
// It does not return on success.
//
// The unix socket is duplicated to a fixed number the filter allows sendmsg
// on, so the fd can be delivered before the supervisor is serving without the
// send being trapped by the very filter it is bootstrapping.
func Fenced(sockFD int, argv, env []string) error {
	runtime.LockOSThread()
	if err := syscall.Dup2(sockFD, handoffFD); err != nil {
		return fmt.Errorf("pin handoff socket: %w", err)
	}
	notify, err := InstallListener()
	if err != nil {
		return err
	}
	rights := syscall.UnixRights(notify)
	if err := syscall.Sendmsg(handoffFD, []byte{'f'}, rights, nil, 0); err != nil {
		return fmt.Errorf("deliver listener fd: %w", err)
	}
	buf := make([]byte, 2)
	if _, err := syscall.Read(handoffFD, buf); err != nil {
		return fmt.Errorf("wait for supervisor: %w", err)
	}
	syscall.Close(notify)
	syscall.Close(handoffFD)
	path, err := lookPath(argv[0], env)
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, env)
}

// ListenerPresent reports whether this process already has a seccomp
// user-notification listener, which prevents installing another. On a normal
// host this is false; some sandboxes install one, and the fence cannot be
// nested inside it.
func ListenerPresent() bool {
	st, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range splitList2(string(st)) {
		if len(line) >= 8 && line[:8] == "Seccomp:" {
			v := line[8:]
			// mode 2 is filter mode; a listener implies filter mode, so this is
			// a necessary condition. The precise listener count is not exported,
			// so a filter already present is treated as "cannot nest".
			return len(v) > 0 && (contains(v, "2"))
		}
	}
	return false
}

func splitList2(s string) []string {
	var out, cur = []string{}, ""
	for _, c := range s {
		if c == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(c)
		}
	}
	return append(out, cur)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// RecvListenerFD reads the notification fd the fenced child sent over sock.
func RecvListenerFD(sock int) (int, error) {
	buf := make([]byte, 8)
	oob := make([]byte, syscall.CmsgSpace(4))
	_, oobn, _, _, err := syscall.Recvmsg(sock, buf, oob, 0)
	if err != nil {
		return -1, err
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) == 0 {
		return -1, fmt.Errorf("no control message from fenced child")
	}
	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) == 0 {
		return -1, fmt.Errorf("no fd from fenced child")
	}
	return fds[0], nil
}
