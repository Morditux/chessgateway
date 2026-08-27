#!/usr/bin/env bash
# Build a .deb package for ChessGateway. Installing the package deploys the
# systemd service automatically (enabled and started by the postinst script).
#
# Usage:
#   ./deploy/build-deb.sh
#
# Environment overrides:
#   VERSION=1.2.3  ARCH=arm64  MAINTAINER="Name <email>"
#
# Requires: go, dpkg-deb. Produces chessgateway_<version>_<arch>.deb in the
# repository root.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${VERSION:-$(git -C "$REPO_DIR" describe --tags --always 2>/dev/null | sed 's/^v//' || true)}"
VERSION="${VERSION:-1.0.0}"
# A bare commit hash does not start with a digit, which dpkg rejects.
if [[ ! "$VERSION" =~ ^[0-9] ]]; then
  VERSION="0.0.0-$VERSION"
fi
ARCH="${ARCH:-amd64}"
MAINTAINER="${MAINTAINER:-ChessGateway developers}"
OUT="$REPO_DIR/chessgateway_${VERSION}_${ARCH}.deb"

command -v dpkg-deb >/dev/null || { echo "dpkg-deb is required" >&2; exit 1; }

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

echo "building binary (version $VERSION)..."
(cd "$REPO_DIR" && CGO_ENABLED=0 go build -o "$STAGE/usr/bin/chessgateway" ./cmd/chessgateway)

install -d "$STAGE/etc/chessgateway" \
  "$STAGE/etc/sysctl.d" \
  "$STAGE/lib/systemd/system" \
  "$STAGE/DEBIAN"

install -m 0640 "$REPO_DIR/config.example.json" "$STAGE/etc/chessgateway/config.json"
install -m 0644 "$REPO_DIR/deploy/chessgateway.service" "$STAGE/lib/systemd/system/"
install -m 0644 "$REPO_DIR/deploy/sysctl.d/99-chessgateway.conf" "$STAGE/etc/sysctl.d/"

cat >"$STAGE/DEBIAN/control" <<EOF
Package: chessgateway
Version: $VERSION
Section: net
Priority: optional
Architecture: $ARCH
Recommends: stockfish
Maintainer: $MAINTAINER
Description: TCP gateway between chess clients and UCI engine processes
 ChessGateway is a TCP/TLS gateway in Go between a chess GUI and one or more
 UCI-compatible engines. Each client connection owns its own engine process:
 several players can search in parallel without sharing UCI state.
 .
 Installing this package creates the chessgateway system user, installs the
 systemd service and enables and starts it automatically. The listen address
 and the engine whitelist are configured in /etc/chessgateway/config.json.
EOF

cat >"$STAGE/DEBIAN/conffiles" <<EOF
/etc/chessgateway/config.json
EOF

cat >"$STAGE/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e

case "$1" in
  configure)
    if ! getent passwd chessgateway >/dev/null; then
      if command -v useradd >/dev/null 2>&1; then
        useradd --system --no-create-home --shell /usr/sbin/nologin chessgateway
      else
        adduser --system --no-create-home --shell /usr/sbin/nologin chessgateway
      fi
    fi
    chown -R chessgateway:chessgateway /etc/chessgateway

    if [ -f /etc/sysctl.d/99-chessgateway.conf ]; then
      sysctl -p /etc/sysctl.d/99-chessgateway.conf >/dev/null 2>&1 || true
    fi

    if command -v deb-systemd-invoke >/dev/null 2>&1; then
      deb-systemd-invoke enable chessgateway.service
      deb-systemd-invoke start chessgateway.service || true
    else
      systemctl enable chessgateway.service || true
      systemctl start chessgateway.service || true
    fi
    ;;
esac
exit 0
EOF

cat >"$STAGE/DEBIAN/prerm" <<'EOF'
#!/bin/sh
set -e

case "$1" in
  remove|upgrade|deconfigure)
    if command -v deb-systemd-invoke >/dev/null 2>&1; then
      deb-systemd-invoke stop chessgateway.service >/dev/null 2>&1 || true
    else
      systemctl stop chessgateway.service >/dev/null 2>&1 || true
    fi
    ;;
esac
exit 0
EOF

cat >"$STAGE/DEBIAN/postrm" <<'EOF'
#!/bin/sh
set -e

if [ "$1" = "purge" ]; then
  rm -f /etc/sysctl.d/99-chessgateway.conf
  rm -rf /etc/chessgateway
  if getent passwd chessgateway >/dev/null 2>&1; then
    userdel chessgateway 2>/dev/null || true
  fi
fi
exit 0
EOF

chmod 0755 "$STAGE/DEBIAN/postinst" "$STAGE/DEBIAN/prerm" "$STAGE/DEBIAN/postrm"

echo "building $OUT..."
dpkg-deb --build --root-owner-group "$STAGE" "$OUT"
echo "done: $OUT"
