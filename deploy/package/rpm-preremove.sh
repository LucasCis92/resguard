#!/bin/sh
# %preun del paquete .rpm. $1 = 0 al desinstalar, 1 al actualizar.
set -e

if [ "$1" = 0 ]; then
	systemctl disable --now resguard.service >/dev/null 2>&1 || true
fi
