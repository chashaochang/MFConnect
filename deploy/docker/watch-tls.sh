#!/bin/sh
# Only the service process in this container is restarted. No Docker socket.
set -eu
tls=${MF_TLS_DIR:-/etc/mfconnect/tls}
entry=${MF_ENTRYPOINT:-/usr/local/bin/mfconnect-entrypoint}
child=
stop() {
    if [ -n "$child" ]; then
        kill -TERM "$child" 2>/dev/null || true
        wait "$child" 2>/dev/null || true
    fi
}
trap 'stop; exit 0' TERM INT
fingerprint() {
    directory=$tls
    if [ -L "$tls/live" ]; then directory=$(readlink -f "$tls/live"); fi
    sha256sum "$directory/fullchain.pem" "$directory/privkey.pem" | sha256sum
}
previous=$(fingerprint)
MF_WATCH_TLS=0 "$entry" serve &
child=$!
while :; do
    if ! kill -0 "$child" 2>/dev/null; then
        result=0
        wait "$child" || result=$?
        exit "$result"
    fi
    sleep 5 &
    wait $! || true
    current=$(fingerprint)
    if [ "$current" != "$previous" ]; then
        stop
        previous=$current
        MF_WATCH_TLS=0 "$entry" serve &
        child=$!
        printf '%s\n' 'TLS files changed; restarted the API and its managed FRP process.'
    fi
done
