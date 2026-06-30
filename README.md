<p align="center">
  <img src="docs/assets/banner.png" alt="MWB Linux — Mouse Without Borders for Linux" width="800">
</p>

<p align="center">
  Share your keyboard, mouse, and clipboard seamlessly between Linux and Windows.
</p>

<p align="center">
  <a href="#features">Features</a> &bull;
  <a href="#whats-new-in-this-fork">What's New</a> &bull;
  <a href="#installation">Installation</a> &bull;
  <a href="#quick-start">Quick Start</a> &bull;
  <a href="#how-it-works">How It Works</a> &bull;
  <a href="#configuration">Configuration</a> &bull;
  <a href="#contributing">Contributing</a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/platform-Linux-blue" alt="Platform">
  <img src="https://img.shields.io/badge/language-Go-00ADD8" alt="Go">
  <img src="https://img.shields.io/badge/license-MIT-green" alt="License">
  <img src="https://img.shields.io/badge/protocol-MWB%20Compatible-orange" alt="MWB Compatible">
</p>

---

> ### 🍴 This is a fork of [lucky-verma/mwb-linux](https://github.com/lucky-verma/mwb-linux)
>
> It adds **native Wayland bidirectional support**, a **configuration GUI**, **file copy/paste**, **switch-from-any-edge** with a modifier "easy mouse" mode, smoother input, and more. See [What's new in this fork](#whats-new-in-this-fork) for the full list. All credit for the original Linux client and the MWB protocol implementation goes to the upstream project.

## What is this?

MWB Linux is a native Linux client that connects to **Microsoft PowerToys Mouse Without Borders** on Windows. Move your mouse to the edge of the screen and it seamlessly jumps to the other machine — along with your keyboard and clipboard. **Bidirectional control now works natively on Wayland** (KDE Plasma 6 / GNOME 46+), not just X11.

```mermaid
flowchart LR
    A["🐧 <b>Linux PC</b><br/>Mouse · Keyboard · Files"] <-->|" 🖱️ Mouse · ⌨️ Keyboard · 📋 Clipboard · 📁 Files "| B["🪟 <b>Windows PC</b><br/>Mouse · Keyboard · Files"]
```

No extra software needed on Windows beyond PowerToys, which bundles Mouse Without Borders.

## Features

- **Bidirectional mouse sharing** — Control both machines from either keyboard/mouse
- **Native Wayland support** — Bidirectional input via the InputCapture portal + libei (no `xdotool`/`xinput`, no root); X11 still supported
- **Switch from any edge** — Cross left/right/top/bottom; choose which edges, or require a held modifier (Shift/Ctrl/Alt) — PowerToys-style "Easy Mouse"
- **Configuration GUI** — `mwb gui` opens a local web UI to edit settings and control the service
- **Clipboard & file sync** — Copy text, images, **or files** on one machine and paste on the other
- **Keyboard forwarding** — Type on your Linux keyboard, text appears on Windows
- **Full mouse support** — Scroll wheel, touchpad two-finger scroll, horizontal scroll, middle click, and side buttons (back/forward)
- **Encrypted** — AES-256-CBC encryption with PBKDF2 key derivation
- **Remembered permission** — The Wayland capture prompt only appears once (persists across reboots)
- **Lightweight** — Single Go binary, minimal dependencies

## What's new in this fork

Everything below is added on top of [lucky-verma/mwb-linux](https://github.com/lucky-verma/mwb-linux):

**Wayland**
- Native **bidirectional** input on Wayland via the `org.freedesktop.portal.InputCapture` portal + libei — the upstream project is X11-only for the capture direction. Compositor-native edge barriers and input suppression replace `xdotool`/`xinput`, and no root is needed.
- The capture **permission dialog is remembered** (`persist_mode` + `restore_token`), so it only prompts once.
- The binary **auto-selects** the Wayland driver on a Wayland session and the X11 path otherwise.

**Switching**
- **Any-edge switching** (left/right/top/bottom) instead of a single configured edge.
- **Selectable edges** (`edges`) and a full **disable** option.
- **Modifier-gated switching** (`switch_modifier`) — only cross while holding Shift/Ctrl/Alt, like PowerToys' Easy Mouse.

**Clipboard / files**
- **File copy/paste** in both directions over MWB's separate file-transfer channel (received files land in `~/Downloads/MouseWithoutBorders/` and on your clipboard).

**Input quality**
- Fixed **scroll direction**, added **touchpad two-finger scroll** and **horizontal scroll**.
- Added **middle-click** and **side-button** (back/forward) forwarding.
- Smoother cursor motion (single combined packet + sub-pixel accumulation).
- **Resolution-free** coordinate mapping — no need to configure the remote screen size.

**Configuration**
- A built-in **web GUI** (`mwb gui`) to edit all settings, toggle bidirectional/edges/modifier, and start/stop/enable the systemd service.
- `bidirectional` config flag so the daemon can be fully config-driven (no CLI flags needed).

## Demo

| Direction | What happens |
|-----------|-------------|
| Mouse hits an enabled edge on Linux | Cursor crosses to Windows |
| Mouse hits the return edge on Windows | Cursor returns to Linux |
| Ctrl+C on Windows | Text / image / **file** available on Linux |
| Ctrl+C on Linux | Text / image / **file** available on Windows |
| Type on Linux keyboard | Text appears in focused Windows app |

## Installation

Install **from source** — this fork isn't published as a prebuilt package. The
whole thing is a single Go binary; the steps below are complete for a fresh
machine.

> Replace `<your-username>` with your GitHub user (or use
> `lucky-verma/mwb-linux` for the upstream X11-only version).

### 1. Install dependencies

**Wayland (KDE Plasma 6+ / GNOME 46+) — recommended:**

| Distro | Command |
|--------|---------|
| Arch / CachyOS | `sudo pacman -S go libei wl-clipboard` |
| Debian / Ubuntu | `sudo apt install golang libei-dev wl-clipboard` |
| Fedora | `sudo dnf install golang libei-devel wl-clipboard` |

**X11:**

| Distro | Command |
|--------|---------|
| Arch / CachyOS | `sudo pacman -S go xdotool xorg-xinput xclip` |
| Debian / Ubuntu | `sudo apt install golang xdotool xinput xclip x11-xserver-utils` |
| Fedora | `sudo dnf install golang xdotool xinput xclip` |

You need **Go 1.25+**. On Wayland, `libei`/`libei-dev` is a *build* dependency
(cgo); `wl-clipboard` is a *runtime* dependency for file/clipboard sharing.

### 2. Set up uinput permissions

Input **injection** always goes through `/dev/uinput`, and on **X11** input
**capture** reads `/dev/input/event*`. Both need the `input` group (this avoids
running as root):

```bash
sudo modprobe uinput
echo uinput | sudo tee /etc/modules-load.d/uinput.conf
echo 'KERNEL=="uinput", GROUP="input", MODE="0660"' | sudo tee /etc/udev/rules.d/99-mwb-uinput.rules
sudo udevadm control --reload-rules && sudo udevadm trigger /dev/uinput
sudo usermod -aG input "$USER"
```

> **Log out and back in** afterwards so the group change takes effect.
> On Wayland the InputCapture portal handles capture, so no root or
> `/dev/input` access is needed for the capture direction — only `uinput` for
> injection.

### 3. Build and install

```bash
git clone https://github.com/<your-username>/mwb-linux.git
cd mwb-linux

# Wayland (cgo + libei):
make install-wayland

# …or X11:
make install
```

This installs a **per-user** systemd service: the binary goes to `~/go/bin/mwb`
and the unit to `~/.config/systemd/user/`. Don't run `make install` with `sudo`
— that installs under `root` and the `--user` service can't find the binary.

Now jump to [Quick Start](#quick-start) to configure the security key, then:

```bash
systemctl --user enable --now mwb
journalctl --user -u mwb -f          # follow logs
```

To uninstall: `make uninstall`. To run in the foreground for testing instead of
as a service: `mwb -bidi -debug`.

## Quick Start

### 1. Get the security key from Windows

Open **PowerToys** → **Mouse Without Borders** → copy the **Security Key**.

### 2. Configure

```bash
mkdir -p ~/.config/mwb
cat > ~/.config/mwb/config.toml << EOF
host = "192.168.1.100"        # Your Windows machine's IP
key = "YourSecurityKey"       # From PowerToys MWB
name = "linux"                # This machine's name (max 15 chars)
keyboard_layout = "auto"      # Inbound keyboard layout profile
EOF
```

### 3. Run

```bash
# Receive only (Windows controls Linux)
mwb

# Bidirectional (Linux also controls Windows)
mwb -bidi            # Wayland: no root needed (the portal handles capture)
sudo mwb -bidi       # X11: needs root/input-group to read /dev/input
```

You can also set `bidirectional = true` in `config.toml` (or toggle it in the
GUI) so the systemd service starts in bidirectional mode without any flags.

### 4. Add your Linux machine on Windows

In PowerToys MWB, enter the security key and device name to connect.

## Configuration GUI

Prefer a UI to the TOML file? Launch the built-in web GUI:

```bash
mwb gui
```

It opens a local page (`http://127.0.0.1:15199`) where you can edit every
setting, toggle bidirectional mode, and start/stop/enable the systemd user
service. Saved settings are written to `config.toml`; restart the service from
the GUI to apply them. The server binds to localhost only.

## File copy/paste

Copying a **file** on one machine and pasting it on the other works in both
directions (text and images too). Files transfer over MWB's separate clipboard
channel (the base port, `15100`); received files are saved to
`~/Downloads/MouseWithoutBorders/` and placed on the local clipboard so you can
paste them in your file manager. Requires `wl-clipboard` on Wayland (`wl-copy`/
`wl-paste`) or `xclip` on X11. Single files only (zip a folder first); 100 MB cap.

## How It Works

MWB Linux implements the full Mouse Without Borders protocol:

1. **Dual-mode connection** — Listens on port 15101 AND connects outbound (first one wins)
2. **Handshake** — AES-256-CBC encrypted challenge-response with PBKDF2-SHA512 key derivation
3. **Heartbeats** — Proactive keepalive every 5s prevents Windows from dropping the connection
4. **Edge detection** — 10ms cursor polling detects screen edges, instant switching with bounce prevention
5. **Input forwarding** — Mouse (absolute coords) and keyboard (VK codes) sent as MWB packets
6. **Device isolation** — `xinput disable/enable` prevents dual cursor movement during remote control
7. **Clipboard** — Bidirectional text/image sync via compressed clipboard packets

For detailed protocol documentation, see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Configuration

### config.toml

| Field | Default | Description |
|-------|---------|-------------|
| `host` | (required) | Windows machine IP address |
| `key` | (required) | MWB security key (from PowerToys) |
| `name` | hostname | This machine's display name |
| `port` | 15100 | Base port (message port = 15101) |
| `edge` | left | **X11 only.** Screen edge for switching: `left` or `right` |
| `edges` | all four | **Wayland only.** Which edges switch to the remote, e.g. `["left","right"]`. Unset = all four; `["none"]` disables edge switching entirely |
| `switch_modifier` | (none) | **Wayland only.** Require a held key to cross edges (PowerToys "Easy Mouse"): `shift`, `ctrl`, or `alt`. Empty = cross freely |
| `bidirectional` | false | Enable bidirectional mode from config (same as `-bidi`). Lets the GUI and systemd service turn it on without editing flags |
| `clipboard` | true | Clipboard sync: set `false` to disable text/image sharing |
| `accel_multiplier` | 2.0 | Cursor speed when controlling Windows. Lower it (e.g. `1.0`, `0.5`) if the Windows cursor feels too fast |
| `inbound_multiplier` | 1.0 | Cursor speed when Windows controls Linux. `1.0` mirrors Windows exactly; raise it for faster inbound movement |
| `keyboard_layout` | auto | Inbound Windows-to-Linux keyboard mapping. `auto` detects the local Linux layout when possible; supported profiles include `us`, `de`, `fr`, `be`, `es`, `it`, `gb`, `pt`, `no`/`dk`/`se`/`fi`, `ch`, and `nl` |

### CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-bidi` | false | Enable bidirectional input (Linux → Windows) |
| `-edge` | *(from config)* | Override edge from config: `left` or `right` |
| `-no-clipboard` | false | Disable clipboard sharing (overrides config) |
| `-debug` | false | Enable debug logging |
| `-config` | ~/.config/mwb/config.toml | Config file path |

### Windows Side Requirements

- **PowerToys** installed with Mouse Without Borders enabled
- **"Move mouse relatively"** set to **OFF** (required for bidirectional mode)
- **"Share clipboard"** set to **ON** (for clipboard sync)
- **"Block screen saver on other machines"** set to **ON** (recommended, keeps connection alive)
- Security key shared with Linux client
- Windows Firewall must allow ports **15100-15101** (TCP inbound/outbound)

## Troubleshooting

### "permission denied" on /dev/uinput
Run the setup permissions commands above, then log out and back in.

### Clipboard not syncing
Ensure the clipboard tool is installed: `wl-clipboard` on Wayland, `xclip` on X11.

### Disable clipboard sharing
Set `clipboard = false` in `config.toml`, or run with `-no-clipboard`. The Linux
client then never reads or writes the local clipboard, so it won't override what
you copied on Windows.

### Mouse controls both screens simultaneously
On Wayland the portal suppresses local input automatically. On X11 you need
`-bidi` with `sudo` (or the `input` group) so device isolation via `xinput` can run.

### Connection refused
- Check Windows firewall allows port 15100-15101
- Verify the IP address in config.toml
- Ensure PowerToys MWB is enabled on Windows

### Cursor bounces back immediately
Set "Move mouse relatively" to OFF in PowerToys MWB settings.

## Project Structure

```
cmd/mwb/
  main.go             CLI entry point + driver selection
  gui.go, web/        Local web configuration GUI (mwb gui)
internal/
  capture/            Edge detection + input forwarding
    capture_linux.go    X11 path (xdotool/xinput/evdev)
    portal_wayland.go   Wayland InputCapture portal driver (build tag: wayland)
    ei_cgo.go           libei receiver (cgo, build tag: wayland)
  clipboard/          Clipboard sync (text/image in-band) + file transfer (:15100)
  config/             TOML configuration
  input/              Virtual mouse/keyboard via uinput
  network/            TCP connection, encryption, packet send/receive
  protocol/           MWB packet types, serialization, AES-256-CBC
docs/
  ARCHITECTURE.md     Detailed protocol and architecture documentation
```

## Known Limitations

- **Keyboard on Windows lock screen** — Keyboard input may not work on the Windows lock screen (Winlogon desktop security restriction)
- **Middle mouse button auto-scroll** — Middle-click auto-scroll (scroll lock mode) does not work in browsers; normal middle-click works
- **First connection** — Initial handshake takes ~3-16s depending on Windows MWB state; subsequent reconnects are instant
- **Bidirectional mode on X11** — The default build uses `xdotool`/`xinput` for edge detection and device isolation, so bidirectional mode needs an X11 (or XWayland) session. Receive-only mode works everywhere.
- **Bidirectional mode on Wayland (opt-in build)** — A native Wayland driver uses the `org.freedesktop.portal.InputCapture` portal + libei (compositor-native edge barriers and input suppression, no `xdotool`/`xinput`, no root). It is opt-in at build time because it needs cgo + libei: `make build-wayland` (Arch: `libei`; Debian: `libei-dev`). The binary auto-selects the portal driver on Wayland and the X11 path otherwise. Requires a compositor with the InputCapture portal (GNOME 46+, KDE Plasma 6+). On Wayland the cursor can switch from **any** edge (the `edge` setting is X11-only), and no remote-resolution config is needed — coordinates are normalized like MWB itself.
- **Keyboard layout metadata** — PowerToys MWB keyboard packets carry Windows virtual-key codes and flags, but not hardware scan codes or Unicode text. MWB Linux uses `keyboard_layout` profiles for common layouts; unsupported profiles fall back to the original US-compatible mapping. Fully zero-config global layout support requires sender-side scan code or Unicode metadata.
- **Brief screen stall on return with many input devices** — Device isolation re-enables every matched device via `xinput` when control returns to Linux. On setups with many input devices (e.g. several gaming peripherals exposing 15+ `xinput` sub-devices) the compositor can stall for ~1-2s on return (the cursor keeps moving, the screen briefly freezes). An EVIOCGRAB-based isolation was tried to avoid this but introduced a worse cursor regression and was reverted; a proper fix (EVIOCGRAB done right, or libei) is tracked for a future release.
- **Cursor speed / drift** — Remote cursor movement scales raw evdev deltas by `accel_multiplier` (default 2×); lower it if the Windows cursor feels too fast (the Windows side adds no acceleration of its own). Tracking is open-loop, so the virtual cursor may still drift from the actual position over long sessions.

## Contributing

Contributions are welcome! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

### Building

```bash
make build          # Build binary (X11 bidirectional)
make build-wayland  # Build with Wayland bidirectional (cgo + libei required)
make test           # Run tests
make lint           # Run linter
make check          # All of the above
```

## Acknowledgments

- [lucky-verma/mwb-linux](https://github.com/lucky-verma/mwb-linux) — **The project this is forked from.** All of the original Linux client, the MWB protocol implementation, and the X11 bidirectional support come from there.
- [Microsoft PowerToys](https://github.com/microsoft/PowerToys) — Mouse Without Borders is part of PowerToys (MIT License). This project implements the MWB network protocol for Linux; the file-transfer and clipboard wire formats were derived from the open-source PowerToys codebase.
- [bketelsen/mwb](https://github.com/bketelsen/mwb) — Initial Go implementation of the MWB receive-only client that upstream builds upon.

## License

MIT License — see [LICENSE](LICENSE) for details.
