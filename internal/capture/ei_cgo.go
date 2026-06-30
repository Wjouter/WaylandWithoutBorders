//go:build linux && wayland

// Package capture — libei receiver wrapper (cgo).
//
// ponytail: cgo + libei is the lazy path within the chosen portal approach.
// A pure-Go EI wire-protocol implementation is the upgrade if dropping the C
// dependency ever matters; it's a large lift and not worth it today.
package capture

/*
#cgo pkg-config: libei-1.0
#include <stdlib.h>
#include <libei.h>

// ei_seat_bind_capabilities is variadic with a NULL sentinel, which is fiddly to
// call from cgo. Wrap the fixed capability set we want as a receiver: pointer
// motion, buttons, scroll, and keyboard. Binding is what makes the EIS server
// create devices and start delivering events.
static void mwb_bind_caps(struct ei_seat *seat) {
	ei_seat_bind_capabilities(seat,
		EI_DEVICE_CAP_POINTER,
		EI_DEVICE_CAP_BUTTON,
		EI_DEVICE_CAP_SCROLL,
		EI_DEVICE_CAP_KEYBOARD,
		NULL);
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// eiEventKind is the subset of EI events we forward to the remote.
type eiEventKind int

const (
	eiNone eiEventKind = iota
	eiMotion
	eiButton
	eiScroll
	eiKey
	eiModifiers
	eiDisconnect
)

// eiEvent is a normalized libei event. dx/dy are relative motion (motion),
// code is the evdev button/key code (button/key), value is wheel steps (scroll),
// press distinguishes down/up (button/key).
type eiEvent struct {
	kind     eiEventKind
	dx, dy   float64 // relative motion (sub-pixel preserved), or scroll dx/dy
	code     uint32
	press    bool
	discrete bool // scroll: true = wheel notches (120-units), false = touchpad pixels
}

// eiConn is a libei receiver bound to the EIS fd handed back by the portal.
type eiConn struct {
	ctx *C.struct_ei
}

// newEIReceiver creates a receiver context on fd. libei takes ownership of fd.
func newEIReceiver(fd int, name string) (*eiConn, error) {
	ctx := C.ei_new_receiver(nil)
	if ctx == nil {
		return nil, fmt.Errorf("ei_new_receiver returned nil")
	}
	cName := C.CString(name)
	C.ei_configure_name(ctx, cName)
	C.free(unsafe.Pointer(cName))

	if rc := C.ei_setup_backend_fd(ctx, C.int(fd)); rc != 0 {
		C.ei_unref(ctx)
		return nil, fmt.Errorf("ei_setup_backend_fd failed: %d", int(rc))
	}
	return &eiConn{ctx: ctx}, nil
}

// pollFD returns the libei fd to wait on for readiness.
func (e *eiConn) pollFD() int { return int(C.ei_get_fd(e.ctx)) }

// dispatch reads any pending data off the fd into libei's internal queue.
func (e *eiConn) dispatch() { C.ei_dispatch(e.ctx) }

// next drains one event. ok=false means the queue is empty. Non-input events
// (connect, device added/resumed, frame, etc.) are reported as eiNone so the
// caller can ignore them without special-casing every type.
func (e *eiConn) next() (ev eiEvent, ok bool) {
	cev := C.ei_get_event(e.ctx)
	if cev == nil {
		return eiEvent{}, false
	}
	defer C.ei_event_unref(cev)

	switch C.ei_event_get_type(cev) {
	case C.EI_EVENT_SEAT_ADDED:
		// Bind the capabilities we want so the EIS server creates devices and
		// starts delivering events. Without this no input ever flows.
		C.mwb_bind_caps(C.ei_event_get_seat(cev))
		ev.kind = eiNone
	case C.EI_EVENT_POINTER_MOTION:
		ev.kind = eiMotion
		ev.dx = float64(C.ei_event_pointer_get_dx(cev))
		ev.dy = float64(C.ei_event_pointer_get_dy(cev))
	case C.EI_EVENT_BUTTON_BUTTON:
		ev.kind = eiButton
		ev.code = uint32(C.ei_event_button_get_button(cev))
		ev.press = bool(C.ei_event_button_get_is_press(cev))
	case C.EI_EVENT_SCROLL_DISCRETE:
		// Mouse wheel notches. libei reports these already in 120-unit steps,
		// matching Windows WHEEL_DELTA, so pass them straight through.
		ev.kind = eiScroll
		ev.discrete = true
		ev.dx = float64(C.ei_event_scroll_get_discrete_dx(cev))
		ev.dy = float64(C.ei_event_scroll_get_discrete_dy(cev))
		if ev.dx == 0 && ev.dy == 0 {
			return eiEvent{kind: eiNone}, true
		}
	case C.EI_EVENT_SCROLL_DELTA:
		// Touchpad two-finger scroll: smooth pixel deltas. Accumulated into wheel
		// notches downstream.
		ev.kind = eiScroll
		ev.discrete = false
		ev.dx = float64(C.ei_event_scroll_get_dx(cev))
		ev.dy = float64(C.ei_event_scroll_get_dy(cev))
		if ev.dx == 0 && ev.dy == 0 {
			return eiEvent{kind: eiNone}, true
		}
	case C.EI_EVENT_KEYBOARD_KEY:
		ev.kind = eiKey
		ev.code = uint32(C.ei_event_keyboard_get_key(cev))
		ev.press = bool(C.ei_event_keyboard_get_key_is_press(cev))
	case C.EI_EVENT_KEYBOARD_MODIFIERS:
		// Currently-held modifier mask (xkb real-mod bits: Shift=1, Control=4,
		// Mod1/Alt=8). Sent at capture start, so it reflects keys held before the
		// edge was crossed — used to gate switching on a modifier.
		ev.kind = eiModifiers
		ev.code = uint32(C.ei_event_keyboard_get_xkb_mods_depressed(cev))
	case C.EI_EVENT_DISCONNECT:
		ev.kind = eiDisconnect
	default:
		ev.kind = eiNone
	}
	return ev, true
}

func (e *eiConn) close() {
	if e.ctx != nil {
		C.ei_unref(e.ctx)
		e.ctx = nil
	}
}
