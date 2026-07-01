//go:build linux

// Package clipboard implements MWB clipboard sharing between Linux and Windows.
// Text clipboard data is UTF-16 encoded, Deflate compressed, and sent in 48-byte
// chunks as ClipboardText (124) packets, terminated by ClipboardDataEnd (76).
package clipboard

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lucky-verma/mwb-linux/internal/network"
	"github.com/lucky-verma/mwb-linux/internal/protocol"
)

const (
	dataSize      = 48 // bytes of clipboard data per 64-byte packet
	pollInterval  = 1 * time.Second
	execTimeout   = 5 * time.Second // max time for any xclip/xsel call
	textTypeSep   = "{4CFF57F7-BEDD-43d5-AE8F-27A61E886F2F}"
	maxInlineSize = 1048576     // 1 MB — max for inline TCP send
	maxRecvBuf    = 2 * 1048576 // 2 MB — max in-flight clipboard receive buffer
)

// Manager handles clipboard synchronization.
type Manager struct {
	conn        *network.Conn
	display     string
	key         string // security key — for the separate file-transfer channel
	host        string // remote host — to pull files from
	basePort    int    // file-transfer port (MessagePort-1)
	lastHash    string // hash of last clipboard content we sent
	localFile   string // local file currently on the clipboard, offered to peers
	mu          sync.Mutex
	recvBuf     bytes.Buffer // accumulates incoming clipboard chunks
	receiving   bool
	recvIsImage bool
	justSet     time.Time // when we last set clipboard from remote — suppress re-send
	stopCh      chan struct{}
	wg          sync.WaitGroup // tracks pollClipboard goroutine for clean shutdown
}

// NewManager creates a clipboard manager. key/host/basePort enable file transfer
// over the separate MWB clipboard channel; pass an empty key to disable it.
func NewManager(conn *network.Conn, display, key, host string, basePort int) *Manager {
	return &Manager{
		conn:     conn,
		display:  display,
		key:      key,
		host:     host,
		basePort: basePort,
		stopCh:   make(chan struct{}),
	}
}

// Start begins monitoring the local clipboard for changes.
func (m *Manager) Start() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.pollClipboard()
	}()
	if m.key != "" {
		go m.serveFiles() // serve files to peers that pull from us
	}
	slog.Info("clipboard sharing enabled")
}

// Stop stops clipboard monitoring and waits for the goroutine to exit.
func (m *Manager) Stop() {
	close(m.stopCh)
	m.wg.Wait()
}

// HandlePacket processes incoming clipboard packets.
func (m *Manager) HandlePacket(pkt *protocol.Packet) {
	switch pkt.Type {
	case protocol.ClipboardText, protocol.ClipboardImage:
		m.handleChunk(pkt)
	case protocol.ClipboardDataEnd:
		m.handleEnd(pkt)
	case protocol.Clipboard:
		slog.Debug("clipboard beat received from remote", "src", pkt.Src, "len", len(pkt.Raw))
		// MWB is a pull model: the beat says "my clipboard changed". Pull it over
		// the file-transfer channel — files are saved + put on our clipboard;
		// text/image come in-band so the pull skips them. Skip if we just set the
		// clipboard ourselves, to avoid a loop.
		m.mu.Lock()
		recentlySet := time.Since(m.justSet) < 3*time.Second
		m.mu.Unlock()
		if !recentlySet && m.key != "" {
			go m.pullRemoteFile()
		}
	case protocol.ClipboardAsk:
		// The remote wants our clipboard. For a file, push it to the asker's
		// file-transfer port (MWB's owner-pushes-on-ask model). Otherwise fall
		// back to the in-band text path.
		slog.Debug("clipboard ask received", "src", pkt.Src, "des", pkt.Des)
		if pkt.Des != m.conn.MachineID {
			break // ask addressed to a different machine
		}
		m.mu.Lock()
		path := m.localFile
		m.mu.Unlock()
		if path != "" && m.key != "" && m.host != "" {
			go m.pushFile(m.host, path)
		} else {
			go m.sendClipboard()
		}
	case protocol.ClipboardPush:
		slog.Debug("clipboard push received", "len", len(pkt.Raw), "hex", hex.EncodeToString(pkt.Raw))
	default:
		slog.Debug("unhandled clipboard packet", "type", pkt.Type, "len", len(pkt.Raw), "hex", hex.EncodeToString(pkt.Raw))
	}
}

