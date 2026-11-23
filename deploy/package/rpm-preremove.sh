#!/bin/sh
# %preun of the .rpm package. $1 = 0 on removal, 1 on an upgrade.
set -e

if [ "$1" = 0 ]; then
	systemctl disable --now resguard.service >/dev/null 2>&1 || true
fi
