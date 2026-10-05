# Installs errand on a leased Lambda machine. The cloud peer unpacks this
# script into a fresh directory together with everything it needs, and runs
# it as root through sudo. Every value comes from a file in that directory,
# so nothing is ever quoted into this script.
set -euo pipefail
dir=$(cd "$(dirname "$0")" && pwd)
# The directory holds the tailnet auth key; it goes however this ends.
trap 'rm -rf "$dir"' EXIT
cd "$dir"

install -m 0755 errand /usr/local/bin/errand
install -D -m 0644 errandd.toml /etc/errand/errandd.toml
command -v tailscale >/dev/null || curl -fsSL https://tailscale.com/install.sh | sh
tailscale up --auth-key="file:$dir/tailscale-auth-key" --hostname="$(cat hostname)"

cat > /etc/systemd/system/errand.service <<'UNIT'
[Unit]
Description=errand runner (leased)
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/errand serve --config /etc/errand/errandd.toml
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
# The runner runs as the login the cloud peer installed with, an account
# this machine already has.
mkdir -p /etc/systemd/system/errand.service.d
printf '[Service]\nUser=%s\n' "$SUDO_USER" > /etc/systemd/system/errand.service.d/user.conf
systemctl daemon-reload
systemctl enable --now errand.service
