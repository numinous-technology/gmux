package fence

import (
	"testing"
	"unsafe"
)

// The kernel structs the fence hands to seccomp ioctls must match byte for
// byte. The end-to-end fence test cannot run in sandboxes that already hold a
// seccomp filter, so these layout checks are what catches an ABI slip locally.
// A 32-bit addfd id once shifted every later field; the kernel then rejected
// every socket handoff and --allow could never work.
func TestSeccompStructLayouts(t *testing.T) {
	var a addFD
	if got := unsafe.Sizeof(a); got != 24 {
		t.Fatalf("seccomp_notif_addfd is 24 bytes, addFD is %d", got)
	}
	if got := unsafe.Sizeof(a.id); got != 8 {
		t.Fatalf("seccomp_notif_addfd.id is a u64, addFD.id is %d bytes", got)
	}
	if off := unsafe.Offsetof(a.flags); off != 8 {
		t.Fatalf("seccomp_notif_addfd.flags is at offset 8, addFD.flags is at %d", off)
	}
	if off := unsafe.Offsetof(a.newfdFlags); off != 20 {
		t.Fatalf("seccomp_notif_addfd.newfd_flags is at offset 20, got %d", off)
	}
	if got := unsafe.Sizeof(notifReq{}); got != 80 {
		t.Fatalf("seccomp_notif is 80 bytes, notifReq is %d", got)
	}
}
