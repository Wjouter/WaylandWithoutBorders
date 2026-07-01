//go:build linux && wayland

// Package capture — Wayland bidirectional driver via the
// org.freedesktop.portal.InputCapture portal + libei.
//
// The portal replaces the three X11-only pieces of the bidi path:
//   - pointer barriers at the screen edge → the Activated signal (edge detection)
//   - the portal suppresses local input while captured (xinput isolation)
//   - Release(session, cursor_position) stops capture and warps the cursor (xdotool)
//
// Input events arrive over the EI socket the portal hands back; we reuse the
// existing handleRel/handleKey forwarding logic via Capturer.FeedEvent.
package capture

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/lucky-verma/mwb-linux/internal/network"
)

const (
	portalDest = "org.freedesktop.portal.Desktop"
	portalPath = "/org/freedesktop/portal/desktop"
	icIface    = "org.freedesktop.portal.InputCapture"

	// InputCapture capability bitmask.
	capKeyboard = 1
	capPointer  = 2

	portalRequestTimeout = 60 * time.Second // first call may show a permission dialog

	// touchpadPxPerNotch converts smooth touchpad scroll pixels into wheel
	// notches (one WHEEL_DELTA per this many pixels of accumulated scroll).
	touchpadPxPerNotch = 30.0
)

// barrierPos is the (iiii) position struct the portal expects per barrier.
type barrierPos struct{ X1, Y1, X2, Y2 int32 }

// cursorPos is the (dd) tuple the portal expects for cursor_position.
type cursorPos struct{ X, Y float64 }

type portal struct {
	conn     *dbus.Conn
	obj      dbus.BusObject
	unique   string // our unique bus name, dot/colon-sanitized for request paths
	tokenSeq atomic.Uint64
	session  dbus.ObjectPath
	zoneSet  uint32
	screenW  int32
	screenH  int32
	zoneX    int32 // zone origin — barriers and cursor coords are in this space
	zoneY    int32
	activeID atomic.Uint32 // last activation_id from the portal
	cap      *Capturer
	ei       *eiConn
	scrAccX  float64 // touchpad scroll pixel accumulators → wheel notches
	scrAccY  float64
	wantMod  uint32        // required modifier mask to allow switching (0 = none)
	curMods  atomic.Uint32 // latest held-modifier mask from libei
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// RunWayland sets up the InputCapture portal + libei receiver and returns a
// running Capturer plus a stop function. The returned Capturer reuses the same
// forwarding logic as the X11 path; only the event source and switch triggers
// differ.
func RunWayland(conn *network.Conn, handler *network.Handler, edgeSide string, edges []string, switchModifier string, accel float64) (*Capturer, func(), error) {
	bus, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, nil, fmt.Errorf("connect session bus: %w", err)
	}
	p := &portal{
		conn:    bus,
		obj:     bus.Object(portalDest, portalPath),
		unique:  sanitizeName(bus.Names()[0]),
		wantMod: modMask(switchModifier),
		stopCh:  make(chan struct{}),
	}

	if err := p.createSession(); err != nil {
		_ = bus.Close()
		return nil, nil, err
	}
	if err := p.start(); err != nil {
		_ = bus.Close()
		return nil, nil, err
	}
	if err := p.getZones(); err != nil {
		_ = bus.Close()
		return nil, nil, err
	}

	p.cap = New(conn, ScreenInfo{Width: p.screenW, Height: p.screenH}, edgeSide)
	p.cap.SetAccelMultiplier(accel)
	// Wire the return-to-local action to portal Release (replaces xdotool+xinput).
	p.cap.returnLocal = func(x, y int32) { p.release(x, y) }

	if err := p.setBarriers(edges); err != nil {
		_ = bus.Close()
		return nil, nil, err
	}
	fd, err := p.connectToEIS()
	if err != nil {
		_ = bus.Close()
		return nil, nil, err
	}
	ei, err := newEIReceiver(fd, "mwb")
	if err != nil {
		_ = syscall.Close(fd)
		_ = bus.Close()
		return nil, nil, fmt.Errorf("libei receiver: %w", err)
	}
	p.ei = ei

	if err := p.subscribeSignals(); err != nil {
		ei.close()
		_ = bus.Close()
		return nil, nil, err
	}
	if err := p.enable(); err != nil {
		ei.close()
		_ = bus.Close()
		return nil, nil, err
	}

	// Server-driven returns (Windows bounced the cursor back to us) must release
	// the portal so the local cursor is freed — the Wayland equivalent of the
	// X11 xdotool recenter + xinput enable.
	if handler != nil {
		handler.OnActivated = p.ReleaseRemote
		handler.OnReclaimed = p.ReleaseRemote
	}

	p.wg.Add(1)
	go p.eiLoop()

	slog.Info("Wayland InputCapture bidirectional enabled", "edge", edgeSide, "screen", fmt.Sprintf("%dx%d", p.screenW, p.screenH))

	stop := func() { p.stop() }
	return p.cap, stop, nil
}

