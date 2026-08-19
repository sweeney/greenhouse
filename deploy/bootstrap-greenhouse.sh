#!/usr/bin/env bash
#
# First-time bring-up of greenhouse on garibaldi. Self-contained — copy nothing
# else: it embeds the config, systemd unit and sudoers rule, and mints the Influx
# token itself. Run AS ROOT on the host:
#
#   sudo bash bootstrap-greenhouse.sh                              # prompts for what it needs
#   sudo GH_CLIENT_SECRET='<secret>' GH_SITE_ID=home \
#        GH_DEVICES_NAMESPACE=devices_home bash bootstrap-greenhouse.sh   # non-interactive
#
# Idempotent. It creates the service user, dirs, config, and systemd unit; mints
# a read-only Influx token (only if one isn't already in place); reuses the
# existing id.swee.net "greenhouse" client; and installs the deploy sudoers
# rule. It ENABLES but does not START the service — there's no binary yet. Deploy
# it from the dev machine with `make deploy`, which uploads the binary and starts
# the service.
#
set -euo pipefail

SERVICE=greenhouse
PORT=8686
ORG=swee.net
BUCKET=statehouse
CLIENT_ID=greenhouse
DEPLOY_USER="${SUDO_USER:-sweeney}"

if [ "$(id -u)" -ne 0 ]; then echo "Run as root: sudo bash $0" >&2; exit 1; fi

# Identity client secret — reused from the dev machine's "greenhouse" client.
# Supply it via the GH_CLIENT_SECRET env var, or be prompted (hidden input).
SECRET="${GH_CLIENT_SECRET:-}"
if [ -z "$SECRET" ]; then
    printf 'id.swee.net %s client_secret: ' "$CLIENT_ID"
    stty -echo 2>/dev/null || true
    read -r SECRET
    stty echo 2>/dev/null || true
    echo
fi
[ -n "$SECRET" ] || { echo "client_secret is required" >&2; exit 1; }

# The site and its devices namespace, demanded here rather than written as a placeholder.
#
# A placeholder would be worse than nothing: `devices_REPLACE_ME` is a *named* namespace,
# so greenhouse boots happily, the fetch 404s, fail-open keeps an empty snapshot, and the
# host serves zero devices — the exact failure the boot refusal exists to remove. Leaving
# it unset instead would refuse to boot, which is safe but defers the discovery to
# whenever someone next restarts the service, possibly unattended.
#
# Asking now fails while a human is watching the install, and produces a config that works.
SITE_ID="${GH_SITE_ID:-}"
if [ -z "$SITE_ID" ]; then
    printf 'site id (e.g. home): '
    read -r SITE_ID
fi
[ -n "$SITE_ID" ] || { echo "site id is required" >&2; exit 1; }

DEVICES_NAMESPACE="${GH_DEVICES_NAMESPACE:-}"
if [ -z "$DEVICES_NAMESPACE" ]; then
    printf 'devices namespace for site %s (e.g. devices_%s): ' "$SITE_ID" "$SITE_ID"
    read -r DEVICES_NAMESPACE
fi
# Not defaulted to devices_$SITE_ID: a namespace is a document that either exists or does
# not, so guessing turns a typo in the site id into a 404 and an empty snapshot rather
# than a complaint. Two facts, stated separately, checked against each other.
[ -n "$DEVICES_NAMESPACE" ] || { echo "devices namespace is required" >&2; exit 1; }

echo "=== Service user ==="
if ! id "$SERVICE" >/dev/null 2>&1; then
    useradd --system --shell /usr/sbin/nologin --home-dir "/var/lib/$SERVICE" "$SERVICE"
    echo "  created $SERVICE"
else
    echo "  $SERVICE already exists"
fi

echo "=== Directories ==="
# Binary dir is owned by the deploy user so scp from the dev machine works.
mkdir -p /opt/$SERVICE/bin
chown "$DEPLOY_USER:$DEPLOY_USER" /opt/$SERVICE/bin
echo "  /opt/$SERVICE/bin (owner $DEPLOY_USER)"
mkdir -p /var/lib/$SERVICE
chown "$SERVICE:$SERVICE" /var/lib/$SERVICE; chmod 700 /var/lib/$SERVICE
echo "  /var/lib/$SERVICE"
mkdir -p /etc/$SERVICE
chown root:$SERVICE /etc/$SERVICE; chmod 750 /etc/$SERVICE
echo "  /etc/$SERVICE"

