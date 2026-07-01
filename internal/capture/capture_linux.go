//go:build linux

// Package capture monitors the cursor position and evdev input events,
// forwarding them as MWB protocol packets when the cursor crosses a screen edge.
package capture

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lucky-verma/mwb-linux/internal/input"
	"github.com/lucky-verma/mwb-linux/internal/network"
	"github.com/lucky-verma/mwb-linux/internal/protocol"
)

const (
	evKey = 0x01
	evRel = 0x02

	relX      = 0x00
	relY      = 0x01
	relHWheel = 0x06
	relWheel  = 0x08

	inputEventSize = 24

	// The MWB wire protocol carries absolute cursor coordinates on a normalized
	// 0..normMax grid, so the receiver maps them to its own resolution. We track
	// the virtual remote cursor in that same normalized space — that's why no
	// remote screen resolution is needed (the Windows side auto-maps it).
	normMax = 65535

	// switchMargin is how far (in normalized units) inside the remote we enter
	// from, and how far you must move back before a return is armed. ~10% of the
	// range, matching the old 200px-of-1920 debounce.
	switchMargin = normMax / 10

	// Default speed multiplier on top of proportional (1:1 screen) mapping.
	// 2.0 ≈ traverse the remote in half a local-screen sweep, matching the old feel.
	defaultAccelMultiplier = 2.0
)

type inputEvent struct {
	Sec   int64
	Usec  int64
	Type  uint16
	Code  uint16
	Value int32
}

// ScreenInfo holds screen dimensions.
type ScreenInfo struct {
	Width  int32
	Height int32
}

// Capturer monitors input and forwards events to the remote MWB host.
type Capturer struct {
	conn              *network.Conn
	screen            ScreenInfo
	active            bool   // true = cursor is on this machine
	edgeSide          string // "left" or "right"
	mu                sync.Mutex
	stopCh            chan struct{}
	wg                sync.WaitGroup // tracks all goroutines for clean Stop()
	deviceFiles       []*os.File     // open /dev/input/event* fds — closed on Stop() to unblock f.Read
	lastSwitch        time.Time      // debounce outgoing switches
	switchSent        time.Time      // when we last sent switch packets
	lastActivated     time.Time      // when cursor last arrived on this machine
	remoteX           int32          // virtual cursor position on remote (normalized 0..normMax)
	remoteY           int32          // virtual cursor position on remote (normalized 0..normMax)
	accX              float64        // sub-pixel motion carry
	accY              float64        // sub-pixel motion carry
	activeEdge        string         // edge we crossed into the remote on: left/right/top/bottom
	accelMult         float64        // speed multiplier on the normalized mapping (config: accel_multiplier)
	edgeY             int32          // Y position where cursor left local screen
	canSwitch         bool           // true once cursor has been away from edge since activation
	canReturn         bool           // true once cursor has moved away from the remote return edge
	hotkeyCtrl        bool           // tracks Ctrl key state for hotkey detection
	hotkeyAlt         bool           // tracks Alt key state for hotkey detection
	disabledXinputIDs []int          // device IDs we disabled — re-enable same set to avoid TOCTOU

	// returnLocal, when set, replaces the X11 xdotool+xinput "return to local"
	// action. The Wayland portal driver sets this to call portal Release (which
	// both stops capture and warps the local cursor). nil on X11.
	returnLocal func(x, y int32)
}

// New creates a new input capturer.
// Does NOT call enableXinput — Stop() on the previous Capturer already
// re-enables any devices it disabled. Calling xinput enable unconditionally
// here can corrupt the attachment state of floating slave devices.
func New(conn *network.Conn, screen ScreenInfo, edgeSide string) *Capturer {
	return &Capturer{
		conn:       conn,
		screen:     screen,
		active:     true,
		edgeSide:   edgeSide,
		activeEdge: edgeSide, // X11 default; Wayland overrides per crossing
		stopCh:     make(chan struct{}),
		accelMult:  defaultAccelMultiplier,
		canSwitch:  true, // allow first switch immediately
	}
}

