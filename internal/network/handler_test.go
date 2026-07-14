// internal/network/handler_test.go
package network

import (
	"testing"

	"github.com/lucky-verma/mwb-linux/internal/input"
	"github.com/lucky-verma/mwb-linux/internal/protocol"
)

// MockInputDevice records calls for testing.
type MockInputDevice struct {
	MouseMoves  []struct{ X, Y int32 }
	ButtonDowns []uint16
	ButtonUps   []uint16
	Wheels      []int32
	KeyDowns    []uint16
	KeyUps      []uint16
	held        []uint16
}

func (m *MockInputDevice) MoveTo(x, y int32) error {
	m.MouseMoves = append(m.MouseMoves, struct{ X, Y int32 }{x, y})
	return nil
}
func (m *MockInputDevice) ButtonDown(btn uint16) error {
	m.ButtonDowns = append(m.ButtonDowns, btn)
	return nil
}
func (m *MockInputDevice) ButtonUp(btn uint16) error {
	m.ButtonUps = append(m.ButtonUps, btn)
	return nil
}
func (m *MockInputDevice) Wheel(delta int32) error {
	m.Wheels = append(m.Wheels, delta)
	return nil
}
func (m *MockInputDevice) HWheel(delta int32) error {
	return nil
}
func (m *MockInputDevice) KeyDown(code uint16) error {
	m.KeyDowns = append(m.KeyDowns, code)
	m.held = append(m.held, code)
	return nil
}
func (m *MockInputDevice) KeyUp(code uint16) error {
	m.KeyUps = append(m.KeyUps, code)
	m.removeHeld(code)
	return nil
}
func (m *MockInputDevice) ReleaseAll() error {
	for _, code := range m.held {
		m.KeyUps = append(m.KeyUps, code)
	}
	m.held = nil
	return nil
}
func (m *MockInputDevice) removeHeld(code uint16) {
	for i, c := range m.held {
		if c == code {
			m.held = append(m.held[:i], m.held[i+1:]...)
			return
		}
	}
}

func TestHandleMouseMove(t *testing.T) {
	mock := &MockInputDevice{}
	h := &Handler{Mouse: mock, Keyboard: mock}

	pkt := &protocol.Packet{Type: protocol.Mouse}
	pkt.Mouse.X = 32768
	pkt.Mouse.Y = 16384
	pkt.Mouse.DwFlags = protocol.WM_MOUSEMOVE

	h.HandlePacket(pkt)

	if len(mock.MouseMoves) != 1 {
		t.Fatalf("expected 1 move, got %d", len(mock.MouseMoves))
	}
	if mock.MouseMoves[0].X != 32768 || mock.MouseMoves[0].Y != 16384 {
		t.Errorf("move = (%d,%d), want (32768,16384)", mock.MouseMoves[0].X, mock.MouseMoves[0].Y)
	}
}

func sendMove(h *Handler, x, y int32) {
	p := &protocol.Packet{Type: protocol.Mouse}
	p.Mouse.X, p.Mouse.Y, p.Mouse.DwFlags = x, y, protocol.WM_MOUSEMOVE
	h.HandlePacket(p)
}

func TestInboundMultiplier_DefaultMirrors(t *testing.T) {
	mock := &MockInputDevice{}
	h := &Handler{Mouse: mock, Keyboard: mock} // unset -> treated as 1.0
	sendMove(h, 5000, 5000)
	sendMove(h, 5300, 5200)
	last := mock.MouseMoves[len(mock.MouseMoves)-1]
	if last.X != 5300 || last.Y != 5200 {
		t.Errorf("default move = (%d,%d), want (5300,5200) 1:1 mirror", last.X, last.Y)
	}
}

func TestInboundMultiplier_ScalesDeltas(t *testing.T) {
	mock := &MockInputDevice{}
	h := &Handler{Mouse: mock, Keyboard: mock, InboundMultiplier: 2.0}
	sendMove(h, 10000, 10000) // seeds (snap to position)
	sendMove(h, 10100, 10080) // delta (100,80) * 2 -> (200,160) => (10200,10160)
	if mock.MouseMoves[0].X != 10000 || mock.MouseMoves[0].Y != 10000 {
		t.Errorf("seed move = (%d,%d), want (10000,10000)", mock.MouseMoves[0].X, mock.MouseMoves[0].Y)
	}
	last := mock.MouseMoves[len(mock.MouseMoves)-1]
	if last.X != 10200 || last.Y != 10160 {
		t.Errorf("scaled move = (%d,%d), want (10200,10160)", last.X, last.Y)
	}
}

func TestInboundMultiplier_JumpSnaps(t *testing.T) {
	mock := &MockInputDevice{}
	h := &Handler{Mouse: mock, Keyboard: mock, InboundMultiplier: 3.0}
	sendMove(h, 10000, 10000)
	sendMove(h, 60000, 60000) // delta > threshold => snap, not scaled
	last := mock.MouseMoves[len(mock.MouseMoves)-1]
	if last.X != 60000 || last.Y != 60000 {
		t.Errorf("jump move = (%d,%d), want (60000,60000) snapped", last.X, last.Y)
	}
}

func TestHandleMouseButtons(t *testing.T) {
	mock := &MockInputDevice{}
	h := &Handler{Mouse: mock, Keyboard: mock}

	pkt := &protocol.Packet{Type: protocol.Mouse}
	pkt.Mouse.DwFlags = protocol.WM_LBUTTONDOWN
	h.HandlePacket(pkt)
	if len(mock.ButtonDowns) != 1 || mock.ButtonDowns[0] != input.BTN_LEFT {
		t.Errorf("expected BTN_LEFT down, got %v", mock.ButtonDowns)
	}

	pkt.Mouse.DwFlags = protocol.WM_LBUTTONUP
	h.HandlePacket(pkt)
	if len(mock.ButtonUps) != 1 || mock.ButtonUps[0] != input.BTN_LEFT {
		t.Errorf("expected BTN_LEFT up, got %v", mock.ButtonUps)
	}
}

