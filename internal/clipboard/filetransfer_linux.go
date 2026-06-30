//go:build linux

package clipboard

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lucky-verma/mwb-linux/internal/protocol"
)

// MWB transfers files (and clipboard data > 1 MB) over a SEPARATE TCP connection
// on the base port (TcpPort = MessagePort-1), distinct from the input/control
// channel. The flow (reverse-engineered from PowerToys SocketStuff.cs /
// Clipboard.cs):
//
//	1. AES + IV-block exchange (identical to the control channel).
//	2. A 64-byte DATA header exchange (Type=Clipboard/ClipboardPush, Src, Des,
//	   PostAction, MachineName). No magic/checksum stamp — sent raw-encrypted.
//	3. A 1024-byte header: the UTF-16LE string "<dataSize>*<fileName>".
//	4. dataSize raw bytes of file content.
//
// The machine that copied broadcasts a Clipboard beat; the machine that wants
// the data connects to the copier's base port and pulls it.

const (
	clipHeaderSize = 1024
	maxFileSize    = 100 * 1024 * 1024 // match MWB's 100 MB cap
	clipDialQuiet  = 10 * time.Second
	clipXferQuiet  = 5 * time.Minute
)

// clipHandshake runs the clipboard-channel handshake on conn and returns the
// encrypted writer / decrypted reader for the transfer. push selects our header
// type: true=ClipboardPush (we will send), false=Clipboard (we will receive).
func clipHandshake(conn net.Conn, key string, machineID uint32, name string, push bool) (*protocol.EncryptWriter, *protocol.DecryptReader, error) {
	aesKey := protocol.DeriveKey(key)
	iv := protocol.FixedIV()
	enc, err := protocol.NewEncryptWriter(conn, aesKey, iv)
	if err != nil {
		return nil, nil, err
	}
	dec, err := protocol.NewDecryptReader(conn, aesKey, iv)
	if err != nil {
		return nil, nil, err
	}

	// 1. send 16-byte random IV block (primes the CBC chain, like the control conn)
	ran := make([]byte, 16)
	if _, err := rand.Read(ran); err != nil {
		return nil, nil, err
	}
	if _, err := enc.Write(ran); err != nil {
		return nil, nil, fmt.Errorf("send IV block: %w", err)
	}

	// 2. send the 64-byte DATA header (raw, no magic stamp)
	hdr := make([]byte, 64)
	if push {
		hdr[0] = byte(protocol.ClipboardPush)
	} else {
		hdr[0] = byte(protocol.Clipboard)
	}
	binary.LittleEndian.PutUint32(hdr[8:12], machineID)       // Src
	binary.LittleEndian.PutUint32(hdr[12:16], protocol.IDAll) // Des
	binary.LittleEndian.PutUint32(hdr[16:20], 1)              // PostAction = Desktop
	for i := 32; i < 64; i++ {
		hdr[i] = ' '
	}
	copy(hdr[32:64], []byte(name)) // ASCII machine name, space-padded
	if _, err := enc.Write(hdr); err != nil {
		return nil, nil, fmt.Errorf("send header: %w", err)
	}

	// 3. read peer IV block, 4. read peer 64-byte header
	if _, err := io.ReadFull(dec, make([]byte, 16)); err != nil {
		return nil, nil, fmt.Errorf("read IV block: %w", err)
	}
	peer := make([]byte, 64)
	if _, err := io.ReadFull(dec, peer); err != nil {
		return nil, nil, fmt.Errorf("read peer header: %w", err)
	}
	if pt := protocol.PackageType(peer[0]); pt != protocol.Clipboard && pt != protocol.ClipboardPush {
		return nil, nil, fmt.Errorf("unexpected peer header type %d", pt)
	}
	return enc, dec, nil
}

// pullFile connects to the remote's clipboard port and receives a file, saving
// it under saveDir. Returns "" (no error) when the remote's clipboard is text or
// an image — those are delivered in-band on the control channel instead.
func pullFile(host string, basePort int, key string, machineID uint32, name, saveDir string) (string, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(basePort))
	conn, err := net.DialTimeout("tcp", addr, clipDialQuiet)
	if err != nil {
		return "", fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(clipXferQuiet))

	_, dec, err := clipHandshake(conn, key, machineID, name, false)
	if err != nil {
		return "", fmt.Errorf("handshake: %w", err)
	}

	hdr := make([]byte, clipHeaderSize)
	if _, err := io.ReadFull(dec, hdr); err != nil {
		return "", fmt.Errorf("read file header: %w", err)
	}
	headerStr := strings.TrimRight(decodeUTF16LE(hdr), "\x00")
	slog.Debug("clipboard file header", "header", headerStr)
	parts := strings.SplitN(headerStr, "*", 2)
	if len(parts) < 2 {
		return "", fmt.Errorf("bad header %q", headerStr)
	}
	size, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return "", fmt.Errorf("bad size %q", parts[0])
	}
	name0 := strings.ToLower(parts[1])
	if strings.HasPrefix(name0, "text") || strings.HasPrefix(name0, "image") {
		return "", nil // handled in-band
	}
	if size <= 0 || size > maxFileSize {
		return "", fmt.Errorf("file size out of range: %d", size)
	}

	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(saveDir, winBase(parts[1]))
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.CopyN(f, dec, size); err != nil {
		return "", fmt.Errorf("receive file body: %w", err)
	}
	slog.Info("received file from remote", "path", dest, "size", size)
	return dest, nil
}

