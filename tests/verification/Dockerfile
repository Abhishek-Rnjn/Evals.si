FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587

# The CA is a build secret supplied by the caller; preserve proxy defaults.
RUN --mount=type=secret,id=proxy_ca,required=true \
    apt-get -o Acquire::https::CaInfo=/run/secrets/proxy_ca update && \
    apt-get -o Acquire::https::CaInfo=/run/secrets/proxy_ca install -y --no-install-recommends \
      bubblewrap ca-certificates e2fsprogs gcc libc6-dev python3 && \
    rm -rf /var/lib/apt/lists/* && \
    install -d -m 1777 /workspace /work

ENV PATH=/usr/local/go/bin:$PATH GOCACHE=/tmp/go-cache GOMODCACHE=/go/pkg/mod GOPROXY=off
WORKDIR /src
