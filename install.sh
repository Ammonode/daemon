#!/usr/bin/env bash
# Ammonode daemon installer for Linux nodes (Ubuntu, Debian, Rocky/Alma, ...).
#
#   curl -sSL https://raw.githubusercontent.com/Ammonode/daemon/main/install.sh | sudo bash
#
# Then run the "configure" command shown on the node page of your panel and:
#   systemctl enable --now ammonode-daemon
set -euo pipefail

VERSION="${AMMONODE_DAEMON_VERSION:-latest}"
info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mError:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || fail "Run this script as root (sudo)."
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) fail "Unsupported CPU architecture: $(uname -m)" ;;
esac

if ! command -v docker >/dev/null 2>&1; then
  info "Installing Docker"
  curl -fsSL https://get.docker.com | CHANNEL=stable sh >/dev/null
fi
if [ -d /run/systemd/system ]; then
  systemctl enable --now docker >/dev/null 2>&1
fi
docker info >/dev/null 2>&1 || fail "Docker is installed but not running."

if [ "$VERSION" = "latest" ]; then
  URL="https://github.com/Ammonode/daemon/releases/latest/download/ammonode-daemon_linux_${ARCH}"
else
  URL="https://github.com/Ammonode/daemon/releases/download/${VERSION}/ammonode-daemon_linux_${ARCH}"
fi
info "Downloading ammonode-daemon ($VERSION, $ARCH)"
TMP="$(mktemp)"
curl -fsSL -o "$TMP" "$URL" || fail "Download failed: $URL"
install -m 0755 "$TMP" /usr/local/bin/ammonode-daemon
rm -f "$TMP"
/usr/local/bin/ammonode-daemon version

mkdir -p /etc/ammonode /var/lib/ammonode /var/log/ammonode
if [ -d /run/systemd/system ]; then
  info "Installing the systemd service"
  cat > /etc/systemd/system/ammonode-daemon.service <<'UNIT'
[Unit]
Description=Ammonode Daemon
After=docker.service network-online.target
Requires=docker.service
PartOf=docker.service

[Service]
User=root
WorkingDirectory=/etc/ammonode
LimitNOFILE=4096
ExecStart=/usr/local/bin/ammonode-daemon
Restart=on-failure
RestartSec=5s
StartLimitInterval=180
StartLimitBurst=30

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
fi

echo
if [ -f /etc/ammonode/config.yml ]; then
  info "Existing configuration found, restarting the daemon"
  [ -d /run/systemd/system ] && systemctl restart ammonode-daemon
else
  info "Almost done"
  echo "    1. In the panel open Admin > Nodes > your node and copy the configure command."
  echo "    2. Run it on this machine."
  echo "    3. systemctl enable --now ammonode-daemon"
fi