func (p *portal) stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		_ = p.obj.Call(icIface+".Disable", 0, p.session, map[string]dbus.Variant{}).Err
		p.wg.Wait()
		if p.ei != nil {
			p.ei.close()
		}
		_ = p.conn.Close()
	})
}

// --- portal request/response plumbing ---

func sanitizeName(name string) string {
	name = strings.TrimPrefix(name, ":")
	return strings.ReplaceAll(name, ".", "_")
}

func (p *portal) nextToken() string {
	return fmt.Sprintf("mwb%d", p.tokenSeq.Add(1))
}

// requestPath is the predictable Request object path the portal will emit the
// Response signal on (per the portal spec, derived from our name + token).
func (p *portal) requestPath(token string) dbus.ObjectPath {
	return dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + p.unique + "/" + token)
}

// doRequest subscribes to the predicted Response path, runs call (which invokes
// the portal method passing the handle_token), and waits for the Response.
func (p *portal) doRequest(call func(token string) error) (map[string]dbus.Variant, error) {
	token := p.nextToken()
	reqPath := p.requestPath(token)

	if err := p.conn.AddMatchSignal(
		dbus.WithMatchObjectPath(reqPath),
		dbus.WithMatchInterface("org.freedesktop.portal.Request"),
		dbus.WithMatchMember("Response"),
	); err != nil {
		return nil, fmt.Errorf("add response match: %w", err)
	}
	defer func() {
		_ = p.conn.RemoveMatchSignal(
			dbus.WithMatchObjectPath(reqPath),
			dbus.WithMatchInterface("org.freedesktop.portal.Request"),
			dbus.WithMatchMember("Response"),
		)
	}()

	ch := make(chan *dbus.Signal, 8)
	p.conn.Signal(ch)
	defer p.conn.RemoveSignal(ch)

	if err := call(token); err != nil {
		return nil, err
	}

	for {
		select {
		case sig := <-ch:
			if sig.Path != reqPath || sig.Name != "org.freedesktop.portal.Request.Response" {
				continue
			}
			var code uint32
			var results map[string]dbus.Variant
			if err := dbus.Store(sig.Body, &code, &results); err != nil {
				return nil, fmt.Errorf("decode response: %w", err)
			}
			if code != 0 {
				return nil, fmt.Errorf("portal request rejected (code %d)", code)
			}
			return results, nil
		case <-time.After(portalRequestTimeout):
			return nil, fmt.Errorf("portal request timed out")
		case <-p.stopCh:
			return nil, fmt.Errorf("stopped")
		}
	}
}

// createSession uses CreateSession2 — the current (non-deprecated) call, which
// returns its results synchronously (a{sv} → a{sv}) instead of via a Response
// signal. Capabilities are negotiated later in Start, not here.
func (p *portal) createSession() error {
	opts := map[string]dbus.Variant{
		"session_handle_token": dbus.MakeVariant(p.nextToken()),
	}
	var results map[string]dbus.Variant
	if err := p.obj.Call(icIface+".CreateSession2", 0, opts).Store(&results); err != nil {
		return fmt.Errorf("CreateSession2: %w", err)
	}
	v, ok := results["session_handle"]
	if !ok {
		return fmt.Errorf("CreateSession2: no session_handle in result")
	}
	// session_handle may arrive as a string or object path; methods take it as 'o'.
	p.session = dbus.ObjectPath(fmt.Sprint(v.Value()))
	return nil
}

