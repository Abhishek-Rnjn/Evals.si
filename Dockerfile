# The evalsi image: evalsid (server, worker, sandbox pool), evalsi-guest (the
# pod rung's in-sandbox agent, at /usr/local/bin/evalsi-guest),
# evalsi-operator, and the Python worker with the built-in packs and the
# harness. Build from the repository root:
#   docker build -t evalsi:dev .
ARG GO_VERSION=1.26
ARG PYTHON_IMAGE=python:3.13-slim-trixie

FROM golang:${GO_VERSION}-trixie AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY cmd cmd
COPY gen gen
COPY internal internal
COPY operator operator
ARG VERSION=0.0.0-dev
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    for b in evalsid evalsi-guest evalsi-operator; do \
      CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -X github.com/abhishek-rnjn/evals.si/internal/version.Version=${VERSION}" \
        -o /out/$b ./cmd/$b || exit 1; \
    done

FROM ${PYTHON_IMAGE} AS python
COPY --from=ghcr.io/astral-sh/uv:0.8.17 /uv /usr/local/bin/uv
WORKDIR /src/python
COPY python/pyproject.toml python/uv.lock ./
COPY python/evalsi evalsi
COPY python/evalsi-harness evalsi-harness
ENV UV_PROJECT_ENVIRONMENT=/opt/evalsi/venv UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --frozen --no-dev --all-packages --no-editable \
      --extra server --extra anthropic --extra jsonschema

# Firecracker for evalsi-sandboxd's microVM rung (mode: firecracker).
FROM ${PYTHON_IMAGE} AS firecracker
ARG FIRECRACKER_VERSION=v1.17.0
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
      amd64) arch=x86_64; sum=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558 ;; \
      arm64) arch=aarch64; sum=e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256 ;; \
      *) echo "no firecracker build for ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    v=${FIRECRACKER_VERSION}; \
    curl -fsSL -o /tmp/fc.tgz "https://github.com/firecracker-microvm/firecracker/releases/download/$v/firecracker-$v-$arch.tgz"; \
    echo "$sum  /tmp/fc.tgz" | sha256sum -c -; \
    tar -xzf /tmp/fc.tgz -C /tmp; \
    install -m 0755 "/tmp/release-$v-$arch/firecracker-$v-$arch" /out-firecracker

FROM ${PYTHON_IMAGE}
# bubblewrap for the namespaced rung; git for agent diffs; e2fsprogs
# (mkfs.ext4) and firecracker for the microVM rung.
RUN apt-get update \
 && apt-get install -y --no-install-recommends bubblewrap ca-certificates e2fsprogs git \
 && rm -rf /var/lib/apt/lists/*
COPY --from=firecracker /out-firecracker /usr/local/bin/firecracker
COPY --from=python /opt/evalsi/venv /opt/evalsi/venv
COPY --from=go /out/ /usr/local/bin/
# Non-root by default (the server, workers and the operator run as this
# user); sandbox pools run as root inside a user namespace.
RUN useradd --uid 65532 --home-dir /var/lib/evalsi --create-home evalsi
ENV PATH=/opt/evalsi/venv/bin:$PATH PYTHONUNBUFFERED=1
USER 65532:65532
WORKDIR /var/lib/evalsi
EXPOSE 8080
ENTRYPOINT ["evalsid"]
CMD ["serve"]