// SetActive sets whether this machine currently owns the cursor.
func (c *Capturer) SetActive(active bool) {
	c.mu.Lock()
	if c.active != active {
		slog.Info("cursor ownership changed", "active", active)
	}
	wasActive := c.active
	c.active = active
	shouldEnable := active && !wasActive
	if shouldEnable {
		c.switchSent = time.Time{}
		c.lastActivated = time.Now()
		c.canSwitch = false // must move away from local edge before next outbound switch
		c.canReturn = false // must move away from remote edge before next return switch
	}
	c.mu.Unlock()
	// enableXinput acquires c.mu internally — must be called after unlock.
	// Calling it under the lock caused a deadlock that froze all goroutines.
	if shouldEnable {
		c.enableXinput()
	}
}

// EnterRemoteEdge sets up the virtual-cursor state when control crosses to the
// remote host across the given local edge. perpFrac is the cursor position along
// that edge as a fraction (0..1) — Y for left/right, X for top/bottom. We enter
// switchMargin inside from the return edge so momentum doesn't bounce straight
// back; the perpendicular axis carries the crossing point.
func (c *Capturer) EnterRemoteEdge(edge string, perpFrac float64) {
	if perpFrac < 0 {
		perpFrac = 0
	} else if perpFrac > 1 {
		perpFrac = 1
	}
	perp := int32(perpFrac * normMax)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = false
	c.switchSent = time.Now()
	c.activeEdge = edge
	switch edge {
	case "right":
		c.remoteX, c.remoteY = switchMargin, perp
	case "top":
		c.remoteX, c.remoteY = perp, normMax-switchMargin
	case "bottom":
		c.remoteX, c.remoteY = perp, switchMargin
	default: // left
		c.remoteX, c.remoteY = normMax-switchMargin, perp
	}
	c.accX, c.accY = 0, 0
	c.canReturn = false
}

// EdgeReentry returns a local pixel position just inside the given edge,
// carrying the perpendicular fraction. Used to park the cursor when a modifier-
// gated switch is declined, so it stays on the local screen near where it hit.
func (c *Capturer) EdgeReentry(edge string, perpFrac float64) (x, y int32) {
	const inset = 12
	px := int32(perpFrac * float64(c.screen.Width))
	py := int32(perpFrac * float64(c.screen.Height))
	switch edge {
	case "right":
		return c.screen.Width - inset, py
	case "top":
		return px, inset
	case "bottom":
		return px, c.screen.Height - inset
	default: // left
		return inset, py
	}
}

// NotifyRemoteSwitch tells the remote host it now owns the cursor (the MWB
// MachineSwitched handoff). Besides the formal switch, this is what makes the
// remote pull our clipboard when we've just copied a file/large data (it checks
// for a recent clipboard beat on MachineSwitched). Call it right after we cross
// to the remote.
func (c *Capturer) NotifyRemoteSwitch() {
	pkt := &protocol.Packet{
		Type: protocol.MachineSwitched,
		Src:  c.conn.MachineID,
		Des:  c.conn.RemoteID,
	}
	if err := c.conn.SendPacket(pkt); err != nil {
		slog.Debug("send MachineSwitched failed", "err", err)
	}
}

// FeedEvent injects a normalized input event into the shared forwarding path.
// Used by the Wayland portal driver; the X11 path calls handleEvent directly
// from its evdev readers. Both funnel into the same handleRel/handleKey logic.
func (c *Capturer) FeedEvent(typ, code uint16, value int32) {
	c.handleEvent(inputEvent{Type: typ, Code: code, Value: value})
}

// IsActive returns true if cursor is on this machine.
func (c *Capturer) IsActive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

// SafeEntryPosition returns a cursor position 100px inside from the switch edge,
// safe to move to after MachineSwitched without immediately re-triggering the edge.
func (c *Capturer) SafeEntryPosition() (x, y int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	y = c.screen.Height / 2
	switch c.edgeSide {
	case "left":
		x = 100
	case "right":
		x = c.screen.Width - 100
	default:
		x = c.screen.Width / 2
	}
	return x, y
}

// SetAccelMultiplier sets the scaling applied to raw evdev deltas before they
// move the remote cursor. Non-positive values are ignored so the default is
// never clobbered by an unset/invalid config value.
func (c *Capturer) SetAccelMultiplier(m float64) {
	if m <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accelMult = m
	slog.Info("cursor acceleration multiplier set", "accel_multiplier", m)
}

