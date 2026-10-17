#!/bin/sh
set -eu
umask 077

state=${MF_STATE_DIR:-/var/lib/mfconnect}
tls=${MF_TLS_DIR:-/etc/mfconnect/tls}
server=${MF_SERVER_BIN:-/usr/local/bin/mfconnect-server}
frps=${MF_FRPS_BIN:-/usr/local/bin/frps}
config="$state/server.json"

fail() { printf '%s\n' "$1" >&2; exit 1; }

validate_environment() {
    MF_ENTRY_ORIGIN=${MF_ENTRY_ORIGIN:-}
    if [ -n "$MF_ENTRY_ORIGIN" ]; then
        entry_host=$(printf '%s' "$MF_ENTRY_ORIGIN" | sed 's|^https://||')
        [ "$MF_ENTRY_ORIGIN" = "https://$entry_host" ] || fail 'MF_ENTRY_ORIGIN must be an HTTPS origin without a path.'
        MF_API_HOST=${MF_API_HOST:-$entry_host}
        MF_BASE_DOMAIN=${MF_BASE_DOMAIN:-$entry_host}
        MF_TUNNEL_HOST=${MF_TUNNEL_HOST:-$entry_host}
        MF_CONNECT_HOST=${MF_CONNECT_HOST:-$entry_host}
    fi
    : "${MF_API_HOST:?Set MF_ENTRY_ORIGIN (or legacy MF_API_HOST)}" "${MF_BASE_DOMAIN:?Set MF_BASE_DOMAIN}" "${MF_TUNNEL_HOST:?Set MF_TUNNEL_HOST}"
    export MF_ENTRY_ORIGIN MF_API_HOST MF_BASE_DOMAIN MF_TUNNEL_HOST MF_CONNECT_HOST
    MF_TUNNEL_PORT=${MF_TUNNEL_PORT:-17000}
    MF_PROXY_PORT_START=${MF_PROXY_PORT_START:-20000}
    MF_PROXY_PORT_END=${MF_PROXY_PORT_END:-20999}
    MF_TRUSTED_PROXY_IPS=${MF_TRUSTED_PROXY_IPS:-[]}
    export MF_TUNNEL_PORT MF_PROXY_PORT_START MF_PROXY_PORT_END MF_TRUSTED_PROXY_IPS
    jq -en --arg api "$MF_API_HOST" --arg base "$MF_BASE_DOMAIN" --arg tunnel "$MF_TUNNEL_HOST" \
      --arg entry "${entry_host:-}" --arg connect "${MF_CONNECT_HOST:-}" \
      --arg port "$MF_TUNNEL_PORT" --arg start "$MF_PROXY_PORT_START" --arg end "$MF_PROXY_PORT_END" \
      --argjson proxies "$MF_TRUSTED_PROXY_IPS" '
      def dns: length <= 253 and (split(".") | length > 1 and all(.[]; length <= 63 and test("^[a-z0-9]([a-z0-9-]*[a-z0-9])?$")));
      [$api, $base, $tunnel] | all(.[]; dns) and
      ((if $entry == "" then ($api != $base) and ($api | endswith("." + $base) | not) else ($entry | dns) end)) and
      ($connect == "" or ($connect | dns)) and
      ($port | test("^[1-9][0-9]{3,4}$")) and ($port | tonumber) >= 1024 and ($port | tonumber) <= 65535 and
      ($proxies | type == "array" and length <= 16 and all(.[]; type == "string")) and
      ($start | test("^[1-9][0-9]{3,4}$")) and ($end | test("^[1-9][0-9]{3,4}$")) and
      ($start | tonumber) >= 1024 and ($end | tonumber) >= ($start | tonumber) and ($end | tonumber) <= 65535 and
      (([$port, "8443", "9091"] | map(tonumber)) | unique | length == 3) and
      ([$port, "8443", "9091"] | all(.[]; (tonumber) < ($start | tonumber) or (tonumber) > ($end | tonumber)))
      ' >/dev/null || fail 'Invalid DNS names or tunnel port range in environment.'
}