// start activates the session and negotiates capabilities. parent_window is
// empty (no parent toplevel). persist_mode=2 + a saved restore_token let the
// compositor remember the grant so the permission dialog only appears once
// (and survives reboots). This is the call that may show that dialog.
func (p *portal) start() error {
	prev := loadRestoreToken()
	results, err := p.doRequest(func(token string) error {
		opts := map[string]dbus.Variant{
			"handle_token": dbus.MakeVariant(token),
			"capabilities": dbus.MakeVariant(uint32(capPointer | capKeyboard)),
			"persist_mode": dbus.MakeVariant(uint32(2)), // persist until the user revokes
		}
		if prev != "" {
			opts["restore_token"] = dbus.MakeVariant(prev)
		}
		var handle dbus.ObjectPath
		return p.obj.Call(icIface+".Start", 0, p.session, "", opts).Store(&handle)
	})
	if err != nil {
		return fmt.Errorf("Start: %w", err)
	}
	// Persist the (possibly refreshed) token so the next run restores silently.
	if rt, ok := results["restore_token"]; ok {
		if s, ok := rt.Value().(string); ok && s != "" && s != prev {
			saveRestoreToken(s)
		}
	}
	return nil
}

func restoreTokenPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "mwb", "inputcapture.token")
}

func loadRestoreToken() string {
	p := restoreTokenPath()
	if p == "" {
		return ""
	}
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

func saveRestoreToken(tok string) {
	p := restoreTokenPath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(tok), 0o600); err != nil {
		slog.Warn("could not save InputCapture restore token", "err", err)
	}
}

func (p *portal) getZones() error {
	results, err := p.doRequest(func(token string) error {
		opts := map[string]dbus.Variant{"handle_token": dbus.MakeVariant(token)}
		var handle dbus.ObjectPath
		return p.obj.Call(icIface+".GetZones", 0, p.session, opts).Store(&handle)
	})
	if err != nil {
		return fmt.Errorf("GetZones: %w", err)
	}
	// zones: a(uuii) = (width, height, x, y). Use the first zone. dbus.Store
	// converts the nested struct array straight into typed Go structs.
	zv, ok := results["zones"]
	if !ok {
		return fmt.Errorf("GetZones: no zones")
	}
	var zones []struct {
		W, H uint32
		X, Y int32
	}
	if err := dbus.Store([]interface{}{zv.Value()}, &zones); err != nil {
		return fmt.Errorf("GetZones: decode zones (%T): %w", zv.Value(), err)
	}
	if len(zones) == 0 {
		return fmt.Errorf("GetZones: empty zones")
	}
	p.screenW = int32(zones[0].W)
	p.screenH = int32(zones[0].H)
	p.zoneX = zones[0].X
	p.zoneY = zones[0].Y
	if zsv, ok := results["zone_set"]; ok {
		p.zoneSet = toU32(zsv.Value())
	}
	if p.screenW == 0 || p.screenH == 0 {
		return fmt.Errorf("GetZones: zero-size zone %dx%d", p.screenW, p.screenH)
	}
	return nil
}

// barrier IDs → which local edge the cursor crossed. Used to set up the remote
// entry direction in EnterRemoteEdge.
const (
	barrierLeft   = 1
	barrierRight  = 2
	barrierTop    = 3
	barrierBottom = 4
)

func barrierEdge(id uint32) string {
	switch id {
	case barrierRight:
		return "right"
	case barrierTop:
		return "top"
	case barrierBottom:
		return "bottom"
	default:
		return "left"
	}
}

// setBarriers puts a barrier on each enabled zone edge so the cursor can cross
// there (matching Windows MWB's "switch from any side"). Coordinates are in the
// zone's space and pixel-inclusive: a line spans first..last pixel (using the
// full width/height would overshoot the zone and the compositor rejects it).
func (p *portal) setBarriers(edges []string) error {
	xL := p.zoneX
	xR := p.zoneX + p.screenW
	yT := p.zoneY
	yB := p.zoneY + p.screenH
	byEdge := map[string]map[string]dbus.Variant{
		"left":   mkBarrier(barrierLeft, barrierPos{xL, yT, xL, yB - 1}),
		"right":  mkBarrier(barrierRight, barrierPos{xR, yT, xR, yB - 1}),
		"top":    mkBarrier(barrierTop, barrierPos{xL, yT, xR - 1, yT}),
		"bottom": mkBarrier(barrierBottom, barrierPos{xL, yB, xR - 1, yB}),
	}
	var barriers []map[string]dbus.Variant
	for _, e := range edges {
		if b, ok := byEdge[e]; ok {
			barriers = append(barriers, b)
		}
	}
	if len(barriers) == 0 {
		return fmt.Errorf("no valid edges to set barriers on")
	}
	results, err := p.doRequest(func(token string) error {
		opts := map[string]dbus.Variant{"handle_token": dbus.MakeVariant(token)}
		var handle dbus.ObjectPath
		return p.obj.Call(icIface+".SetPointerBarriers", 0,
			p.session, opts, barriers, p.zoneSet).Store(&handle)
	})
	if err != nil {
		return fmt.Errorf("SetPointerBarriers: %w", err)
	}
	if fv, ok := results["failed_barriers"]; ok {
		if failed, ok := fv.Value().([]uint32); ok && len(failed) == len(barriers) {
			return fmt.Errorf("all barriers rejected by compositor: %v", failed)
		} else if len(failed) > 0 {
			slog.Warn("some pointer barriers rejected", "failed", failed)
		}
	}
	return nil
}