// Stop signals the capturer to stop, waits for all goroutines to exit,
// and ensures xinput devices are always re-enabled on teardown.
func (c *Capturer) Stop() {
	close(c.stopCh)
	// Close all device fds to unblock any goroutines stuck in f.Read().
	// Without this, monitorDevice goroutines block indefinitely and accumulate
	// across reconnect cycles (35 devices × N reconnects = goroutine storm).
	c.mu.Lock()
	for _, f := range c.deviceFiles {
		_ = f.Close()
	}
	c.mu.Unlock()
	c.wg.Wait()
	// Only re-enable if WE disabled them — avoids calling xinput enable on
	// floating/unmanaged devices which can corrupt their attachment state.
	c.mu.Lock()
	hasDisabled := len(c.disabledXinputIDs) > 0
	c.mu.Unlock()
	if hasDisabled {
		c.enableXinput()
	}
}

// Run starts edge detection polling and evdev monitoring.
// Validates all preconditions before starting any goroutines.
func (c *Capturer) Run() error {
	devices, err := findInputDevices()
	if err != nil {
		return fmt.Errorf("find input devices: %w", err)
	}
	slog.Info("found input devices", "count", len(devices))

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.pollCursorEdge()
	}()
	for _, d := range devices {
		f, err := os.Open(d)
		if err != nil {
			continue
		}
		c.mu.Lock()
		c.deviceFiles = append(c.deviceFiles, f)
		c.mu.Unlock()
		c.wg.Add(1)
		go func(file *os.File) {
			defer c.wg.Done()
			c.monitorDevice(file)
		}(f)
	}
	return nil
}

// pollCursorEdge checks the actual cursor position and triggers switches.
func (c *Capturer) pollCursorEdge() {
	slog.Info("edge polling started", "edge", c.edgeSide, "screenWidth", c.screen.Width)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	errCount := 0
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			if !c.IsActive() {
				continue
			}
			c.mu.Lock()
			// canSwitch gate handles loop prevention — no time-based cooldown needed
			c.mu.Unlock()
			x, y, err := getCursorPosition()
			if err != nil {
				errCount++
				if errCount <= 3 {
					slog.Warn("getCursorPosition failed", "err", err, "count", errCount)
				}
				continue
			}
			if errCount > 0 {
				errCount = 0
			}

			// Track whether cursor has been away from the edge since activation
			// This prevents loops: cursor must move inward first, then back to edge
			c.mu.Lock()
			edgeZone := int32(20) // pixels from edge — must move this far inward to re-arm
			switch c.edgeSide {
			case "left":
				if x > edgeZone {
					c.canSwitch = true
				}
			case "right":
				if x < c.screen.Width-edgeZone {
					c.canSwitch = true
				}
			}
			canSwitch := c.canSwitch
			c.mu.Unlock()

			switched := false
			if canSwitch {
				switch c.edgeSide {
				case "left":
					if x <= 1 {
						switched = true
					}
				case "right":
					if x >= c.screen.Width-1 {
						switched = true
					}
				}
			}

			if switched {
				now := time.Now()
				if now.Sub(c.lastSwitch) < 100*time.Millisecond {
					continue
				}
				c.lastSwitch = now

				slog.Info("screen edge hit, switching to remote", "edge", c.edgeSide, "x", x, "y", y)

				// Map local Y to remote entry point (proportional)
				entryY := int32(float64(y) / float64(c.screen.Height) * 65535)
				// Enter 200px inside the remote screen, not at the literal edge.
				// Entering at exactly 0 or 65535 triggers Windows MWB's own edge
				// detection immediately, bouncing the cursor straight back.
				// 200px margin ≈ 200/1920 * 65535 ≈ 6826 units from the edge.
				const edgeMargin = int32(6826)
				entryX := edgeMargin // enter from left of remote, slightly inside
				if c.edgeSide == "left" {
					entryX = 65535 - edgeMargin // enter from right of remote, slightly inside
				}

				c.mu.Lock()
				c.active = false
				c.switchSent = time.Now()
				c.edgeY = y
				c.activeEdge = c.edgeSide
				c.accX, c.accY = 0, 0
				// Virtual cursor offset switchMargin from the return edge (normalized
				// 0..normMax space) to give mouse momentum room before a return arms.
				if c.edgeSide == "left" {
					c.remoteX = normMax - switchMargin
				} else {
					c.remoteX = switchMargin
				}
				c.remoteY = int32(float64(y) / float64(c.screen.Height) * normMax)
				c.canReturn = false // must move away from return edge first
				c.mu.Unlock()

				// Disable local input in X11 (synchronous — only takes ~2ms)
				c.disableXinput()

				// Formal handoff — also triggers the remote to pull our clipboard.
				c.NotifyRemoteSwitch()

				// Send mouse burst to the entry position on remote
				// Multiple packets help Windows MWB register the switch reliably
				conn := c.conn
				go func() {
					for i := 0; i < 5; i++ {
						mouse := &protocol.Packet{
							Type: protocol.Mouse,
							Src:  conn.MachineID,
							Des:  conn.RemoteID,
						}
						mouse.Mouse.X = entryX
						mouse.Mouse.Y = entryY
						mouse.Mouse.DwFlags = protocol.WM_MOUSEMOVE
						_ = conn.SendPacket(mouse)
						time.Sleep(5 * time.Millisecond)
					}
				}()
			}
		}
	}
}

