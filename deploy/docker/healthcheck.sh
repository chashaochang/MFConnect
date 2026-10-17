#!/bin/sh
set -eu
if [ -n "${MF_ENTRY_ORIGIN:-}" ]; then
    host=${MF_ENTRY_ORIGIN#https://}; path=/api/healthz
else
    host=${MF_API_HOST:?Set MF_ENTRY_ORIGIN or MF_API_HOST}; path=/healthz
fi
state=${MF_STATE_DIR:-/var/lib/mfconnect}
pid=$(jq -er 'select(.state == "running") | .process_id | select(type == "number" and . > 0)' "$state/run/frps-status.json")
kill -0 "$pid"
exec curl --noproxy '*' --fail --silent --show-error --max-time 5 \
  --resolve "$host:8443:127.0.0.1" "https://$host:8443$path" --output /dev/null