// pollClipboard monitors the local clipboard for changes.
func (m *Manager) pollClipboard() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			// Don't re-send clipboard we just received from remote
			m.mu.Lock()
			recentlySet := time.Since(m.justSet) < 3*time.Second
			m.mu.Unlock()
			if recentlySet {
				continue
			}

			// Check for a file on the clipboard first — beat so peers can pull it
			// over the file-transfer channel (file bytes don't go in-band).
			if m.key != "" {
				if path := m.getLocalClipboardFile(); path != "" {
					hash := "file:" + path
					m.mu.Lock()
					changed := hash != m.lastHash
					if changed {
						m.lastHash = hash
						m.localFile = path
					}
					m.mu.Unlock()
					if changed {
						slog.Info("file on clipboard, notifying remote", "path", path)
						m.sendBeat()
					}
					continue
				}
			}

			// Check for image clipboard first
			imgData := m.getLocalImageClipboard()
			if imgData != nil {
				hash := fmt.Sprintf("img:%d", len(imgData))
				m.mu.Lock()
				changed := hash != m.lastHash
				if changed {
					m.lastHash = hash
				}
				m.mu.Unlock()
				if changed {
					slog.Info("image clipboard changed, sending to remote", "size", len(imgData))
					m.wg.Add(1)
					go func(d []byte) {
						defer m.wg.Done()
						m.sendImage(d)
					}(imgData)
				}
				continue
			}

			// Check for text clipboard
			text := m.getLocalClipboard()
			if text == "" {
				continue
			}
			hash := fmt.Sprintf("%d:%s", len(text), text[:min(100, len(text))])
			m.mu.Lock()
			changed := hash != m.lastHash
			if changed {
				m.lastHash = hash
			}
			m.mu.Unlock()

			if changed {
				slog.Info("clipboard changed, sending to remote", "len", len(text))
				m.wg.Add(1)
				go func(t string) {
					defer m.wg.Done()
					m.sendText(t)
				}(text)
			}
		}
	}
}

// pullRemoteFile pulls the remote clipboard over the file-transfer channel and,
// if it's a file, saves it and puts it on the local clipboard.
func (m *Manager) pullRemoteFile() {
	if m.host == "" {
		return
	}
	path, err := pullFile(m.host, m.basePort, m.key, m.conn.MachineID, m.conn.LocalName, fileSaveDir())
	if err != nil {
		slog.Debug("clipboard file pull failed", "err", err)
		return
	}
	if path == "" {
		return // text/image — handled in-band
	}
	m.mu.Lock()
	m.justSet = time.Now()
	m.localFile = path
	m.lastHash = "file:" + path // don't re-beat the file we just received
	m.mu.Unlock()
	m.setLocalFileClipboard(path)
}

// sendBeat notifies peers that our clipboard changed (pull model). Peers then
// pull the data from our file-transfer server.
func (m *Manager) sendBeat() {
	pkt := &protocol.Packet{Type: protocol.Clipboard, Src: m.conn.MachineID, Des: protocol.IDAll}
	pkt.SetMachineName(m.conn.LocalName)
	if err := m.conn.SendPacket(pkt); err != nil {
		slog.Debug("send clipboard beat failed", "err", err)
	}
}

// fileSaveDir is where pulled files are written.
func fileSaveDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads", "MouseWithoutBorders")
}

// sendClipboard sends the current clipboard to the remote.
func (m *Manager) sendClipboard() {
	text := m.getLocalClipboard()
	if text != "" {
		m.sendText(text)
	}
}