var (
	displayOnce   sync.Once
	cachedDisplay string
)

// DetectDisplay finds the active X11 display and XAUTHORITY, caches the result,
// and sets XAUTHORITY in the process environment if missing.
// Detection order: DISPLAY env var → loginctl session query → X11 socket scan → ":0".
// Safe to call from multiple goroutines; detection runs exactly once.
func DetectDisplay() string {
	return getDisplay()
}

func getDisplay() string {
	displayOnce.Do(func() {
		detect()
	})
	return cachedDisplay
}

func detect() {

	// 1. Check environment variable (explicit override)
	d := os.Getenv("DISPLAY")

	// 2. Ask loginctl for the active graphical session's display
	if d == "" {
		d = detectDisplayFromLoginctl()
	}

	// 3. Scan X11 sockets as last resort
	if d == "" {
		d = detectDisplayFromSockets()
	}

	// 4. Final fallback
	if d == "" {
		d = ":0"
	}

	cachedDisplay = d
	// Set in process environment so all child commands (xrandr, xdotool, xinput, xclip) inherit it
	if err := os.Setenv("DISPLAY", d); err != nil {
		slog.Warn("failed to set DISPLAY env", "err", err)
	}
	slog.Info("X11 display detected", "display", d)

	// Also ensure XAUTHORITY is set — xdotool/xinput/xclip need it when running as root
	detectAndSetXauthority(d)
}

// detectDisplayFromLoginctl queries loginctl for an active X11 session.
func detectDisplayFromLoginctl() string {
	out, err := exec.Command("loginctl", "list-sessions", "--no-legend").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		sid := fields[0]
		display, err := exec.Command("loginctl", "show-session", sid, "-p", "Display", "--value").Output()
		if err != nil {
			continue
		}
		d := strings.TrimSpace(string(display))
		if d != "" {
			return d
		}
	}
	return ""
}

// detectDisplayFromSockets checks /tmp/.X11-unix/ for active X server sockets.
func detectDisplayFromSockets() string {
	entries, err := os.ReadDir("/tmp/.X11-unix")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "X") {
			return ":" + strings.TrimPrefix(name, "X")
		}
	}
	return ""
}

// detectAndSetXauthority finds the Xauthority file for the given display
// and sets XAUTHORITY in the process environment if not already set.
func detectAndSetXauthority(display string) {
	if os.Getenv("XAUTHORITY") != "" {
		return
	}
	// Common GDM/SDDM paths for UID 1000+ users
	entries, _ := os.ReadDir("/run/user")
	for _, e := range entries {
		// Try GDM path first, then generic .Xauthority
		candidates := []string{
			fmt.Sprintf("/run/user/%s/gdm/Xauthority", e.Name()),
			fmt.Sprintf("/run/user/%s/.Xauthority", e.Name()),
		}
		for _, path := range candidates {
			if _, err := os.Stat(path); err == nil {
				if err := os.Setenv("XAUTHORITY", path); err != nil {
					slog.Warn("failed to set XAUTHORITY env", "err", err)
				} else {
					slog.Info("XAUTHORITY auto-detected", "path", path)
				}
				return
			}
		}
	}
	// Try home directory fallback
	if home := os.Getenv("HOME"); home != "" {
		path := filepath.Join(home, ".Xauthority")
		if _, err := os.Stat(path); err == nil {
			if err := os.Setenv("XAUTHORITY", path); err != nil {
				slog.Warn("failed to set XAUTHORITY env", "err", err)
			} else {
				slog.Info("XAUTHORITY auto-detected", "path", path)
			}
		}
	}
}

