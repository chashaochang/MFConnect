#!/bin/sh
# Plain Docker lifecycle, with the same images and isolation as Compose.
set -eu
umask 077
fail() { printf '%s\n' "$1" >&2; exit 1; }
cd "$(dirname "$0")"
action=${1:-up}
case "$action" in up|update|recreate|down|status|logs) ;; *) fail 'Usage: sh docker-run.sh [up|update|recreate|down|status|logs]';; esac
[ -f .env ] || fail 'Missing .env; run the configuration wizard or copy .env.example.'
# Parse values as data, never source/eval deployment configuration.
MF_ENTRY_ORIGIN=
MF_TLS_MODE=provided
MF_DNS_PROVIDER=
MF_HTTP_BIND=0.0.0.0:80
MF_HTTPS_BIND=0.0.0.0:443
MF_TUNNEL_PORT=17000
MF_NETWORK_SUBNET=172.31.253.0/28
MF_GATEWAY_IP=172.31.253.2
MF_SERVER_IMAGE=ghcr.io/chashaochang/mfconnect-server:0.7.2-docker.1
MF_GATEWAY_IMAGE=ghcr.io/chashaochang/mfconnect-gateway:0.7.2
MF_DOCKER_PROJECT=mfconnect-docker
MF_INSTALL_DEVICE_LIMIT=10
MF_UPDATE_OWNER=
MF_UPDATE_DEVICE=
while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in ''|'#'*) continue;; *=*) ;; *) fail 'Invalid .env line; use KEY=value without quotes.';; esac
    key=${line%%=*}; value=${line#*=}
    case "$key" in
        MF_ENTRY_ORIGIN|MF_TLS_MODE|MF_DNS_PROVIDER|MF_HTTP_BIND|MF_HTTPS_BIND|MF_TUNNEL_PORT|MF_NETWORK_SUBNET|MF_GATEWAY_IP|MF_SERVER_IMAGE|MF_GATEWAY_IMAGE|MF_DOCKER_PROJECT|MF_INSTALL_DEVICE_LIMIT|MF_UPDATE_OWNER|MF_UPDATE_DEVICE)
            export "$key=$value";;
        *) fail "Unknown .env field: $key";;
    esac
done < .env
case "$MF_DOCKER_PROJECT" in ''|*[!a-z0-9-]*) fail 'MF_DOCKER_PROJECT must contain only lowercase letters, digits and hyphens.';; esac
case "$MF_ENTRY_ORIGIN" in https://?*) ;; *) fail 'Set MF_ENTRY_ORIGIN to your HTTPS entry.';; esac
docker info >/dev/null
project=$MF_DOCKER_PROJECT
gateway=$project-gateway
server=$project-server
network=$project-network
label=io.mfconnect.docker-project
managed() {
    actual=$(docker "$1" inspect --format "{{index .Labels \"$label\"}}" "$2" 2>/dev/null) || return 1
    [ "$actual" = "$project" ] || fail "Refusing to modify an unrelated resource: $2"
}
container_exists() {
    actual=$(docker container inspect --format "{{index .Config.Labels \"$label\"}}" "$1" 2>/dev/null) || return 1
    [ "$actual" = "$project" ] || fail "Refusing to modify an unrelated container: $1"
}
wait_healthy() {
    attempt=0
    until [ "$(docker inspect --format '{{.State.Health.Status}}' "$1")" = healthy ]; do
        attempt=$((attempt + 1))
        [ "$attempt" -lt 120 ] || fail 'Service is not ready. Run sh docker-run.sh logs; correct settings, then run update.'
        state=$(docker inspect --format '{{.State.Status}}' "$1")
        case "$state" in exited|dead|restarting) fail 'Service failed to start. Run sh docker-run.sh logs.';; esac
        sleep 5
    done
}
for name in "$gateway" "$server"; do
    if container_exists "$name"; then :; fi
done
case "$action" in
    status) docker ps -a --filter "label=$label=$project"; exit;;
    logs)
        for name in "$gateway" "$server"; do if container_exists "$name"; then docker logs --tail=100 "$name"; fi; done
        exit;;
    down)
        for name in "$server" "$gateway"; do
            if container_exists "$name"; then docker stop --time=120 "$name"; docker rm "$name"; fi
        done
        printf '%s\n' 'Containers removed; configuration, network and all data volumes retained.'
        exit;;
esac
# Fetch before stopping any existing service. Offline up uses loaded images.
for image in "$MF_GATEWAY_IMAGE" "$MF_SERVER_IMAGE"; do
    if [ "$action" = update ] || ! docker image inspect "$image" >/dev/null 2>&1; then docker pull "$image"; fi
