FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
LABEL org.opencontainers.image.title="MF Connect gateway" \
      org.opencontainers.image.source="https://github.com/chashaochang/MFConnect" \
      org.opencontainers.image.version="0.8.0"
RUN apk add --no-cache nginx python3 curl openssl ca-certificates \
    && mkdir -p /state /updates /opt/mfconnect
COPY app/ /opt/mfconnect/
COPY plugin/ /opt/mfconnect/plugin/
COPY acme/ /opt/acme/
COPY --chmod=0755 acme-wrapper.sh /usr/local/bin/mfconnect-acme
EXPOSE 80 443
ENTRYPOINT ["python3", "/opt/mfconnect/gateway.py"]
CMD ["serve"]