func getCursorPosition() (x, y int32, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xdotool", "getmouselocation")
	cmd.Env = append(os.Environ(), "DISPLAY="+getDisplay())
	out, err := cmd.Output()
	if err != nil {
		return -1, -1, fmt.Errorf("xdotool: %w", err)
	}
	var ix, iy int
	if _, err = fmt.Sscanf(string(out), "x:%d y:%d", &ix, &iy); err != nil {
		// Return sentinel -1,-1 to distinguish parse failure from cursor at origin (0,0)
		return -1, -1, fmt.Errorf("xdotool parse: %w", err)
	}
	return int32(ix), int32(iy), nil
}

// getXinputIDs finds xinput device IDs for Razer/Wooting devices.
func getXinputIDs() []int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xinput", "list")
	cmd.Env = append(os.Environ(), "DISPLAY="+getDisplay())
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseXinputIDs(string(out))
}

// parseXinputIDs extracts IDs of attached (non-floating) Razer/Wooting devices
// from the output of `xinput list`. Separated from getXinputIDs for testability.
//
// Key invariant: NEVER include [floating slave] devices. xinput enable/disable
// on floating slaves corrupts their attachment state and leaves them unrecoverable
// without manual xinput reattach + xinput enable.
func parseXinputIDs(output string) []int {
	var ids []int
	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)
		// Skip floating slaves — they are not attached to a master device and
		// don't inject events into X11. Calling xinput disable/enable on them
		// detaches them permanently, requiring manual recovery.
		if strings.Contains(line, "[floating slave]") {
			continue
		}
		if strings.Contains(lower, "razer") || strings.Contains(lower, "wooting") {
			if idx := strings.Index(line, "id="); idx >= 0 {
				numStr := ""
				for _, ch := range line[idx+3:] {
					if ch >= '0' && ch <= '9' {
						numStr += string(ch)
					} else {
						break
					}
				}
				if id, err := strconv.Atoi(numStr); err == nil {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

// disableXinput disables Razer/Wooting devices and caches which IDs were disabled
// so enableXinput re-enables the exact same set (avoids TOCTOU if devices change).
func (c *Capturer) disableXinput() {
	ids := getXinputIDs()
	c.mu.Lock()
	c.disabledXinputIDs = ids
	c.mu.Unlock()
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, "xinput", "disable", strconv.Itoa(id))
		cmd.Env = append(os.Environ(), "DISPLAY="+getDisplay())
		_ = cmd.Run()
		cancel()
	}
	slog.Info("disabled Razer/Wooting xinput devices", "count", len(ids))
}

// enableXinput re-enables the exact device IDs that were disabled by disableXinput.
// Also scans for any Razer/Wooting devices that are attached-but-disabled from a
// previous broken session (e.g. disableXinput ran but enableXinput never did because
// the connection dropped). Only touches attached slaves — never floating devices.
func (c *Capturer) enableXinput() {
	c.mu.Lock()
	ids := c.disabledXinputIDs
	c.disabledXinputIDs = nil
	c.mu.Unlock()

	// Always include currently-disabled attached devices to recover from prior
	// broken sessions — idempotent for already-enabled devices.
	current := getXinputIDs()
	merged := make(map[int]struct{}, len(ids)+len(current))
	for _, id := range ids {
		merged[id] = struct{}{}
	}
	for _, id := range current {
		merged[id] = struct{}{}
	}

	for id := range merged {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, "xinput", "enable", strconv.Itoa(id))
		cmd.Env = append(os.Environ(), "DISPLAY="+getDisplay())
		_ = cmd.Run()
		cancel()
	}
	slog.Info("enabled Razer/Wooting xinput devices", "count", len(merged))
}

func findInputDevices() ([]string, error) {
	entries, err := os.ReadDir("/dev/input")
	if err != nil {
		return nil, err
	}
	var devices []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "event") {
			devices = append(devices, filepath.Join("/dev/input", e.Name()))
		}
	}
	return devices, nil
}

func (c *Capturer) monitorDevice(f *os.File) {
	defer f.Close() //nolint:errcheck
	slog.Debug("monitoring device", "path", f.Name())
	buf := make([]byte, inputEventSize*32)
	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		n, err := f.Read(buf)
		if err != nil {
			return
		}

		for off := 0; off+inputEventSize <= n; off += inputEventSize {
			ev := parseEvent(buf[off : off+inputEventSize])
			c.handleEvent(ev)
		}
	}
}