func mkBarrier(id uint32, pos barrierPos) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"barrier_id": dbus.MakeVariant(id),
		"position":   dbus.MakeVariant(pos),
	}
}

func (p *portal) connectToEIS() (int, error) {
	var fd dbus.UnixFD
	err := p.obj.Call(icIface+".ConnectToEIS", 0, p.session, map[string]dbus.Variant{}).Store(&fd)
	if err != nil {
		return -1, fmt.Errorf("ConnectToEIS: %w", err)
	}
	return int(fd), nil
}

func (p *portal) enable() error {
	c := p.obj.Call(icIface+".Enable", 0, p.session, map[string]dbus.Variant{})
	if c.Err != nil {
		return fmt.Errorf("Enable: %w", c.Err)
	}
	return nil
}

// release stops capture and asks the portal to place the local cursor at (x,y).
func (p *portal) release(x, y int32) {
	opts := map[string]dbus.Variant{
		"activation_id":   dbus.MakeVariant(p.activeID.Load()),
		"cursor_position": dbus.MakeVariant(cursorPos{X: float64(x), Y: float64(y)}),
	}
	if c := p.obj.Call(icIface+".Release", 0, p.session, opts); c.Err != nil {
		slog.Warn("portal Release failed", "err", c.Err)
	}
}

// ReleaseRemote forces a release if we currently hold the remote (used when the
// server bounces the cursor back to us via MachineSwitched/NextMachine).
func (p *portal) ReleaseRemote() {
	if !p.cap.IsActive() {
		x, y := p.cap.SafeEntryPosition()
		p.cap.SetActive(true)
		p.release(x, y)
	}
}

// --- signals ---

func (p *portal) subscribeSignals() error {
	for _, member := range []string{"Activated", "Deactivated", "Disabled"} {
		if err := p.conn.AddMatchSignal(
			dbus.WithMatchObjectPath(portalPath),
			dbus.WithMatchInterface(icIface),
			dbus.WithMatchMember(member),
		); err != nil {
			return fmt.Errorf("subscribe %s: %w", member, err)
		}
	}
	ch := make(chan *dbus.Signal, 16)
	p.conn.Signal(ch)
	p.wg.Add(1)
	go p.signalLoop(ch)
	return nil
}

func (p *portal) signalLoop(ch chan *dbus.Signal) {
	defer p.wg.Done()
	for {
		select {
		case <-p.stopCh:
			return
		case sig := <-ch:
			if sig == nil || !strings.HasPrefix(sig.Name, icIface+".") {
				continue
			}
			switch sig.Name {
			case icIface + ".Activated":
				p.onActivated(sig.Body)
			case icIface + ".Deactivated":
				// Portal stopped capturing; ensure we consider ourselves local.
				p.cap.SetActive(true)
			case icIface + ".Disabled":
				slog.Info("portal disabled the capture session")
			}
		}
	}
}

func (p *portal) onActivated(body []interface{}) {
	// body: (o session_handle, a{sv} options) with activation_id (u) and
	// cursor_position ((dd)).
	if len(body) < 2 {
		return
	}
	opts, ok := body[1].(map[string]dbus.Variant)
	if !ok {
		return
	}
	if av, ok := opts["activation_id"]; ok {
		p.activeID.Store(toU32(av.Value()))
	}
	edge := "left"
	if bv, ok := opts["barrier_id"]; ok {
		edge = barrierEdge(toU32(bv.Value()))
	}
	// perpFrac is the position along the crossed edge: Y for vertical (left/right)
	// edges, X for horizontal (top/bottom) edges.
	var px, py float64 = 0.5, 0.5
	if cv, ok := opts["cursor_position"]; ok {
		if xy, ok := cv.Value().([]interface{}); ok && len(xy) >= 2 {
			if x, ok := xy[0].(float64); ok && p.screenW > 0 {
				px = (x - float64(p.zoneX)) / float64(p.screenW)
			}
			if y, ok := xy[1].(float64); ok && p.screenH > 0 {
				py = (y - float64(p.zoneY)) / float64(p.screenH)
			}
		}
	}
	perpFrac := py
	if edge == "top" || edge == "bottom" {
		perpFrac = px
	}

	if p.wantMod == 0 {
		p.cap.EnterRemoteEdge(edge, perpFrac)
		p.cap.NotifyRemoteSwitch() // formal handoff + triggers remote clipboard pull
		slog.Info("portal Activated — control crossed to remote", "edge", edge, "perpFrac", perpFrac)
		return
	}
	// Modifier-gated: the cursor is captured now, but only commit the switch if the
	// required modifier is held. libei reports the held-modifier state shortly after
	// capture starts, so reset, wait briefly, then commit or release back to local.
	p.curMods.Store(0)
	go p.gatedEnter(edge, perpFrac)
}

