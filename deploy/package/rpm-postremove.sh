#!/bin/sh
# %postun del paquete .rpm. $1 = 0 al desinstalar, 1 al actualizar.
# rpm no tiene "purge": la configuración modificada se guarda como
# /etc/resguard/config.yaml.rpmsave y el registro de incidencias se conserva.
set -e

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi
