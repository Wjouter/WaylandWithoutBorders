//go:build !linux

package network

import "net"

// outstandingSendBytes is Linux-only (SIOCOUTQ); elsewhere send-stall detection
// is unavailable, so report not-ok and rely on receive-side liveness alone.
func outstandingSendBytes(raw net.Conn) (int, bool) { return 0, false }