func parseEvent(buf []byte) inputEvent {
	return inputEvent{
		Sec:   int64(binary.LittleEndian.Uint64(buf[0:8])),
		Usec:  int64(binary.LittleEndian.Uint64(buf[8:16])),
		Type:  binary.LittleEndian.Uint16(buf[16:18]),
		Code:  binary.LittleEndian.Uint16(buf[18:20]),
		Value: int32(binary.LittleEndian.Uint32(buf[20:24])),
	}
}

func (c *Capturer) handleEvent(ev inputEvent) {
	if c.IsActive() {
		return
	}
	// Suppress during switch grace period
	c.mu.Lock()
	grace := !c.switchSent.IsZero() && time.Since(c.switchSent) < 100*time.Millisecond
	c.mu.Unlock()
	if grace {
		return
	}

	switch ev.Type {
	case evRel:
		c.handleRel(ev)
	case evKey:
		c.handleKey(ev)
	}
}

// pick returns down if cond else up — for choosing button DOWN/UP flags.
func pick(cond bool, down, up int32) int32 {
	if cond {
		return down
	}
	return up
}

func (c *Capturer) clampRemoteLocked() {
	if c.remoteX < 0 {
		c.remoteX = 0
	}
	if c.remoteX > normMax {
		c.remoteX = normMax
	}
	if c.remoteY < 0 {
		c.remoteY = 0
	}
	if c.remoteY > normMax {
		c.remoteY = normMax
	}
}

// normPerPx returns how many normalized units one local pixel maps to on each
// axis. Moving the full local screen width thus sweeps the full remote range
// (times accel_multiplier). Guards against a zero screen size.
func (c *Capturer) normPerPx() (x, y float64) {
	w, h := c.screen.Width, c.screen.Height
	if w <= 0 {
		w = 1
	}
	if h <= 0 {
		h = 1
	}
	return float64(normMax) / float64(w), float64(normMax) / float64(h)
}

// addMotionLocked accumulates already-scaled normalized deltas with sub-pixel
// carry and applies them to the virtual cursor. Shared by handleRel (X11,
// per-axis) and FeedMotion (Wayland, combined).
func (c *Capturer) addMotionLocked(dxNorm, dyNorm float64) {
	c.accX += dxNorm
	c.accY += dyNorm
	ix := math.Trunc(c.accX)
	iy := math.Trunc(c.accY)
	c.accX -= ix
	c.accY -= iy
	c.remoteX += int32(ix)
	c.remoteY += int32(iy)
	c.clampRemoteLocked()
}

func (c *Capturer) handleRel(ev inputEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	nppx, nppy := c.normPerPx()
	switch ev.Code {
	case relX:
		c.addMotionLocked(float64(ev.Value)*c.accelMult*nppx, 0)
	case relY:
		c.addMotionLocked(0, float64(ev.Value)*c.accelMult*nppy)
	case relWheel:
		// evdev REL_WHEEL is +1 per notch up; Windows WHEEL_DELTA is +120 up — same
		// direction. Send at the current cursor position so it scrolls the right window.
		c.sendMouseLocked(c.remoteX, c.remoteY, ev.Value*120, protocol.WM_MOUSEWHEEL)
		return
	case relHWheel:
		c.sendMouseLocked(c.remoteX, c.remoteY, ev.Value*120, protocol.WM_MOUSEHWHEEL)
		return
	default:
		return
	}
	c.afterMotionLocked()
}

// FeedWheel forwards a scroll to the remote at the current cursor position.
// Units are Windows WHEEL_DELTA (±120 per notch): vertical>0 scrolls up,
// horizontal>0 scrolls right. Used by the Wayland driver for wheel and touchpad.
func (c *Capturer) FeedWheel(vertical, horizontal int32) {
	if c.IsActive() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if vertical != 0 {
		c.sendMouseLocked(c.remoteX, c.remoteY, vertical, protocol.WM_MOUSEWHEEL)
	}
	if horizontal != 0 {
		c.sendMouseLocked(c.remoteX, c.remoteY, horizontal, protocol.WM_MOUSEHWHEEL)
	}
}

