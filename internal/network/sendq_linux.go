//go:build linux

package network

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// outstandingSendBytes returns the number of bytes queued in the socket send
// buffer that the peer has not yet acknowledged (Linux SIOCOUTQ — the same
// value `ss` shows as Send-Q). ok is false if it can't be read. A value that
// stays high and never decreases means the peer stopped reading our data (its
// receive window is closed), which TCP still reports as an ESTAB connection.
func outstandingSendBytes(raw net.Conn) (n int, ok bool) {
	sc, isSyscallConn := raw.(syscall.Conn)
	if !isSyscallConn {
		return 0, false
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var ioErr error
	ctlErr := rc.Control(func(fd uintptr) {
		// SIOCOUTQ == TIOCOUTQ (0x5411): un-ACKed bytes in the send queue.
		n, ioErr = unix.IoctlGetInt(int(fd), unix.TIOCOUTQ)
	})
	if ctlErr != nil || ioErr != nil {
		return 0, false
	}
	return n, true
}
