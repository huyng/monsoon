# monsoon

A lightweight screen sharing server for Linux. Point a browser at it and watch
the desktop live — no plugins, no accounts, no WebRTC negotiation, no external
services.

## Why monsoon?

Most screen sharing tools are built around accounts, installers, proprietary
clients, or cloud infrastructure that your traffic has to pass through. Monsoon
runs entirely on your machine. It is a single self-contained binary that any
browser on your network can connect to immediately.

Under the hood it uses X11 MIT-SHM to capture frames with zero pixel copies,
then diffs each frame against the previous one at the tile level and ships only
what changed. Tiles are JPEG-encoded and packed into a compact binary stream
delivered over plain HTTP chunked transfer — no WebSocket, no plugin, just
`fetch()` and a `<canvas>`. At 10 fps a mostly-static desktop typically
transfers less than 50 KB/s.

Good for:
- Sharing your screen with someone on your local network instantly
- Remote pair programming without installing anything on either side
- Monitoring a headless Linux machine from a phone or tablet browser
- Any situation where standing up a full video-conferencing stack is overkill

## Requirements

- Linux with an X11 display server
- Go 1.22 or newer
- `libX11` and `libXext` development headers (for MIT-SHM support)

## Installation

Install the system dependencies:

```bash
# Debian / Ubuntu
sudo apt install libx11-dev libxext-dev

# Fedora / RHEL
sudo dnf install libX11-devel libXext-devel

# Arch
sudo pacman -S libx11 libxext
```

Install the binary:

```bash
go install github.com/huyng/monsoon@latest
```

## Usage

Start the server:

```bash
monsoon
```

Then open `http://<your-ip>:8080` in any browser on the same network.

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-d` | `:0.0` | X display to capture |
| `-p` | `8080` | HTTP port |
| `-w` | `1280` | Output width in pixels (height scales proportionally) |
| `-r` | `10` | Capture frame rate |

### Examples

```bash
# Stream at 1920px wide, 24 fps, on port 9000
monsoon -w 1920 -r 24 -p 9000

# Stream a secondary display
monsoon -d :0.1

# Lower bandwidth — smaller output, slower rate
monsoon -w 960 -r 5
```
