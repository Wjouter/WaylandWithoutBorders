// cmd/mwb/main.go
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lucky-verma/mwb-linux/internal/capture"
	"github.com/lucky-verma/mwb-linux/internal/clipboard"
	"github.com/lucky-verma/mwb-linux/internal/config"
	"github.com/lucky-verma/mwb-linux/internal/input"
	"github.com/lucky-verma/mwb-linux/internal/network"
)

func main() {
	// `mwb gui` launches the local web configuration UI instead of the daemon.
	if len(os.Args) > 1 && os.Args[1] == "gui" {
		runGUI(os.Args[2:])
		return
	}

	configPath := flag.String("config", "", "path to config.toml")
	debug := flag.Bool("debug", false, "enable debug logging")
	edgeSide := flag.String("edge", "", "screen edge to switch: left or right (overrides config)")
	bidirectional := flag.Bool("bidi", false, "enable bidirectional input (send local input to remote)")
	noClipboard := flag.Bool("no-clipboard", false, "disable clipboard sharing (overrides config)")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if *configPath == "" {
		home, _ := os.UserHomeDir()
		*configPath = filepath.Join(home, ".config", "mwb", "config.toml")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		fmt.Fprintf(os.Stderr, "\nCreate config at %s with:\n\n", *configPath)
		fmt.Fprintf(os.Stderr, "  host = \"192.168.1.100\"\n  key = \"YourSecurityKey\"\n  name = \"linux\"\n\n")
		os.Exit(1)
	}

	// Apply config defaults for flags not explicitly set on the command line.
	// This allows config.toml to set edge/remote dims without requiring CLI flags.
	if *edgeSide == "" {
		*edgeSide = cfg.Edge
	}
	if *edgeSide == "" {
		*edgeSide = "right" // final fallback
	}

	// Bidirectional turns on if either the flag or config requests it, so the
	// GUI can toggle it via config without editing the systemd unit.
	bidi := *bidirectional || cfg.Bidirectional

	// Clipboard runs by default. Either config (clipboard = false) or the
	// --no-clipboard flag disables it; the flag wins over config.
	clipboardEnabled := cfg.ClipboardEnabled() && !*noClipboard
	keyboardLayout := input.ResolveKeyboardLayout(cfg.KeyboardLayout)

	slog.Debug("debug logging enabled")
	slog.Info("mwb starting", "host", cfg.Host, "port", cfg.MessagePort(), "name", cfg.Name, "bidirectional", bidi, "edge", *edgeSide, "clipboard", clipboardEnabled, "keyboard_layout", keyboardLayout)

	mouse, err := input.CreateVirtualMouse("mwb-mouse")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating virtual mouse: %v\n", err)
		fmt.Fprintln(os.Stderr, "Setup required:")
		fmt.Fprintln(os.Stderr, "  1. sudo modprobe uinput")
		fmt.Fprintln(os.Stderr, "  2. echo 'uinput' | sudo tee /etc/modules-load.d/uinput.conf")
		fmt.Fprintln(os.Stderr, "  3. echo 'KERNEL==\"uinput\", GROUP=\"input\", MODE=\"0660\"' | sudo tee /etc/udev/rules.d/99-uinput.rules")
		fmt.Fprintln(os.Stderr, "  4. sudo udevadm control --reload-rules && sudo udevadm trigger /dev/uinput")
		fmt.Fprintln(os.Stderr, "  5. Ensure your user is in the 'input' group: sudo usermod -aG input $USER")
		fmt.Fprintln(os.Stderr, "Ensure your user is in the 'input' group: sudo usermod -aG input $USER")
		os.Exit(1)
	}
	defer func() { _ = mouse.Close() }()

	keyboard, err := input.CreateVirtualKeyboard("mwb-keyboard")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating virtual keyboard: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = keyboard.Close() }()

	slog.Info("virtual input devices created")

	handler := &network.Handler{
		Mouse:             mouse,
		Keyboard:          keyboard,
		InboundMultiplier: cfg.InboundMultiplier,
		KeyboardLayout:    keyboardLayout,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start TCP server to accept incoming connections from Windows MWB
	serverStop := make(chan struct{})
	incomingCh, err := network.ListenAndAccept(cfg.MessagePort(), cfg.Key, cfg.Name, serverStop)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error starting listener: %v\n", err)
		fmt.Fprintln(os.Stderr, "Is another mwb instance already running?")
		os.Exit(1)
	}
	defer close(serverStop)

	go func() {
		for {
			// Race: try outbound connect AND accept inbound — first one wins
			addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.MessagePort())
			slog.Info("connecting", "addr", addr)

			// Always report a result (conn or nil) so a failed outbound attempt
			// doesn't leave the select below blocked forever waiting on inbound.
			connCh := make(chan *network.Conn, 1)
			go func() {
				c, err := network.Connect(addr, cfg.Key, cfg.Name, 10*time.Second)
				if err != nil {
					slog.Debug("outbound connect failed", "err", err)
					connCh <- nil
					return
				}
				connCh <- c
			}()

			// Wait for either outbound or inbound connection
			var conn *network.Conn
			select {
			case conn = <-connCh:
				if conn == nil {
					// Outbound failed (interface down, host unreachable). Back off
					// briefly and retry — the loop keeps listening for inbound too.
					time.Sleep(2 * time.Second)
					continue
				}
				slog.Info("connected (outbound)", "remote", conn.RemoteName)
			case conn = <-incomingCh:
				slog.Info("connected (inbound)", "remote", conn.RemoteName)
				// The outbound goroutine is still dialing; reap whatever it returns
				// so a late-succeeding connection isn't leaked.
				go func(ch chan *network.Conn) {
					if c := <-ch; c != nil {
						_ = c.Close()
					}
				}(connCh)
			}

			// Start clipboard sharing on the auto-detected display unless disabled.
			var clipMgr *clipboard.Manager
			if clipboardEnabled {
				clipMgr = clipboard.NewManager(conn, capture.DetectDisplay(), cfg.Key, cfg.Host, cfg.Port)
				handler.Clipboard = clipMgr
				clipMgr.Start()
			}

			// Start bidirectional capture if enabled. On Wayland the X11
			// xdotool/xinput path can't work, so use the InputCapture portal driver.
			var cap *capture.Capturer
			var capStop func()
			if bidi && isWayland() {
				edges := cfg.EnabledEdges()
				if len(edges) == 0 {
					slog.Info("edge switching disabled (edges = none); not capturing")
				} else if c, stop, err := capture.RunWayland(conn, handler, *edgeSide, edges, cfg.SwitchModifier, cfg.AccelMultiplier); err != nil {
					slog.Error("wayland capture start failed", "err", err)
				} else {
					cap, capStop = c, stop
				}
			} else if bidi {
				screen := capture.GetScreenSizeXrandr()
				slog.Info("screen detected", "width", screen.Width, "height", screen.Height)

				cap = capture.New(conn, screen, *edgeSide)
				// Cursor speed: the only acceleration knob lives here (Windows
				// applies none of its own), so honor the configured multiplier.
				cap.SetAccelMultiplier(cfg.AccelMultiplier)

				// When we receive MachineSwitched, mark ourselves as active and
				// move cursor away from edge — without this the cursor stays at
				// x=0 and re-triggers the edge switch immediately on any movement.
				handler.OnActivated = func() {
					cap.SetActive(true)
					go func() {
						ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
						defer cancel()
						entryX, entryY := cap.SafeEntryPosition()
						_ = exec.CommandContext(ctx, "xdotool", "mousemove", "--",
							fmt.Sprintf("%d", entryX),
							fmt.Sprintf("%d", entryY)).Run()
					}()
				}

				// When server sends NextMachine (cursor bounced off server's edge),
				// reclaim control and move cursor away from our edge
				handler.OnReclaimed = func() {
					cap.SetActive(true)
					// Move cursor to center so it doesn't immediately re-trigger edge
					go func() {
						ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
						defer cancel()
						_ = exec.CommandContext(ctx, "xdotool", "mousemove", "--",
							fmt.Sprintf("%d", screen.Width/2),
							fmt.Sprintf("%d", screen.Height/2)).Run()
					}()
				}

				if err := cap.Run(); err != nil {
					slog.Error("capture start failed", "err", err)
				} else {
					slog.Info("bidirectional capture enabled", "edge", *edgeSide)
				}
			}

			if err := network.ReceiveLoop(conn, handler); err != nil {
				slog.Error("receive loop error", "err", err)
			}

			// Stop capture first — prevents in-flight SendPacket after conn.Close()
			if capStop != nil {
				capStop() // Wayland: tears down the portal + libei
			} else if cap != nil {
				cap.Stop()
			}

			if clipMgr != nil {
				clipMgr.Stop() // waits for goroutine via WaitGroup
			}

			_ = conn.Close()
			slog.Info("disconnected, reconnecting")
		}
	}()

	sig := <-sigCh
	slog.Info("shutting down", "signal", sig)
}

// isWayland reports whether we're running under a Wayland session, in which
// case the X11 xdotool/xinput bidi path won't work and we use the portal driver.
//
// Env vars alone are unreliable: a --user service can start with a sparse
// environment (only DISPLAY=:0 from XWayland) before the session imports
// WAYLAND_DISPLAY, and would then wrongly take the X11 path. So also probe for a
// live Wayland socket in XDG_RUNTIME_DIR. Failing toward Wayland is the safe
// choice — a portal error just disables capture, whereas the X11 path can grab
// input and lock the session.
func isWayland() bool {
	if os.Getenv("XDG_SESSION_TYPE") == "wayland" || os.Getenv("WAYLAND_DISPLAY") != "" {
		return true
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		if socks, _ := filepath.Glob(filepath.Join(dir, "wayland-[0-9]*")); len(socks) > 0 {
			return true
		}
	}
	return false
}