// FeedMotion applies a combined relative motion (both axes at once) and sends a
// single mouse packet. The EI/Wayland source delivers dx and dy together, so
// feeding them as one update avoids the per-axis double-send that stair-steps
// diagonal motion. Sub-pixel deltas are accumulated so slow movement stays smooth.
func (c *Capturer) FeedMotion(dx, dy float64) {
	if c.IsActive() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.switchSent.IsZero() && time.Since(c.switchSent) < 100*time.Millisecond {
		return
	}
	nppx, nppy := c.normPerPx()
	c.addMotionLocked(dx*c.accelMult*nppx, dy*c.accelMult*nppy)
	c.afterMotionLocked()
}

// afterMotionLocked evaluates the return-edge gate and forwards the new cursor
// position. Caller must hold c.mu. Shared by handleRel (per-axis, X11) and
// FeedMotion (combined, Wayland).
func (c *Capturer) afterMotionLocked() {
	// canReturn gate: must move switchMargin away from the entry edge before a
	// return is armed — prevents the initial momentum from bouncing straight back.
	// switchBack fires when the virtual cursor crosses back over that same edge.
	switchBack := false
	switch c.activeEdge {
	case "right":
		if c.remoteX > switchMargin {
			c.canReturn = true
		}
		switchBack = c.canReturn && c.remoteX <= 0
	case "top":
		if c.remoteY < normMax-switchMargin {
			c.canReturn = true
		}
		switchBack = c.canReturn && c.remoteY >= normMax-1
	case "bottom":
		if c.remoteY > switchMargin {
			c.canReturn = true
		}
		switchBack = c.canReturn && c.remoteY <= 0
	default: // left
		if c.remoteX < normMax-switchMargin {
			c.canReturn = true
		}
		switchBack = c.canReturn && c.remoteX >= normMax-1
	}

	// Log virtual position periodically for debugging
	if c.remoteX%2000 == 0 || switchBack {
		slog.Debug("virtual cursor", "x", c.remoteX, "y", c.remoteY, "edge", c.activeEdge, "switchBack", switchBack)
	}

	if switchBack {
		slog.Info("remote edge hit — switching back to local", "edge", c.activeEdge, "remoteX", c.remoteX, "remoteY", c.remoteY)
		entryX, entryY := c.localReentryLocked()
		c.active = true
		c.switchSent = time.Time{}
		c.lastActivated = time.Now()
		c.canSwitch = false // block re-trigger until cursor moves away from edge
		c.mu.Unlock()

		if c.returnLocal != nil {
			// Wayland: portal Release stops capture and warps the cursor.
			c.returnLocal(entryX, entryY)
		} else {
			// X11: warp the cursor away from the edge before re-enabling devices.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			cmd := exec.CommandContext(ctx, "xdotool", "mousemove", "--",
				fmt.Sprintf("%d", entryX),
				fmt.Sprintf("%d", entryY))
			cmd.Env = append(os.Environ(), "DISPLAY="+getDisplay())
			_ = cmd.Run()
			cancel()
			c.enableXinput()
		}
		c.mu.Lock()
		return
	}

	// remoteX/remoteY are already in the wire's 0..normMax space — send directly.
	c.sendMouseLocked(c.remoteX, c.remoteY, 0, protocol.WM_MOUSEMOVE)
}

// localReentryLocked maps the virtual cursor back to a local pixel position just
// inside the edge we originally left, carrying the perpendicular coordinate.
func (c *Capturer) localReentryLocked() (x, y int32) {
	const inset = 100
	px := int32(float64(c.remoteX) / normMax * float64(c.screen.Width))
	py := int32(float64(c.remoteY) / normMax * float64(c.screen.Height))
	switch c.activeEdge {
	case "right":
		return c.screen.Width - inset, py
	case "top":
		return px, inset
	case "bottom":
		return px, c.screen.Height - inset
	default: // left
		return inset, py
	}
}

