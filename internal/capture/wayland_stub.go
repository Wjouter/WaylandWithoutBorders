//go:build linux && !wayland

package capture

import (
	"fmt"

	"github.com/lucky-verma/mwb-linux/internal/network"
)

// RunWayland is a stub for builds without the `wayland` tag. The Wayland
// bidirectional driver depends on cgo + libei, so it is opt-in at build time.
func RunWayland(conn *network.Conn, handler *network.Handler, edgeSide string, edges []string, switchModifier string, accel float64) (*Capturer, func(), error) {
	return nil, nil, fmt.Errorf("this mwb build has no Wayland support; rebuild with: go build -tags wayland")
}