// sendText sends text to the remote via ClipboardText packets.
func (m *Manager) sendText(text string) {
	// Prepend format marker: "TXT" + text
	// MWB uses multi-format with GUID separator, but for simplicity we just send TXT
	markedText := "TXT" + text
	utf16 := encodeUTF16LE(markedText)

	// Deflate compress
	compressed, err := deflateCompress(utf16)
	if err != nil {
		slog.Error("clipboard compress failed", "err", err)
		return
	}

	if len(compressed) > maxInlineSize {
		slog.Warn("clipboard too large for inline send", "size", len(compressed))
		return
	}

	// Chunk into 48-byte packets
	for offset := 0; offset < len(compressed); offset += dataSize {
		end := offset + dataSize
		if end > len(compressed) {
			end = len(compressed)
		}
		chunk := compressed[offset:end]

		pkt := &protocol.Packet{
			Type: protocol.ClipboardText,
			Src:  m.conn.MachineID,
			Des:  protocol.IDAll,
		}
		// Copy chunk into packet payload (bytes 16-63)
		// We need to set the raw bytes — use Mouse fields as overlay
		// The packet Marshal will handle this via the ClipboardText case
		pkt.ClipboardData = make([]byte, dataSize)
		copy(pkt.ClipboardData, chunk)

		if err := m.conn.SendPacket(pkt); err != nil {
			slog.Error("send clipboard chunk failed", "err", err)
			return
		}
	}

	// Send end marker
	endPkt := &protocol.Packet{
		Type: protocol.ClipboardDataEnd,
		Src:  m.conn.MachineID,
		Des:  protocol.IDAll,
	}
	endPkt.ClipboardData = make([]byte, dataSize)
	if err := m.conn.SendPacket(endPkt); err != nil {
		slog.Error("send clipboard end failed", "err", err)
	}

	slog.Info("clipboard sent to remote", "chunks", (len(compressed)+dataSize-1)/dataSize)
}

// handleChunk accumulates a clipboard data chunk.
func (m *Manager) handleChunk(pkt *protocol.Packet) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.receiving {
		m.recvBuf.Reset()
		m.receiving = true
		m.recvIsImage = (pkt.Type == protocol.ClipboardImage)
	}
	if pkt.ClipboardData != nil {
		if m.recvBuf.Len()+len(pkt.ClipboardData) > maxRecvBuf {
			slog.Warn("clipboard receive buffer exceeded limit, dropping stream", "limit", maxRecvBuf)
			m.recvBuf.Reset()
			m.receiving = false
			return
		}
		m.recvBuf.Write(pkt.ClipboardData)
	}
}

// handleEnd processes the complete clipboard data.
func (m *Manager) handleEnd(pkt *protocol.Packet) {
	m.mu.Lock()
	data := make([]byte, m.recvBuf.Len())
	copy(data, m.recvBuf.Bytes())
	isImage := m.recvIsImage
	m.recvBuf.Reset()
	m.receiving = false
	m.mu.Unlock()

	if len(data) == 0 {
		return
	}

	if isImage {
		// Try decompress first, fall back to raw data
		decompressed, err := deflateDecompress(data)
		if err != nil {
			slog.Info("image clipboard not deflate-compressed, using raw data", "dataLen", len(data))
			m.handleImageClipboard(data)
		} else {
			m.handleImageClipboard(decompressed)
		}
		return
	}

	// Text clipboard — always Deflate compressed
	decompressed, err := deflateDecompress(data)
	if err != nil {
		slog.Error("clipboard decompress failed", "err", err, "dataLen", len(data))
		return
	}

	// Decode UTF-16LE to string
	text := decodeUTF16LE(decompressed)

	// Parse multi-format: split on TEXT_TYPE_SEP, find TXT section
	parts := strings.Split(text, textTypeSep)
	plainText := ""
	for _, part := range parts {
		if strings.HasPrefix(part, "TXT") {
			plainText = strings.TrimPrefix(part, "TXT")
			break
		}
	}
	if plainText == "" && len(parts) > 0 {
		plainText = text
	}

	if plainText == "" {
		return
	}

	// Update our hash so we don't re-send what we just received
	hash := fmt.Sprintf("%d:%s", len(plainText), plainText[:min(100, len(plainText))])
	m.mu.Lock()
	m.lastHash = hash
	m.mu.Unlock()

	// Set local clipboard
	m.setLocalClipboard(plainText)
	m.mu.Lock()
	m.justSet = time.Now()
	m.mu.Unlock()
	slog.Info("clipboard text received from remote", "len", len(plainText))
}

