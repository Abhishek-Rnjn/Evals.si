# Lightweight verification image: build the three static binaries into bin/
# first. This image does not verify the stock Firecracker image or GPU serving.
FROM python:3.13-slim
COPY --chmod=755 bin/evalsid bin/evalsi-guest bin/evalsi-operator /usr/local/bin/
COPY python/evalsi /src/evalsi
COPY python/evalsi-harness /src/evalsi-harness
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export PIP_CERT=/run/secrets/proxy_ca; fi; \
    pip install --no-cache-dir '/src/evalsi[server,jsonschema,anthropic]' /src/evalsi-harness
RUN mkdir -p /var/lib/evalsi && chown 65532:65532 /var/lib/evalsi
ENV HOME=/var/lib/evalsi
USER 65532:65532
WORKDIR /var/lib/evalsi
ENTRYPOINT ["evalsid"]
CMD ["serve"]
