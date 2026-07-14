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
	"html"
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
	key         string     // security key — for the separate file-transfer channel
	host        string     // remote host — to pull files from
	basePort    int        // file-transfer port (MessagePort-1)
	lastHash    string     // hash of last clipboard content we sent
	localFile   string     // local file currently on the clipboard, offered to peers
	sendMu      sync.Mutex // serializes sendText/sendImage so their packet sequences never interleave on the wire
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
			m.mu.Unlock()
			if !changed {
				continue
			}

			// A rapidly-rewritten source (e.g. dragging a text selection in a
			// terminal, which can push the clipboard several times as the
			// selection grows) can be read mid-update. Confirm it's stable
			// before trusting and sending it; otherwise wait for a later tick
			// once it settles, rather than shipping a torn read.
			time.Sleep(150 * time.Millisecond)
			if m.getLocalClipboard() != text {
				continue
			}

			m.mu.Lock()
			m.lastHash = hash
			m.mu.Unlock()

			slog.Info("clipboard changed, sending to remote", "len", len(text))
			m.wg.Add(1)
			go func(t string) {
				defer m.wg.Done()
				m.sendText(t)
			}(text)
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

// clipTextPayload formats clipboard text the way the Windows MWB receiver
// expects: "TXT" + text + separator. The trailing GUID separator is required,
// not cosmetic. The receiver reads its whole decompress buffer, and .NET's
// DeflateStream leaves replayed window bytes just past the real data; the
// separator makes those bytes a trailing split segment MWB discards
// (Split(TEXT_TYPE_SEP, RemoveEmptyEntries)) rather than appending them to the
// pasted text. Without it, pasting yields the text with a garbage suffix. This
// is why MWB's own multi-format payloads stay clean.
func clipTextPayload(text string) string {
	return "TXT" + text + textTypeSep
}

// chunkPackets splits compressed clipboard data into 48-byte ClipboardText/
// ClipboardImage packets (last chunk zero-padded, matching MWB) followed by a
// ClipboardDataEnd marker — the full sequence the Windows receiver reads as one
// contiguous run.
func (m *Manager) chunkPackets(data []byte, dataType protocol.PackageType) []*protocol.Packet {
	var pkts []*protocol.Packet
	for offset := 0; offset < len(data); offset += dataSize {
		end := offset + dataSize
		if end > len(data) {
			end = len(data)
		}
		pkt := &protocol.Packet{Type: dataType, Src: m.conn.MachineID, Des: protocol.IDAll}
		pkt.ClipboardData = make([]byte, dataSize) // zero-padded to 48
		copy(pkt.ClipboardData, data[offset:end])
		pkts = append(pkts, pkt)
	}
	endPkt := &protocol.Packet{Type: protocol.ClipboardDataEnd, Src: m.conn.MachineID, Des: protocol.IDAll}
	endPkt.ClipboardData = make([]byte, dataSize)
	return append(pkts, endPkt)
}

