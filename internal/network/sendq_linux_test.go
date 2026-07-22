//go:build linux

package network

import (
	"net"
	"testing"
	"time"
)

// TestOutstandingSendBytes reproduces an asymmetric half-open (peer accepts but
// never reads) and confirms the SIOCOUTQ probe actually sees the backed-up send
// queue — the exact signal send-stall detection relies on.
func TestOutstandingSendBytes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c // hold it open but never Read → receiver window fills
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	tc := c.(*net.TCPConn)
	_ = tc.SetWriteBuffer(32 * 1024)

	// A live, drained socket should read ok with zero (or near-zero) queued.
	if n, ok := outstandingSendBytes(tc); !ok {
		t.Fatalf("probe should work on a linux TCP socket, got ok=false (n=%d)", n)
	}

	// Blast data the peer never reads; once its window closes, bytes back up.
	buf := make([]byte, 32*1024)
	for i := 0; i < 200; i++ {
		_ = tc.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err := tc.Write(buf); err != nil {
			break // buffer full / deadline hit — expected
		}
	}
	_ = tc.SetWriteDeadline(time.Time{})

	n, ok := outstandingSendBytes(tc)
	if !ok {
		t.Fatal("probe returned ok=false after filling the send queue")
	}
	if n <= 0 {
		t.Fatalf("expected a backed-up send queue (>0), got %d", n)
	}
	c2 := <-accepted
	_ = c2.Close()
}