// gatedEnter commits or declines a modifier-gated edge crossing once libei has
// had a moment to report the held-modifier state.
func (p *portal) gatedEnter(edge string, perpFrac float64) {
	select {
	case <-p.stopCh:
		return
	case <-time.After(80 * time.Millisecond):
	}
	mods := p.curMods.Load()
	if mods&p.wantMod != 0 {
		p.cap.EnterRemoteEdge(edge, perpFrac)
		p.cap.NotifyRemoteSwitch()
		slog.Info("modifier held — control crossed to remote", "edge", edge, "mods", mods)
		return
	}
	// Modifier not held: stay local. Release the grab and park the cursor just
	// inside the edge where it hit.
	x, y := p.cap.EdgeReentry(edge, perpFrac)
	p.release(x, y)
	slog.Debug("edge crossed without modifier — staying local", "edge", edge, "mods", mods, "want", p.wantMod)
}

// modMask maps a modifier name to its xkb real-modifier mask bit.
func modMask(name string) uint32 {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "shift":
		return 1 // ShiftMask
	case "ctrl", "control":
		return 4 // ControlMask
	case "alt":
		return 8 // Mod1Mask
	default:
		return 0
	}
}

// --- libei receive loop ---

func (p *portal) eiLoop() {
	defer p.wg.Done()
	// ponytail: 2ms poll of the ei fd. ei_dispatch is a cheap no-op when idle;
	// 2ms is invisible for input. Upgrade path: block on the fd via unix.Poll
	// if the busy-ish tick ever shows up in power profiling.
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.ei.dispatch()
			for {
				ev, ok := p.ei.next()
				if !ok {
					break
				}
				p.feed(ev)
			}
		}
	}
}

func (p *portal) feed(ev eiEvent) {
	switch ev.kind {
	case eiMotion:
		// One combined update + one packet — avoids per-axis stair-stepping.
		p.cap.FeedMotion(ev.dx, ev.dy)
	case eiButton:
		p.cap.FeedEvent(evKey, uint16(ev.code), boolToVal(ev.press))
	case eiKey:
		p.cap.FeedEvent(evKey, uint16(ev.code), boolToVal(ev.press))
	case eiScroll:
		// Vertical is negated: EI scroll-down is positive, Windows WHEEL_DELTA-down
		// is negative. Horizontal passes through (both positive = right).
		if ev.discrete {
			p.cap.FeedWheel(-int32(ev.dy), int32(ev.dx))
		} else {
			// Touchpad pixels → accumulate into 120-unit notches.
			p.scrAccY += ev.dy
			p.scrAccX += ev.dx
			v := notches(&p.scrAccY)
			h := notches(&p.scrAccX)
			if v != 0 || h != 0 {
				p.cap.FeedWheel(-v, h)
			}
		}
	case eiModifiers:
		p.curMods.Store(ev.code)
	case eiDisconnect:
		slog.Warn("libei disconnected")
		p.stop()
	}
}

// notches drains an accumulated pixel scroll into whole WHEEL_DELTA notches,
// leaving the sub-notch remainder in *acc for the next event.
func notches(acc *float64) int32 {
	n := math.Trunc(*acc / touchpadPxPerNotch)
	*acc -= n * touchpadPxPerNotch
	return int32(n) * 120
}

func boolToVal(b bool) int32 {
	if b {
		return 1
	}
	return 0
}

func toU32(v interface{}) uint32 {
	switch n := v.(type) {
	case uint32:
		return n
	case int32:
		return uint32(n)
	case uint64:
		return uint32(n)
	case int64:
		return uint32(n)
	case int:
		return uint32(n)
	default:
		return 0
	}
}