// handleImageClipboard processes received image data and sets it to clipboard.
func (m *Manager) handleImageClipboard(data []byte) {
	slog.Info("processing image clipboard", "rawSize", len(data))

	// MWB may send raw BMP data — detect by header
	// BMP starts with "BM", PNG starts with 0x89504E47
	imgData := data
	mimeType := "image/bmp"

	if len(data) > 4 {
		if data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
			mimeType = "image/png"
		} else if data[0] == 'B' && data[1] == 'M' {
			mimeType = "image/bmp"
		} else {
			// Might be raw DIB (no BM header) — add BMP header
			slog.Info("image data doesn't have known header, trying as raw DIB",
				"first4", fmt.Sprintf("%02x %02x %02x %02x", data[0], data[1], data[2], data[3]))
			mimeType = "image/bmp"
		}
	}

	// Write to temp file
	ext := ".bmp"
	if mimeType == "image/png" {
		ext = ".png"
	}
	tmpFile := "/tmp/mwb-clipboard-image" + ext
	if err := os.WriteFile(tmpFile, imgData, 0644); err != nil {
		slog.Error("write clipboard image failed", "err", err)
		return
	}

	// Set image clipboard: wl-copy on Wayland, xclip on X11.
	var setCmd []string
	if onWayland() {
		setCmd = []string{"wl-copy", "--type", mimeType}
	} else {
		setCmd = []string{"xclip", "-selection", "clipboard", "-t", mimeType, "-i", tmpFile}
	}
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	cmd := exec.CommandContext(ctx, setCmd[0], setCmd[1:]...)
	if onWayland() {
		f, _ := os.Open(tmpFile)
		defer f.Close() //nolint:errcheck
		cmd.Stdin = f
	} else {
		cmd.Env = append(os.Environ(), "DISPLAY="+m.display)
	}
	err := cmd.Run()
	cancel()
	if err != nil {
		slog.Error("set image clipboard failed", "err", err, "mime", mimeType, "wayland", onWayland())
		return
	}

	m.mu.Lock()
	m.justSet = time.Now()
	// Also update lastHash so pollClipboard doesn't re-send after the 3s suppress
	// window expires — without this, the same image echoes back to Windows.
	m.lastHash = fmt.Sprintf("img:%d", len(data))
	m.mu.Unlock()
	slog.Info("clipboard image received from remote", "size", len(data), "mime", mimeType)
}

// getLocalClipboard reads the current clipboard text.
// On Wayland, xclip reads a stale XWayland clipboard, so use wl-paste there.
// Times out after execTimeout to prevent blocking the poll goroutine indefinitely.
func (m *Manager) getLocalClipboard() string {
	cmds := [][]string{
		{"xclip", "-selection", "clipboard", "-o"},
		{"xsel", "--clipboard", "--output"},
	}
	if onWayland() {
		cmds = [][]string{{"wl-paste", "--no-newline"}}
	}
	for _, args := range cmds {
		if out, err := m.runClip(args); err == nil {
			return string(out)
		}
	}
	return ""
}

// setLocalClipboard sets the clipboard text.
// Times out after execTimeout to prevent blocking on a hung xclip/xsel.
func (m *Manager) setLocalClipboard(text string) {
	cmds := [][]string{
		{"xclip", "-selection", "clipboard"},
		{"xsel", "--clipboard", "--input"},
	}
	if onWayland() {
		cmds = [][]string{{"wl-copy"}}
	}
	for _, args := range cmds {
		ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		if !onWayland() {
			cmd.Env = append(os.Environ(), "DISPLAY="+m.display)
		}
		cmd.Stdin = strings.NewReader(text)
		err := cmd.Run()
		cancel()
		if err == nil {
			return
		}
	}
	slog.Error("set clipboard failed", "wayland", onWayland())
}