done
if [ "$action" = up ]; then
    if container_exists "$gateway" && container_exists "$server"; then
        for name in "$gateway" "$server"; do docker start "$name"; wait_healthy "$name"; done
        printf '%s\n' 'Existing containers retained. To apply changed images/configuration: sh docker-run.sh update'
        exit
    elif container_exists "$gateway" || container_exists "$server"; then
        fail 'Partial deployment found. Check logs and settings, then run sh docker-run.sh update; data is retained.'
    fi
fi
if managed network "$network"; then
    current=$(docker network inspect --format '{{(index .IPAM.Config 0).Subnet}}' "$network")
    [ "$current" = "$MF_NETWORK_SUBNET" ] || fail 'Existing Docker subnet differs; migrate explicitly instead of replacing it.'
else
    docker network create --label "$label=$project" --subnet "$MF_NETWORK_SUBNET" "$network" >/dev/null
fi
for volume in gateway tls updates data; do
    if ! managed volume "$project-$volume"; then docker volume create --label "$label=$project" "$project-$volume" >/dev/null; fi
done
[ -d config ] || fail 'Missing config directory.'
if [ "$action" = update ] || [ "$action" = recreate ]; then
    for name in "$server" "$gateway"; do
        if container_exists "$name"; then docker stop --time=120 "$name"; docker rm "$name"; fi
    done
fi
docker run -d --name "$gateway" --label "$label=$project" \
    --platform linux/amd64 --init --restart unless-stopped --stop-timeout 120 \
    --read-only --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
    --cap-add SETUID --cap-add SETGID --cap-add NET_BIND_SERVICE --security-opt no-new-privileges:true \
    --tmpfs /tmp:rw,noexec,nosuid,size=64m,mode=1777 --tmpfs /run:rw,noexec,nosuid,size=8m,mode=0755 \
    --network "$network" --ip "$MF_GATEWAY_IP" --network-alias gateway \
    --publish "$MF_HTTP_BIND:80/tcp" --publish "$MF_HTTPS_BIND:443/tcp" \
    --volume "$project-gateway:/state" --volume "$project-tls:/state/certs" \
    --volume "$project-updates:/updates" --volume "$PWD/config:/config:ro,z" \
    --env "MF_ENTRY_ORIGIN=$MF_ENTRY_ORIGIN" --env "MF_TLS_MODE=$MF_TLS_MODE" --env "MF_DNS_PROVIDER=$MF_DNS_PROVIDER" \
    --health-cmd 'python3 /opt/mfconnect/gateway.py health' --health-interval 10s --health-timeout 5s --health-retries 60 \
    --log-driver json-file --log-opt max-size=10m --log-opt max-file=3 "$MF_GATEWAY_IMAGE" >/dev/null
wait_healthy "$gateway"
docker run -d --name "$server" --label "$label=$project" \
    --platform linux/amd64 --init --restart unless-stopped --stop-timeout 60 \
    --user 10001:10001 --read-only --cap-drop ALL --security-opt no-new-privileges:true \
    --tmpfs /tmp:rw,noexec,nosuid,size=16m,mode=1777 \
    --network "$network" --network-alias mfconnect \
    --publish "$MF_TUNNEL_PORT:$MF_TUNNEL_PORT/tcp" \
    --volume "$project-data:/var/lib/mfconnect" --volume "$project-tls:/etc/mfconnect/tls:ro" \
    --volume "$project-updates:/var/lib/mfconnect-updates" \
    --env "MF_ENTRY_ORIGIN=$MF_ENTRY_ORIGIN" --env "MF_TUNNEL_PORT=$MF_TUNNEL_PORT" \
    --env "MF_TRUSTED_PROXY_IPS=[\"$MF_GATEWAY_IP\"]" --env MF_WATCH_TLS=1 \
    --env MF_UPDATE_DIR=/var/lib/mfconnect-updates --env MF_PLUGIN_MANIFEST=/var/lib/mfconnect-updates/plugin-install.plg \
    --env MF_INSTALL_MODE=self_service --env "MF_INSTALL_DEVICE_LIMIT=$MF_INSTALL_DEVICE_LIMIT" \
    --env "MF_UPDATE_OWNER=$MF_UPDATE_OWNER" --env "MF_UPDATE_DEVICE=$MF_UPDATE_DEVICE" \
    --log-driver json-file --log-opt max-size=10m --log-opt max-file=3 "$MF_SERVER_IMAGE" bootstrap >/dev/null
wait_healthy "$server"
printf '%s\n' 'Both containers are healthy. Status: sh docker-run.sh status'
