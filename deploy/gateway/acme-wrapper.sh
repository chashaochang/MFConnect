#!/bin/sh
exec /opt/acme/acme.sh --home /opt/acme --config-home /state/acme --cert-home /state/acme-certs "$@"
