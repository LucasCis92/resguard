#!/bin/sh
# %postun of the .rpm package. $1 = 0 on removal, 1 on an upgrade.
# rpm has no "purge": a modified configuration is saved as
# /etc/resguard/config.yaml.rpmsave and the incident log is kept.
set -e

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi
