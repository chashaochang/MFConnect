# Digest checked against docker-library/repo-info, repos/alpine/remote/3.22.md.
FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8

ARG MF_VERSION=0.8.1
LABEL org.opencontainers.image.title="MF Connect server" \
      org.opencontainers.image.source="https://github.com/chashaochang/MFConnect" \
      org.opencontainers.image.version="${MF_VERSION}-docker.1"

RUN apk add --no-cache ca-certificates curl jq openssl \
    && addgroup -S -g 10001 mfconnect \
    && adduser -S -D -H -u 10001 -G mfconnect mfconnect \
    && mkdir -p /var/lib/mfconnect /etc/mfconnect/tls \
    && chown 10001:10001 /var/lib/mfconnect \
    && chmod 0700 /var/lib/mfconnect

COPY --chmod=0755 dist/mfconnect-server /usr/local/bin/mfconnect-server
COPY --chmod=0755 dist/tools/frps /usr/local/bin/frps
COPY dist/tools/FRP-LICENSE dist/tools/FRP_VERSION /usr/local/share/mfconnect/
COPY --chmod=0755 deploy/docker/entrypoint.sh /usr/local/bin/mfconnect-entrypoint
COPY --chmod=0755 deploy/docker/healthcheck.sh /usr/local/bin/mfconnect-healthcheck
COPY --chmod=0755 deploy/docker/watch-tls.sh /usr/local/bin/mfconnect-watch-tls

RUN test "$(mfconnect-server version)" = "$MF_VERSION" \
    && test "$(frps --version)" = "0.71.1"

USER 10001:10001
WORKDIR /var/lib/mfconnect
EXPOSE 8443/tcp
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=10s --start-period=15s --retries=3 \
    CMD ["/usr/local/bin/mfconnect-healthcheck"]
ENTRYPOINT ["/usr/local/bin/mfconnect-entrypoint"]
CMD ["serve"]