// getLocalImageClipboard checks if clipboard contains an image and returns it.
func (m *Manager) getLocalImageClipboard() []byte {
	listCmd := []string{"xclip", "-selection", "clipboard", "-t", "TARGETS", "-o"}
	getCmd := []string{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"}
	if onWayland() {
		listCmd = []string{"wl-paste", "--list-types"}
		getCmd = []string{"wl-paste", "--type", "image/png"}
	}
	out, err := m.runClip(listCmd)
	if err != nil || !strings.Contains(string(out), "image/png") {
		return nil
	}
	imgData, err := m.runClip(getCmd)
	if err != nil || len(imgData) == 0 {
		return nil
	}
	return imgData
}

// sendImage sends image data to the remote via ClipboardImage packets.
func (m *Manager) sendImage(data []byte) {
	if len(data) > maxInlineSize {
		slog.Warn("image too large for inline send", "size", len(data))
		return
	}

	// Chunk into 48-byte packets
	for offset := 0; offset < len(data); offset += dataSize {
		end := offset + dataSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[offset:end]

		pkt := &protocol.Packet{
			Type: protocol.ClipboardImage,
			Src:  m.conn.MachineID,
			Des:  protocol.IDAll,
		}
		pkt.ClipboardData = make([]byte, dataSize)
		copy(pkt.ClipboardData, chunk)

		if err := m.conn.SendPacket(pkt); err != nil {
			slog.Error("send image chunk failed", "err", err)
			return
		}
	}

	// End marker
	endPkt := &protocol.Packet{
		Type: protocol.ClipboardDataEnd,
		Src:  m.conn.MachineID,
		Des:  protocol.IDAll,
	}
	endPkt.ClipboardData = make([]byte, dataSize)
	if err := m.conn.SendPacket(endPkt); err != nil {
		slog.Error("send clipboard end failed", "err", err)
	}

	slog.Info("image clipboard sent to remote", "chunks", (len(data)+dataSize-1)/dataSize)
}

// encodeUTF16LE encodes a Go string to UTF-16LE bytes.
func encodeUTF16LE(s string) []byte {
	var buf bytes.Buffer
	for _, r := range s {
		if r > 0xFFFF {
			// Surrogate pair for supplementary characters
			r -= 0x10000
			hi := uint16(0xD800 + (r>>10)&0x3FF)
			lo := uint16(0xDC00 + r&0x3FF)
			_ = binary.Write(&buf, binary.LittleEndian, hi)
			_ = binary.Write(&buf, binary.LittleEndian, lo)
		} else {
			_ = binary.Write(&buf, binary.LittleEndian, uint16(r))
		}
	}
	return buf.Bytes()
}

// decodeUTF16LE decodes UTF-16LE bytes to a Go string.
func decodeUTF16LE(b []byte) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	var runes []rune
	for i := 0; i < len(b); i += 2 {
		u := uint16(b[i]) | uint16(b[i+1])<<8
		if u == 0 {
			break // null terminator
		}
		if u >= 0xD800 && u <= 0xDBFF && i+2 < len(b) {
			// High surrogate
			lo := uint16(b[i+2]) | uint16(b[i+3])<<8
			if lo >= 0xDC00 && lo <= 0xDFFF {
				r := rune((uint32(u)-0xD800)*0x400 + (uint32(lo) - 0xDC00) + 0x10000)
				runes = append(runes, r)
				i += 2
				continue
			}
		}
		runes = append(runes, rune(u))
	}
	return string(runes)
}

// deflateCompress compresses data using Deflate.
func deflateCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// deflateDecompress decompresses Deflate data.
func deflateDecompress(data []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(data))
	defer r.Close() //nolint:errcheck
	return io.ReadAll(r)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
