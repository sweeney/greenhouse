#!/usr/bin/env bash
#
# One-time install for greenhouse on the target host.
# Run as root or with sudo — no repo checkout required.
#
# Usage (from dev machine):
#   scp deploy/install.sh sweeney@garibaldi:/tmp/
#   ssh -t sweeney@garibaldi sudo sh /tmp/install.sh
#
set -euo pipefail

SERVICE=greenhouse
DEPLOY_USER="${SUDO_USER:-sweeney}"

echo "=== Creating user ==="
if ! id "$SERVICE" >/dev/null 2>&1; then
    useradd --system --shell /usr/sbin/nologin --home-dir "/var/lib/$SERVICE" "$SERVICE"
    echo "  Created: $SERVICE"
else
    echo "  User already exists"
fi

echo "=== Creating directories ==="
# Binary dir is owned by the deploy user so scp from the dev machine works.
mkdir -p /opt/$SERVICE/bin
chown "$DEPLOY_USER:$DEPLOY_USER" /opt/$SERVICE/bin
echo "  /opt/$SERVICE/bin (owner $DEPLOY_USER)"

mkdir -p /var/lib/$SERVICE
chown $SERVICE:$SERVICE /var/lib/$SERVICE
chmod 700 /var/lib/$SERVICE
echo "  /var/lib/$SERVICE"

mkdir -p /etc/$SERVICE
chown root:$SERVICE /etc/$SERVICE
chmod 750 /etc/$SERVICE
echo "  /etc/$SERVICE"

echo "=== Installing config ==="
if [ ! -f /etc/$SERVICE/config.yaml ]; then
    cat > /etc/$SERVICE/config.yaml << 'CONFIG'
http:
  listen: ":8082"
  public_url: "https://greenhouse.swee.net"

influx:
  url: "http://localhost:8086"
  org: "swee.net"
  bucket: "statehouse"
  token_file: "/etc/greenhouse/influx-token"

identity:
  base_url: "https://id.swee.net"
  client_id: "REPLACE_ME"
  client_secret: "REPLACE_ME"

remote_config:
  base_url: "https://config.swee.net"

house:
  timezone: "Europe/London"
CONFIG
    chown root:$SERVICE /etc/$SERVICE/config.yaml
    chmod 640 /etc/$SERVICE/config.yaml
    echo "  Installed /etc/$SERVICE/config.yaml"
else
    echo "  /etc/$SERVICE/config.yaml already exists, skipping"
fi

echo "=== Installing systemd unit ==="
# KEEP IN SYNC with deploy/greenhouse.service
cat > /etc/systemd/system/$SERVICE.service << 'UNIT'
[Unit]
Description=Read-side climate/environment reporting service
After=network-online.target
Wants=network-online.target
Documentation=https://github.com/sweeney/greenhouse

[Service]
Type=simple
User=greenhouse
Group=greenhouse

ExecStart=/opt/greenhouse/bin/greenhouse
WorkingDirectory=/var/lib/greenhouse

Restart=always
RestartSec=5

# Hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/greenhouse
ReadOnlyPaths=/etc/greenhouse
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
MemoryDenyWriteExecute=true
LockPersonality=true

StandardOutput=journal
StandardError=journal
SyslogIdentifier=greenhouse

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable $SERVICE
echo "  Installed and enabled greenhouse.service"

echo ""
echo "=== Next steps ==="
echo ""
echo "  (a) Place the Influx READ token (scoped to the 'statehouse' bucket) at"
echo "      /etc/greenhouse/influx-token, then lock it down:"
echo "        docker exec influxdb influx auth create --org swee.net --read-bucket <statehouse-bucket-id>"
echo "        sudo install -o root -g greenhouse -m 640 <token-file> /etc/greenhouse/influx-token"
echo "      (find the bucket id with: docker exec influxdb influx bucket list --org swee.net)"
echo ""
echo "  (b) Fill identity.client_id / identity.client_secret in"
echo "      /etc/greenhouse/config.yaml (register the client in id.swee.net)."
echo ""
echo "  (c) Install the deploy sudoers rule:"
echo "        ssh -t sweeney@garibaldi sudo sh /tmp/sudoers.sh"
echo ""
echo "  (d) Deploy the binary from the dev machine:"
echo "        make deploy"
echo ""
echo "=== Done ==="
echo "  Check status:  sudo systemctl status greenhouse"
echo "  View logs:     sudo journalctl -u greenhouse -f"