// serveFiles listens on the base port and streams the current local file to any
// peer that connects to pull it (the copier-serves, paster-pulls model).
func (m *Manager) serveFiles() {
	addr := fmt.Sprintf(":%d", m.basePort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("clipboard file server listen failed", "port", m.basePort, "err", err)
		return
	}
	slog.Info("clipboard file server listening", "port", m.basePort)
	go func() {
		<-m.stopCh
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-m.stopCh:
				return
			default:
				continue
			}
		}
		go m.serveFileConn(conn)
	}
}

func (m *Manager) serveFileConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(clipXferQuiet))

	// We send (push=true matches MWB's server side); the puller drives the flow.
	enc, _, err := clipHandshake(conn, m.key, m.conn.MachineID, m.conn.LocalName, true)
	if err != nil {
		slog.Debug("clipboard serve handshake failed", "err", err)
		return
	}

	m.mu.Lock()
	path := m.localFile
	m.mu.Unlock()
	if path == "" {
		slog.Debug("clipboard serve: no local file to send")
		return
	}
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		slog.Debug("clipboard serve: file unavailable", "path", path, "err", err)
		return
	}

	header := make([]byte, clipHeaderSize)
	copy(header, encodeUTF16LE(fmt.Sprintf("%d*%s", fi.Size(), path)))
	if _, err := enc.Write(header); err != nil {
		slog.Debug("clipboard serve: write header failed", "err", err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	n, err := io.Copy(enc, f)
	if err != nil {
		slog.Debug("clipboard serve: send body failed", "sent", n, "err", err)
		return
	}
	slog.Info("served file to remote", "path", path, "size", n)
}

// onWayland reports whether to use wl-clipboard instead of xclip.
func onWayland() bool { return os.Getenv("WAYLAND_DISPLAY") != "" }

// getLocalClipboardFile returns the path of a single local file currently on the
// clipboard (as a text/uri-list file:// entry), or "" if there isn't one.
func (m *Manager) getLocalClipboardFile() string {
	var listCmd, getCmd []string
	if onWayland() {
		listCmd = []string{"wl-paste", "--list-types"}
		getCmd = []string{"wl-paste", "--no-newline", "--type", "text/uri-list"}
	} else {
		listCmd = []string{"xclip", "-selection", "clipboard", "-t", "TARGETS", "-o"}
		getCmd = []string{"xclip", "-selection", "clipboard", "-t", "text/uri-list", "-o"}
	}
	types, err := m.runClip(listCmd)
	if err != nil || !strings.Contains(string(types), "text/uri-list") {
		return ""
	}
	out, err := m.runClip(getCmd)
	if err != nil {
		return ""
	}
	for _, line := range strings.FieldsFunc(string(out), func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "file://") {
			continue
		}
		p := uriToPath(line)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// setLocalFileClipboard puts a single file on the local clipboard as a
// text/uri-list entry so file managers can paste it.
func (m *Manager) setLocalFileClipboard(path string) {
	uri := "file://" + pathToURI(path)
	var args []string
	if onWayland() {
		args = []string{"wl-copy", "--type", "text/uri-list"}
	} else {
		args = []string{"xclip", "-selection", "clipboard", "-t", "text/uri-list"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	if !onWayland() {
		cmd.Env = append(os.Environ(), "DISPLAY="+m.display)
	}
	cmd.Stdin = strings.NewReader(uri + "\n")
	if err := cmd.Run(); err != nil {
		slog.Warn("set file clipboard failed", "err", err)
		return
	}
	slog.Info("file placed on local clipboard", "path", path)
}

func (m *Manager) runClip(args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	if !onWayland() {
		cmd.Env = append(os.Environ(), "DISPLAY="+m.display)
	}
	return cmd.Output()
}

// uriToPath converts a file:// URI to a local path (decoding %20 etc.).
func uriToPath(uri string) string {
	p := strings.TrimPrefix(uri, "file://")
	// file:///path has an empty host; drop the leading authority slash if doubled.
	if strings.HasPrefix(p, "/") {
		// ok
	}
	p = strings.ReplaceAll(p, "%20", " ")
	return p
}

// pathToURI percent-encodes spaces in a path for a file:// URI.
func pathToURI(path string) string {
	return strings.ReplaceAll(path, " ", "%20")
}

// winBase returns the final path element, splitting on both Windows and Unix
// separators (the remote name is typically a full Windows path).
func winBase(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	return filepath.Base(p)
}
