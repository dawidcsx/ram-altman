#!/usr/bin/env bash
# RAM Altman Linux installer. All files are installed in the current user's home.
# No sudo or system-wide services are required.
set -euo pipefail

SOURCE_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
case "$(uname -s):$(uname -m)" in
  Linux:x86_64) BIN="$SOURCE_DIR/bin/ram-altman-linux-amd64" ;;
  Linux:aarch64|Linux:arm64) BIN="$SOURCE_DIR/bin/ram-altman-linux-arm64" ;;
  *) echo 'Supported: Linux x86_64 (AMD64) and aarch64 (ARM64).' >&2; exit 1 ;;
esac
[[ -f "$BIN" ]] || { echo "Missing binary: $BIN" >&2; exit 1; }
command -v systemctl >/dev/null || { echo 'systemd is required for the optional user service.' >&2; exit 1; }

BIN_DIR="$HOME/.local/bin"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
DESKTOP_DIR="$HOME/.local/share/applications"
ICON_DIR="$HOME/.local/share/icons/hicolor/scalable/apps"
mkdir -p "$BIN_DIR" "$UNIT_DIR" "$DESKTOP_DIR" "$ICON_DIR"

# Graceful upgrade: preserve the opt-in decision, endpoint, anonymous ID, and
# autostart preference in the existing settings.json.
if systemctl --user --quiet is-active ram-altman.service 2>/dev/null; then
  systemctl --user stop ram-altman.service
fi
install -m 0755 "$BIN" "$BIN_DIR/ram-altman"
cat > "$UNIT_DIR/ram-altman.service" <<'UNIT'
[Unit]
Description=RAM Altman opt-in global RAM wastage agent
Documentation=https://mochisoftware.com/ram-altman
After=network.target

[Service]
Type=simple
ExecStart=%h/.local/bin/ram-altman run
Restart=on-failure
RestartSec=5
NoNewPrivileges=true

[Install]
WantedBy=default.target
UNIT
cat > "$DESKTOP_DIR/ram-altman.desktop" <<DESKTOP
[Desktop Entry]
Type=Application
Version=1.0
Name=RAM Altman
Comment=Your RAM. Your computer. Sam's disappointment.
Exec=$HOME/.local/bin/ram-altman open
Icon=ram-altman
Terminal=false
Categories=Utility;System;
StartupNotify=false
DESKTOP
install -m 0644 "$SOURCE_DIR/ram-altman.svg" "$ICON_DIR/ram-altman.svg"
systemctl --user daemon-reload
systemctl --user start ram-altman.service
printf '\nRAM Altman Linux is installed. Your RAM reports are OFF by default.\n'
printf 'Open settings:  %s/.local/bin/ram-altman open\n' "$HOME"
printf 'Check status:   systemctl --user status ram-altman\n'
printf 'Uninstall:      %s/.local/bin/ram-altman uninstall\n' "$HOME"
printf 'Autostart is OFF by default, unless you already enabled it in a previous installation.\n'
if [[ -n "${DISPLAY:-}" || -n "${WAYLAND_DISPLAY:-}" ]]; then
  "$BIN_DIR/ram-altman" open || true
else
  printf 'No graphical display detected; run ram-altman open from a desktop session.\n'
fi