echo "=== Influx read token ==="
if [ -s /etc/$SERVICE/influx-token ]; then
    echo "  /etc/$SERVICE/influx-token already present, skipping mint"
else
    BID=$(docker exec influxdb influx bucket list --org "$ORG" --name "$BUCKET" --json \
          | sed -nE 's/.*"id": *"([a-f0-9]+)".*/\1/p' | head -1)
    [ -n "$BID" ] || { echo "  could not resolve bucket '$BUCKET' id" >&2; exit 1; }
    echo "  bucket $BUCKET -> $BID"
    TOK=$(docker exec influxdb influx auth create --org "$ORG" --read-bucket "$BID" \
          --description "${SERVICE}-ro" --json \
          | sed -nE 's/.*"token": *"([^"]+)".*/\1/p' | head -1)
    [ -n "$TOK" ] || { echo "  token mint failed" >&2; exit 1; }
    umask 077
    printf '%s' "$TOK" > /etc/$SERVICE/influx-token
    chown root:$SERVICE /etc/$SERVICE/influx-token; chmod 640 /etc/$SERVICE/influx-token
    echo "  minted read-only token -> /etc/$SERVICE/influx-token"
fi

echo "=== Config ==="
# Guarded, as statehouse's installer is: this heredoc is the whole file, so an
# unconditional re-run silently reverts every hand edit made on the host — including
# the site block below, taking the client_secret with it. A bootstrap that undoes the
# operator's configuration is worse than one that declines to run twice.
if [ -f /etc/$SERVICE/config.yaml ]; then
echo "  /etc/$SERVICE/config.yaml exists, leaving it alone"
echo "  (delete it first if you want this script to write a fresh one)"
else
cat > /etc/$SERVICE/config.yaml <<CONFIG
# The property this instance serves, and the config namespace holding its devices.
#
# Both are required — there is no fallback. The shared pre-migration namespace this
# once defaulted to was deleted, so an unset devices_namespace means greenhouse
# refuses to start rather than booting and serving zero devices. Filled in at install
# time so this file is complete as written; a placeholder would name a namespace that
# does not exist, which boots and then serves nothing.
site:
  id: "$SITE_ID"
  devices_namespace: "$DEVICES_NAMESPACE"
  # Optional: the namespace holding floor records (name, storey order) for /floors.
  # Left empty deliberately — /floors still lists every floor that holds a climate
  # sensor, reporting name and order as unknown, so an install is complete without
  # it. Set it once the site publishes a floorplan namespace.
  floorplan_namespace: ""

http:
  listen: ":$PORT"
  public_url: "https://$SERVICE.swee.net"

influx:
  url: "http://localhost:8086"
  org: "$ORG"
  bucket: "$BUCKET"
  token_file: "/etc/$SERVICE/influx-token"

identity:
  base_url: "https://id.swee.net"
  client_id: "$CLIENT_ID"
  client_secret: "$SECRET"

remote_config:
  base_url: "https://config.swee.net"

house:
  timezone: "Europe/London"

auth:
  # Secure-by-default: identity.base_url is set above, so inbound auth is ON.
  # Leave false in production — true would let an empty base_url boot the data
  # API unauthenticated.
  allow_insecure: false
CONFIG
chown root:$SERVICE /etc/$SERVICE/config.yaml; chmod 640 /etc/$SERVICE/config.yaml
echo "  wrote /etc/$SERVICE/config.yaml (listen :$PORT)"
fi

echo "=== systemd unit ==="
# KEEP IN SYNC with deploy/greenhouse.service
cat > /etc/systemd/system/$SERVICE.service <<'UNIT'
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
systemctl enable $SERVICE >/dev/null 2>&1 || true
echo "  installed + enabled $SERVICE.service"

echo "=== Deploy sudoers rule ==="
echo "$DEPLOY_USER ALL=(ALL) NOPASSWD: /usr/bin/systemctl, /usr/bin/journalctl" > /etc/sudoers.d/$SERVICE-deploy
chmod 440 /etc/sudoers.d/$SERVICE-deploy
echo "  /etc/sudoers.d/$SERVICE-deploy"

echo ""
echo "=== Bootstrap complete ==="
echo "  Service is enabled but NOT started (no binary yet)."
echo "  From the dev machine:  make deploy   # uploads the binary and starts it on :$PORT"