func (c *Capturer) handleKey(ev inputEvent) {
	// Track Ctrl+Alt for hotkey — guarded by c.mu via handleEvent → monitorDevice path.
	// Left/right Ctrl (29, 97) and Left/right Alt (56, 100).
	if ev.Code == 29 || ev.Code == 97 {
		c.hotkeyCtrl = ev.Value == 1
	}
	if ev.Code == 56 || ev.Code == 100 {
		c.hotkeyAlt = ev.Value == 1
	}
	// Ctrl+Alt+Right = force return to Ubuntu
	if ev.Code == 106 && ev.Value == 1 && c.hotkeyCtrl && c.hotkeyAlt {
		if !c.IsActive() {
			slog.Info("hotkey Ctrl+Alt+Right: returning to Ubuntu")
			c.SetActive(true)
			return
		}
	}

	// Mouse buttons (left/right/middle + side buttons X1/X2 for back/forward).
	if ev.Code >= input.BTN_LEFT && ev.Code <= input.BTN_EXTRA {
		if !c.IsActive() {
			if ev.Value != 0 && ev.Value != 1 {
				return // ignore auto-repeat on buttons
			}
			down := ev.Value == 1
			var flags, wheel int32
			switch ev.Code {
			case input.BTN_LEFT:
				flags = pick(down, protocol.WM_LBUTTONDOWN, protocol.WM_LBUTTONUP)
			case input.BTN_RIGHT:
				flags = pick(down, protocol.WM_RBUTTONDOWN, protocol.WM_RBUTTONUP)
			case input.BTN_MIDDLE:
				flags = pick(down, protocol.WM_MBUTTONDOWN, protocol.WM_MBUTTONUP)
			case input.BTN_SIDE: // X1 (back)
				flags = pick(down, protocol.WM_XBUTTONDOWN, protocol.WM_XBUTTONUP)
				wheel = 0x0001 << 16 // XBUTTON1 in the mouseData high word
			case input.BTN_EXTRA: // X2 (forward)
				flags = pick(down, protocol.WM_XBUTTONDOWN, protocol.WM_XBUTTONUP)
				wheel = 0x0002 << 16 // XBUTTON2 in the mouseData high word
			default:
				return
			}
			// Send at the current virtual cursor position so the click lands where
			// the cursor is, not at (0,0). remoteX/remoteY are already in wire space.
			c.mu.Lock()
			absX, absY := c.remoteX, c.remoteY
			c.mu.Unlock()
			slog.Debug("forwarding mouse button", "code", ev.Code, "down", down, "flags", flags)
			c.sendMouse(absX, absY, wheel, flags)
		}
		return
	}

	// Keyboard
	if c.IsActive() {
		return
	}

	vk, ok := input.KeyCodeToVK(ev.Code)
	if !ok {
		return
	}

	// evdev value: 1 = press, 2 = auto-repeat, 0 = release. Forward repeats as
	// keydowns — an injected key does not auto-repeat on Windows, so a held key
	// only repeats if we resend the press (mirrors MWB's own keyboard hook,
	// which emits a WM_KEYDOWN per hardware repeat).
	var dwFlags int32
	if ev.Value == 0 {
		dwFlags = protocol.LLKHF_UP
	}

	pkt := &protocol.Packet{
		Type: protocol.Keyboard,
		Src:  c.conn.MachineID,
		Des:  c.conn.RemoteID,
	}
	pkt.Keyboard.WVk = vk
	pkt.Keyboard.DwFlags = dwFlags

	if err := c.conn.SendPacket(pkt); err != nil {
		slog.Debug("send keyboard failed", "err", err)
	}
}

func (c *Capturer) sendMouse(x, y, wheelDelta, flags int32) {
	pkt := &protocol.Packet{
		Type: protocol.Mouse,
		Src:  c.conn.MachineID,
		Des:  c.conn.RemoteID,
	}
	pkt.Mouse.X = x
	pkt.Mouse.Y = y
	pkt.Mouse.WheelDelta = wheelDelta
	pkt.Mouse.DwFlags = flags

	if err := c.conn.SendPacket(pkt); err != nil {
		slog.Debug("send mouse failed", "err", err)
	}
}

// sendMouseLocked sends a mouse packet (caller must hold c.mu).
func (c *Capturer) sendMouseLocked(x, y, wheelDelta, flags int32) {
	pkt := &protocol.Packet{
		Type: protocol.Mouse,
		Src:  c.conn.MachineID,
		Des:  c.conn.RemoteID,
	}
	pkt.Mouse.X = x
	pkt.Mouse.Y = y
	pkt.Mouse.WheelDelta = wheelDelta
	pkt.Mouse.DwFlags = flags

	if err := c.conn.SendPacket(pkt); err != nil {
		slog.Debug("send mouse failed", "err", err)
	}
}