validate_certificate() {
    [ -r "$tls/fullchain.pem" ] && [ -r "$tls/privkey.pem" ] || fail 'Mount readable fullchain.pem and privkey.pem in the certificate directory (container UID 10001).'
    openssl verify -CAfile "${SSL_CERT_FILE:-/etc/ssl/certs/ca-certificates.crt}" \
      -untrusted "$tls/fullchain.pem" -purpose sslserver -verify_hostname "$MF_API_HOST" \
      "$tls/fullchain.pem" >/dev/null || fail 'Certificate is not valid and trusted for the API hostname.'
    openssl verify -CAfile "${SSL_CERT_FILE:-/etc/ssl/certs/ca-certificates.crt}" \
      -untrusted "$tls/fullchain.pem" -purpose sslserver -verify_hostname "$MF_TUNNEL_HOST" \
      "$tls/fullchain.pem" >/dev/null || fail 'Certificate must also be valid for the FRP tunnel hostname.'
    openssl x509 -in "$tls/fullchain.pem" -noout -ext subjectAltName \
      | tr ',' '\n' | sed 's/^[[:space:]]*//' | grep -Fx "DNS:*.$MF_BASE_DOMAIN" >/dev/null \
      || fail 'Certificate must include the application wildcard DNS name.'
    # Legacy installations may terminate browser TLS at nginx with a separate
    # certificate and use API SNI upstream. Preserve that mode until migration.
    if [ -n "$MF_ENTRY_ORIGIN" ]; then
    for domain in "${entry_host:-}" "${MF_CONNECT_HOST:-}"; do
        [ -n "$domain" ] || continue
        openssl verify -CAfile "${SSL_CERT_FILE:-/etc/ssl/certs/ca-certificates.crt}" \
          -untrusted "$tls/fullchain.pem" -purpose sslserver -verify_hostname "$domain" "$tls/fullchain.pem" >/dev/null \
          || fail 'Certificate must cover each configured browser entry itself.'
        openssl x509 -in "$tls/fullchain.pem" -noout -ext subjectAltName \
          | tr ',' '\n' | sed 's/^[[:space:]]*//' | grep -Fx "DNS:*.$domain" >/dev/null \
          || fail 'Certificate must also cover each configured browser wildcard.'
    done
    fi
    cert_tmp=$(mktemp -d)
    trap 'rm -rf "$cert_tmp"' EXIT HUP INT TERM
    openssl x509 -in "$tls/fullchain.pem" -pubkey -noout >"$cert_tmp/cert.pub"
    openssl pkey -in "$tls/privkey.pem" -passin pass: -pubout >"$cert_tmp/key.pub" \
      || fail 'Cannot read an unencrypted TLS private key.'
    cmp -s "$cert_tmp/cert.pub" "$cert_tmp/key.pub" || fail 'TLS certificate and private key do not match.'
    rm -rf "$cert_tmp"
    trap - EXIT HUP INT TERM
}

validate_config() {
    [ -f "$config" ] || fail 'State is not initialized. Run: docker compose run --rm mfconnect init'
    jq -e --arg api "$MF_API_HOST" --arg base "$MF_BASE_DOMAIN" --arg tunnel "$MF_TUNNEL_HOST" \
      --arg entry "$MF_ENTRY_ORIGIN" --arg state "$state" --arg tls "$tls" --arg frps "$frps" \
      --argjson port "$MF_TUNNEL_PORT" --argjson start "$MF_PROXY_PORT_START" --argjson end "$MF_PROXY_PORT_END" \
      --argjson proxies "$MF_TRUSTED_PROXY_IPS" '
      (.entry_origin // "") == $entry and .api_host == $api and .base_domain == $base and .tunnel_host == $tunnel and
      .listen == "0.0.0.0:8443" and .hook_listen == "127.0.0.1:9091" and .proxy_host == "127.0.0.1" and
      .database == ($state + "/state.db") and
      .certificate == ($tls + "/fullchain.pem") and .private_key == ($tls + "/privkey.pem") and
      .easytier == null and .frp.binary == $frps and .frp.run_dir == ($state + "/run") and
      .frp.listen_host == "0.0.0.0" and .tunnel_port == $port and .port_start == $start and .port_end == $end and
      (.trusted_proxy_ips // []) == $proxies
      ' "$config" >/dev/null || fail 'Existing configuration differs from the container environment. Stop the service and review the configuration; existing state will not be reset.'
}

case "${1:-serve}" in
    version) exec "$server" version ;;
    init)
        [ "$#" -eq 1 ] || fail 'init takes its settings from the environment, without extra arguments.'
        validate_environment
        [ ! -e "$config" ] && [ ! -e "$state/state.db" ] && [ ! -e "$state/admin-token" ] \
          || fail 'Existing or partially initialized state found; refusing to overwrite it.'
        validate_certificate
        set --
        if [ -n "$MF_ENTRY_ORIGIN" ]; then set -- --entry "$MF_ENTRY_ORIGIN"; fi
        if [ -n "${MF_CONNECT_HOST:-}" ]; then set -- "$@" --connect-host "$MF_CONNECT_HOST"; fi
        "$server" init "$@" --dir "$state" --api-host "$MF_API_HOST" --base-domain "$MF_BASE_DOMAIN" \
          --tunnel-host "$MF_TUNNEL_HOST" --transport frp --frps "$frps"
        temp_config=$(mktemp "$state/server.json.XXXXXX")
        trap 'rm -f "$temp_config"' EXIT HUP INT TERM
        jq --arg tls "$tls" --argjson port "$MF_TUNNEL_PORT" --argjson start "$MF_PROXY_PORT_START" --argjson end "$MF_PROXY_PORT_END" \
          --argjson proxies "$MF_TRUSTED_PROXY_IPS" '
          .listen = "0.0.0.0:8443" | .certificate = ($tls + "/fullchain.pem") |
          .private_key = ($tls + "/privkey.pem") |
          .tunnel_port = $port | .port_start = $start | .port_end = $end | .trusted_proxy_ips = $proxies
          ' "$config" >"$temp_config"
        chmod 0600 "$temp_config"
        mv "$temp_config" "$config"
        trap - EXIT HUP INT TERM
        validate_config
        printf '%s\n' 'Container configuration ready. Operator credential remains in the private data volume.'
        ;;
    serve)
        [ "$#" -le 1 ] || fail 'serve takes its settings from the environment, without extra arguments.'
        validate_environment
        validate_config
        validate_certificate
        exec "$server" serve --dir "$state"
        ;;
    issue-user)
        shift
        validate_environment
        validate_config
        exec "$server" issue-user --dir "$state" "$@"
        ;;
    *) fail 'Supported commands: init, serve, issue-user, version' ;;
esac
