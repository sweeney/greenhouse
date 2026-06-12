#!/usr/bin/env bash
set -euo pipefail
echo 'sweeney ALL=(ALL) NOPASSWD: /usr/bin/systemctl, /usr/bin/journalctl' > /etc/sudoers.d/greenhouse-deploy
chmod 440 /etc/sudoers.d/greenhouse-deploy
echo "Installed /etc/sudoers.d/greenhouse-deploy"
