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

FROM ${PYTHON_IMAGE}
# bubblewrap for the namespaced rung; git for agent diffs.
RUN apt-get update \
 && apt-get install -y --no-install-recommends bubblewrap ca-certificates git \
 && rm -rf /var/lib/apt/lists/*
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
