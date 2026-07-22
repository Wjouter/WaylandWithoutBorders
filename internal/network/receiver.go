// internal/network/receiver.go
package network

import (
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/lucky-verma/mwb-linux/internal/protocol"
)

// livenessTimeout is how long the peer may go completely silent before we treat
// the link as dead. A healthy Windows MWB sends a heartbeat every ~5s (plus a
// reply to ours), so three missed intervals means the peer stopped reading and
// responding — a half-open zombie that TCP still reports as ESTAB. Closing it
// unblocks RecvPacket so the main loop reconnects.
const livenessTimeout = 15 * time.Second

// startHeartbeat sends periodic heartbeats to keep the connection alive.
// Windows MWB drops clients that don't send heartbeats within ~10s.
func startHeartbeat(conn *Conn, stop chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if d := conn.silentFor(time.Now()); d > livenessTimeout {
				// Peer went silent: no heartbeat reply, no data. The send buffer
				// backs up (Send-Q grows) but SendPacket won't error for minutes,
				// so detect death via the receive side and force a reconnect.
				// ponytail: if heavy outbound streaming ever blocks SendPacket
				// before this fires, add a write deadline in SendPacket too.
				slog.Warn("peer silent past liveness timeout, closing connection to force reconnect",
					"silent_for", d.Round(time.Second))
				_ = conn.Close()
				return
			}
			hb := &protocol.Packet{
				Type: protocol.HeartbeatEx,
				Src:  conn.MachineID,
				Des:  protocol.IDAll,
			}
			hb.SetMachineName(conn.LocalName)
			if err := conn.SendPacket(hb); err != nil {
				// A failed heartbeat means the link is down (interface gone,
				// peer unreachable). Close the connection so the blocked
				// RecvPacket in ReceiveLoop returns and the main loop can
				// reconnect — otherwise we'd sit in a hung read indefinitely.
				slog.Warn("heartbeat send failed, closing connection to force reconnect", "err", err)
				_ = conn.Close()
				return
			}
		}
	}
}

// ReceiveLoop reads packets from the connection and dispatches them.
func ReceiveLoop(conn *Conn, handler *Handler) error {
	stop := make(chan struct{})
	go startHeartbeat(conn, stop)
	defer close(stop)

	for {
		pkt, err := conn.RecvPacket()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				slog.Info("connection closed by remote")
				return nil
			}
			return err
		}

		switch pkt.Type {
		case protocol.Hi:
			slog.Debug("Hi received, responding with Hello", "localName", conn.LocalName, "machineID", conn.MachineID)
			resp := &protocol.Packet{
				Type: protocol.Hello,
				Src:  conn.MachineID,
				Des:  pkt.Src,
			}
			resp.SetMachineName(conn.LocalName)
			if err := conn.SendPacket(resp); err != nil {
				slog.Error("send Hello response", "err", err)
			}
		case protocol.Heartbeat, protocol.HeartbeatEx, protocol.HeartbeatExL2, protocol.HeartbeatExL3:
			slog.Debug("heartbeat received", "type", pkt.Type, "from", pkt.MachineName())
			resp := &protocol.Packet{
				Type: pkt.Type,
				Src:  conn.MachineID,
				Des:  pkt.Src,
			}
			resp.SetMachineName(conn.LocalName)
			if err := conn.SendPacket(resp); err != nil {
				slog.Error("send heartbeat response", "err", err)
			}
		case protocol.ByeBye:
			slog.Info("remote disconnected (ByeBye)")
			return nil
		case protocol.Invalid:
			slog.Warn("invalid packet received")
		case protocol.Handshake:
			slog.Debug("late handshake packet, ignoring")
		default:
			// Handle Matrix packets (bit 128 set) — server sends these for machine layout
			if pkt.Type&protocol.Matrix == protocol.Matrix {
				subType := pkt.Type &^ protocol.Matrix
				slog.Debug("matrix packet received", "fullType", pkt.Type, "subType", subType, "src", pkt.Src)
				if subType == protocol.Hi {
					resp := &protocol.Packet{
						Type: protocol.Hello,
						Src:  conn.MachineID,
						Des:  pkt.Src,
					}
					resp.SetMachineName(conn.LocalName)
					if err := conn.SendPacket(resp); err != nil {
						slog.Error("send matrix Hello response", "err", err)
					}
				}
			} else {
				handler.HandlePacket(pkt)
			}
		}
	}
}
