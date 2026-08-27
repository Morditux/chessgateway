#!/usr/bin/env bash
# Install (or remove) the ChessGateway systemd service.
#
# Usage:
#   sudo ./deploy/install.sh [--start]       # build + install + enable; --start also starts it
#   sudo ./deploy/install.sh --uninstall     # stop, disable and remove the service
#
# The configuration in /etc/chessgateway/config.json is never overwritten.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DEST=/usr/bin/chessgateway
CONFIG_DIR=/etc/chessgateway
CONFIG_DEST="$CONFIG_DIR/config.json"
SERVICE_DEST=/etc/systemd/system/chessgateway.service
SYSCTL_DEST=/etc/sysctl.d/99-chessgateway.conf
SERVICE_USER=chessgateway
START_SERVICE=0
MODE=install

for arg in "$@"; do
  case "$arg" in
    --start) START_SERVICE=1 ;;
    --uninstall) MODE=uninstall ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

if [[ $EUID -ne 0 ]]; then
  echo "run as root: sudo $0 $*" >&2
  exit 1
fi

uninstall() {
  systemctl disable --now chessgateway.service 2>/dev/null || true
  rm -f "$SERVICE_DEST" "$SYSCTL_DEST" "$BIN_DEST"
  systemctl daemon-reload
  echo "ChessGateway service removed. /etc/chessgateway and user $SERVICE_USER kept."
}

if [[ $MODE == uninstall ]]; then
  uninstall
  exit 0
fi

echo "building binary..."
(cd "$REPO_DIR" && go build -o chessgateway ./cmd/chessgateway)
install -m 0755 "$REPO_DIR/chessgateway" "$BIN_DEST"

if ! getent passwd "$SERVICE_USER" >/dev/null; then
  echo "creating user $SERVICE_USER..."
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi

install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$CONFIG_DIR"
if [[ ! -f "$CONFIG_DEST" ]]; then
  echo "installing default config to $CONFIG_DEST (edit it to add your engines)..."
  install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0640 \
    "$REPO_DIR/config.example.json" "$CONFIG_DEST"
fi

install -m 0644 "$REPO_DIR/deploy/chessgateway.service" "$SERVICE_DEST"
install -m 0644 "$REPO_DIR/deploy/sysctl.d/99-chessgateway.conf" "$SYSCTL_DEST"

echo "applying sysctl settings..."
sysctl -p "$SYSCTL_DEST" >/dev/null

systemctl daemon-reload
systemctl enable chessgateway.service
if [[ $START_SERVICE -eq 1 ]]; then
  systemctl start chessgateway.service
  echo "ChessGateway started: systemctl status chessgateway"
else
  echo "ChessGateway installed and enabled: sudo systemctl start chessgateway"
fi
