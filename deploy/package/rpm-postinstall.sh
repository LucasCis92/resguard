#!/bin/sh
# %post of the .rpm package. $1 = 1 on a fresh install, 2 on an upgrade.
set -e

systemd-sysusers /usr/lib/sysusers.d/resguard.conf || true
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi

if [ "$1" = 1 ]; then
	systemctl enable resguard.service >/dev/null 2>&1 || true
	if [ -d /run/systemd/system ]; then
		systemctl start resguard.service || echo "resguard: the service could not start; check: journalctl -u resguard" >&2
	fi
	cat <<'MSG'

resguard is installed.
  Query it:           resguardctl          (or: resguardctl status, resguardctl watch)
  Use it without sudo: sudo usermod -aG resguard $USER   and log in again
  Configuration:      /etc/resguard/config.yaml   (apply: sudo systemctl reload resguard)
  Help:               man resguardctl

MSG
elif [ -d /run/systemd/system ]; then
	# Upgrade: only restart it if the admin had it running.
	systemctl try-restart resguard.service || true
fi