// sendText sends text to the remote via ClipboardText packets. The chunk+end
// sequence is sent atomically (see Conn.SendPackets): any packet interleaved
// mid-sequence corrupts the result on the Windows side. Serialized against
// sendImage via sendMu so two clipboard messages can't overlap either.
func (m *Manager) sendText(text string) {
	m.sendMu.Lock()
	defer m.sendMu.Unlock()

	// Deflate compress
	compressed, err := deflateCompress(encodeUTF16LE(clipTextPayload(text)))
	if err != nil {
		slog.Error("clipboard compress failed", "err", err)
		return
	}

	if len(compressed) > maxInlineSize {
		slog.Warn("clipboard too large for inline send", "size", len(compressed))
		return
	}

	if err := m.conn.SendPackets(m.chunkPackets(compressed, protocol.ClipboardText)); err != nil {
		slog.Error("send clipboard text failed", "err", err)
		return
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

	// Decode UTF-16LE, then extract plain text from the multi-format payload.
	plainText := extractClipboardText(decodeUTF16LE(decompressed))
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
		// --type text/plain is required: without it wl-copy runs xdg-mime on the
		// content to guess a type, and code-like text (e.g. "def ...") gets tagged
		// application/x-ruby and offered as *only* that. Apps requesting
		// text/plain(;charset=utf-8) — Kate, Chromium — then paste nothing.
		// Forcing text/plain makes wl-copy offer the full standard alias set.
		cmds = [][]string{{"wl-copy", "--type", "text/plain"}}
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
// Serialized against sendText — see its comment for why.
func (m *Manager) sendImage(data []byte) {
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	if len(data) > maxInlineSize {
		slog.Warn("image too large for inline send", "size", len(data))
		return
	}

	if err := m.conn.SendPackets(m.chunkPackets(data, protocol.ClipboardImage)); err != nil {
		slog.Error("send image failed", "err", err)
		return
	}
	slog.Info("image clipboard sent to remote", "chunks", (len(data)+dataSize-1)/dataSize)
}

// extractClipboardText pulls plain text out of MWB's multi-format clipboard
// payload: sections "TXT"/"RTF"/"HTM" each followed by textTypeSep (see the
// Windows sender in FormHelper.GetClipboardText). Plain text is preferred, but
// rich apps (IDEs) sometimes send only RTF+HTML with no TXT section — so fall
// back to stripping HTML, then RTF, rather than dumping the raw marked-up blob
// onto the clipboard.
func extractClipboardText(raw string) string {
	var txt, rtf, htm string
	seen := false
	for _, part := range strings.Split(raw, textTypeSep) {
		switch {
		case strings.HasPrefix(part, "TXT"):
			txt, seen = part[3:], true
		case strings.HasPrefix(part, "RTF"):
			rtf, seen = part[3:], true
		case strings.HasPrefix(part, "HTM"):
			htm, seen = part[3:], true
		}
	}
	if txt != "" {
		return txt
	}
	if htm != "" {
		if s := strings.TrimSpace(htmlToText(htm)); s != "" {
			return s
		}
	}
	if rtf != "" {
		if s := strings.TrimSpace(rtfToText(rtf)); s != "" {
			return s
		}
	}
	// No recognizable section marker at all: treat as bare plain text from a
	// simple sender. If markers were present but unextractable, return empty
	// rather than pasting the marked-up blob.
	if !seen {
		return raw
	}
	return ""
}

// htmlToText strips a CF_HTML section down to its text: drop the CF_HTML header
// (everything before the first tag), remove tags, and unescape entities.
func htmlToText(h string) string {
	if i := strings.IndexByte(h, '<'); i >= 0 {
		h = h[i:]
	}
	var b strings.Builder
	inTag := false
	for _, r := range h {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return html.UnescapeString(b.String())
}

// rtfToText does a minimal RTF strip: \par/\line become newlines, other control
// words and the font/color tables are dropped, leaving the literal characters.
// ponytail: a naive stripper, not a full RTF parser — good enough to recover the
// pasted text when only RTF is offered; HTML is the primary rich fallback above.
func rtfToText(s string) string {
	var b strings.Builder
	depth, skipGroup := 0, -1
	i := 0
	for i < len(s) {
		c := s[i]
		switch c {
		case '{':
			depth++
			i++
		case '}':
			if skipGroup == depth {
				skipGroup = -1
			}
			depth--
			i++
		case '\\':
			// Control word or symbol.
			j := i + 1
			for j < len(s) && ((s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z')) {
				j++
			}
			word := s[i+1 : j]
			// optional numeric parameter
			for j < len(s) && (s[j] == '-' || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			if j < len(s) && s[j] == ' ' {
				j++ // a single trailing space delimits the control word
			}
			switch word {
			case "par", "line":
				b.WriteByte('\n')
			case "tab":
				b.WriteByte('\t')
			case "fonttbl", "colortbl", "stylesheet", "pict", "object", "info":
				skipGroup = depth // skip this whole group's contents
			}
			if word == "" && i+1 < len(s) { // control symbol like \' or \~
				i += 2
				continue
			}
			i = j
		default:
			if skipGroup == -1 && c != '\r' && c != '\n' {
				b.WriteByte(c)
			}
			i++
		}
	}
	return b.String()
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
