//go:build darwin

package vmnetlink

/*
#cgo CFLAGS: -fblocks
#cgo LDFLAGS: -framework vmnet
#include <stdlib.h>
#include "shim_darwin.h"
*/
import "C"

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sync"
	"unsafe"
)

// ErrClosed is reported by ReadFrame and WriteFrame once Close has run,
// including by a ReadFrame that was already blocked. It is net.ErrClosed so
// callers can treat link shutdown the same way they treat a closed socket.
var ErrClosed = net.ErrClosed

// Link is a live vmnet shared-mode interface.
//
// One goroutine may read and another may write concurrently; ReadFrame itself
// is serialized because the frame lands in a single interface-wide receive
// buffer. Close may be called from any goroutine at any time.
type Link struct {
	h *C.vmnetlink_t

	readMu sync.Mutex

	closeMu sync.Mutex
	closed  bool

	gateway   netip.Addr
	maxPacket int
}

// Open starts a shared-mode (NAT) vmnet interface on cfg.Subnet.
//
// The interface belongs to the process, not to the uid that created it: the
// caller may drop privileges immediately afterwards and keep using the Link.
func Open(cfg Config) (*Link, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("vmnetlink: starting a vmnet interface requires euid 0 (running as euid %d)", os.Geteuid())
	}

	start, end, mask := subnetParams(cfg.Subnet)
	cStart := C.CString(start)
	defer C.free(unsafe.Pointer(cStart))
	cEnd := C.CString(end)
	defer C.free(unsafe.Pointer(cEnd))
	cMask := C.CString(mask)
	defer C.free(unsafe.Pointer(cMask))

	isolation := C.int(0)
	if cfg.Isolation {
		isolation = 1
	}

	var h *C.vmnetlink_t
	var status C.uint32_t
	if rc := C.vmnetlink_open(cStart, cEnd, cMask, isolation, &h, &status); rc != C.VMNETLINK_OK {
		return nil, shimError("start interface", int(rc), uint32(status))
	}

	l := &Link{
		h:         h,
		maxPacket: int(C.vmnetlink_max_packet_size(h)),
	}
	gw := C.GoString(C.vmnetlink_gateway(h))
	addr, err := netip.ParseAddr(gw)
	if err != nil {
		l.Close()
		return nil, fmt.Errorf("vmnetlink: vmnet reported an unparsable gateway address %q: %w", gw, err)
	}
	l.gateway = addr

	// The interface is stopped by Close; this only reclaims the C-side state if
	// the caller drops the Link without closing it.
	runtime.AddCleanup(l, func(h *C.vmnetlink_t) { C.vmnetlink_free(h) }, h)

	return l, nil
}

// ReadFrame blocks until one ethernet frame arrives and copies it into buf,
// returning its length. buf must be at least MaxPacketSize bytes; a frame that
// does not fit is reported as an error rather than truncated, because a partial
// frame is indistinguishable from a corrupt one downstream.
func (l *Link) ReadFrame(buf []byte) (int, error) {
	l.readMu.Lock()
	defer l.readMu.Unlock()

	var status C.uint32_t
	rc := C.vmnetlink_read(l.h, &status)
	// The cleanup registered in Open frees the C state; l must stay reachable
	// across the call, and across the copy out of its receive buffer below.
	defer runtime.KeepAlive(l)
	if rc < 0 {
		return 0, shimError("read", int(rc), uint32(status))
	}
	n := int(rc)
	if n > len(buf) {
		return 0, fmt.Errorf("vmnetlink: read: frame of %d bytes does not fit in a %d byte buffer", n, len(buf))
	}
	copy(buf, unsafe.Slice((*byte)(C.vmnetlink_rxbuf(l.h)), n))
	return n, nil
}

// WriteFrame writes one ethernet frame. vmnet writes a frame whole or not at
// all, so there is no short-write case to report.
func (l *Link) WriteFrame(p []byte) error {
	if len(p) == 0 {
		return fmt.Errorf("vmnetlink: write: empty frame")
	}
	var status C.uint32_t
	rc := C.vmnetlink_write(l.h, unsafe.Pointer(&p[0]), C.size_t(len(p)), &status)
	runtime.KeepAlive(l)
	if rc != C.VMNETLINK_OK {
		return shimError("write", int(rc), uint32(status))
	}
	return nil
}

// MaxPacketSize is the largest frame the interface will carry, as reported by
// vmnet_max_packet_size_key.
func (l *Link) MaxPacketSize() int { return l.maxPacket }

// Gateway is the host-side address vmnet assigned, i.e. the .1 of the subnet
// and the default route the guest should be handed.
func (l *Link) Gateway() netip.Addr { return l.gateway }

// Close stops the interface. A ReadFrame blocked at the time returns ErrClosed,
// and Close does not return until any in-flight read or write has left the
// framework. It is idempotent and safe to call concurrently with I/O.
func (l *Link) Close() error {
	l.closeMu.Lock()
	defer l.closeMu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true

	var status C.uint32_t
	rc := C.vmnetlink_close(l.h, &status)
	runtime.KeepAlive(l)
	if rc != C.VMNETLINK_OK {
		return shimError("stop interface", int(rc), uint32(status))
	}
	return nil
}

// shimError turns a shim return code into a Go error, naming the vmnet status
// when the framework itself supplied one.
func shimError(op string, rc int, status uint32) error {
	switch rc {
	case C.VMNETLINK_ERR_CLOSED:
		return fmt.Errorf("vmnetlink: %s: %w", op, ErrClosed)
	case C.VMNETLINK_ERR_NOMEM:
		return fmt.Errorf("vmnetlink: %s: out of memory", op)
	case C.VMNETLINK_ERR_START:
		return fmt.Errorf("vmnetlink: %s: vmnet refused to create the interface", op)
	case C.VMNETLINK_ERR_NOTWRITTEN:
		return fmt.Errorf("vmnetlink: %s: vmnet accepted no packets", op)
	case C.VMNETLINK_ERR_VMNET:
		return fmt.Errorf("vmnetlink: %s: %s", op, vmnetStatusString(status))
	default:
		return fmt.Errorf("vmnetlink: %s: shim error %d", op, rc)
	}
}

// vmnetStatusString names a vmnet_return_t. The framework offers no strerror,
// and the raw numbers are unsearchable.
func vmnetStatusString(status uint32) string {
	switch status {
	case 1000:
		return "VMNET_SUCCESS"
	case 1001:
		return "VMNET_FAILURE"
	case 1002:
		return "VMNET_MEM_FAILURE"
	case 1003:
		return "VMNET_INVALID_ARGUMENT"
	case 1004:
		return "VMNET_SETUP_INCOMPLETE"
	case 1005:
		return "VMNET_INVALID_ACCESS (permission denied; euid 0 required)"
	case 1006:
		return "VMNET_PACKET_TOO_BIG"
	case 1007:
		return "VMNET_BUFFER_EXHAUSTED"
	case 1008:
		return "VMNET_TOO_MANY_PACKETS"
	case 1009:
		return "VMNET_SHARING_SERVICE_BUSY"
	case 1010:
		return "VMNET_NOT_AUTHORIZED"
	default:
		return fmt.Sprintf("vmnet status %d", status)
	}
}