func TestHandleKeyboard(t *testing.T) {
	mock := &MockInputDevice{}
	h := &Handler{Mouse: mock, Keyboard: mock}

	expectedCode, ok := input.VKToKeyCode(0x41)
	if !ok {
		t.Fatal("VKToKeyCode(0x41) should map VK_A")
	}

	// Key down: VK_A (0x41)
	pkt := &protocol.Packet{Type: protocol.Keyboard}
	pkt.Keyboard.WVk = 0x41
	pkt.Keyboard.DwFlags = 0

	h.HandlePacket(pkt)
	if len(mock.KeyDowns) != 1 || mock.KeyDowns[0] != expectedCode {
		t.Errorf("expected keycode %d down, got %v", expectedCode, mock.KeyDowns)
	}

	// Key up: VK_A with LLKHF_UP (0x80)
	pkt.Keyboard.DwFlags = protocol.LLKHF_UP
	h.HandlePacket(pkt)
	if len(mock.KeyUps) != 1 || mock.KeyUps[0] != expectedCode {
		t.Errorf("expected keycode %d up, got %v", expectedCode, mock.KeyUps)
	}
}

// A modifier held when the cursor leaves us (HideMouse) must be released — its
// key-up happens on the remote and never reaches us. Same for a switch-away.
func TestHeldKeyReleasedOnLeave(t *testing.T) {
	shift, ok := input.VKToKeyCode(0xA0) // VK_LSHIFT
	if !ok {
		t.Fatal("VKToKeyCode(VK_LSHIFT) should map")
	}
	for _, leave := range []protocol.PackageType{protocol.HideMouse, protocol.MachineSwitched} {
		mock := &MockInputDevice{}
		h := &Handler{Mouse: mock, Keyboard: mock}

		down := &protocol.Packet{Type: protocol.Keyboard}
		down.Keyboard.WVk = 0xA0
		h.HandlePacket(down) // Shift down, no matching up

		h.HandlePacket(&protocol.Packet{Type: leave})

		if len(mock.KeyUps) != 1 || mock.KeyUps[0] != shift {
			t.Errorf("%v: expected Shift (%d) released, got %v", leave, shift, mock.KeyUps)
		}
	}
}

// ReleaseHeldKeys (called on disconnect) must lift a stuck modifier too.
func TestReleaseHeldKeysOnDisconnect(t *testing.T) {
	shift, _ := input.VKToKeyCode(0xA0)
	mock := &MockInputDevice{}
	h := &Handler{Mouse: mock, Keyboard: mock}
	down := &protocol.Packet{Type: protocol.Keyboard}
	down.Keyboard.WVk = 0xA0
	h.HandlePacket(down)

	h.ReleaseHeldKeys()

	if len(mock.KeyUps) != 1 || mock.KeyUps[0] != shift {
		t.Errorf("expected Shift (%d) released on disconnect, got %v", shift, mock.KeyUps)
	}
}

func TestHandleKeyboardGermanLayout(t *testing.T) {
	mock := &MockInputDevice{}
	h := &Handler{Mouse: mock, Keyboard: mock, KeyboardLayout: "de"}

	pkt := &protocol.Packet{Type: protocol.Keyboard}
	pkt.Keyboard.WVk = 0x5A // VK_Z; German layout should inject the physical Y key.

	h.HandlePacket(pkt)
	if len(mock.KeyDowns) != 1 || mock.KeyDowns[0] != input.KEY_Y {
		t.Errorf("expected KEY_Y down for German VK_Z, got %v", mock.KeyDowns)
	}
}

func TestHandleMouseWheel(t *testing.T) {
	mock := &MockInputDevice{}
	h := &Handler{Mouse: mock, Keyboard: mock}

	pkt := &protocol.Packet{Type: protocol.Mouse}
	pkt.Mouse.DwFlags = protocol.WM_MOUSEWHEEL
	pkt.Mouse.WheelDelta = 120

	h.HandlePacket(pkt)
	if len(mock.Wheels) != 1 || mock.Wheels[0] != 1 {
		t.Errorf("expected wheel=1, got %v", mock.Wheels)
	}
}

func TestHandleRightButton(t *testing.T) {
	mock := &MockInputDevice{}
	h := &Handler{Mouse: mock, Keyboard: mock}

	pkt := &protocol.Packet{Type: protocol.Mouse}
	pkt.Mouse.DwFlags = protocol.WM_RBUTTONDOWN
	h.HandlePacket(pkt)
	if len(mock.ButtonDowns) != 1 || mock.ButtonDowns[0] != input.BTN_RIGHT {
		t.Errorf("expected BTN_RIGHT down, got %v", mock.ButtonDowns)
	}
}

func TestHandleMiddleButton(t *testing.T) {
	mock := &MockInputDevice{}
	h := &Handler{Mouse: mock, Keyboard: mock}

	pkt := &protocol.Packet{Type: protocol.Mouse}
	pkt.Mouse.DwFlags = protocol.WM_MBUTTONDOWN
	h.HandlePacket(pkt)
	if len(mock.ButtonDowns) != 1 || mock.ButtonDowns[0] != input.BTN_MIDDLE {
		t.Errorf("expected BTN_MIDDLE down, got %v", mock.ButtonDowns)
	}
}
