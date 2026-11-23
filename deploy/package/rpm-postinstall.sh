#!/bin/sh
# %post del paquete .rpm. $1 = 1 en una instalación nueva, 2 en una actualización.
set -e

systemd-sysusers /usr/lib/sysusers.d/resguard.conf || true
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi

if [ "$1" = 1 ]; then
	systemctl enable resguard.service >/dev/null 2>&1 || true
	if [ -d /run/systemd/system ]; then
		systemctl start resguard.service || echo "resguard: no se pudo arrancar el servicio; revisa: journalctl -u resguard" >&2
	fi
	cat <<'MSG'

resguard está instalado.
  Consultar:          resguardctl          (o: resguardctl status, resguardctl watch)
  Usarlo sin sudo:    sudo usermod -aG resguard $USER   y volver a iniciar sesión
  Configuración:      /etc/resguard/config.yaml   (aplicar: sudo systemctl reload resguard)
  Ayuda:              man resguardctl

MSG
elif [ -d /run/systemd/system ]; then
	# Actualización: solo se reinicia si el administrador lo tenía en marcha.
	systemctl try-restart resguard.service || true
fi
