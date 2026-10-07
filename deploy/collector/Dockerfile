# The evalsi-collector image. Build from the repository root:
#   docker build -f deploy/collector/Dockerfile -t evalsi-collector:dev .
ARG GO_VERSION=1.26
FROM golang:${GO_VERSION}-trixie AS build
WORKDIR /src
COPY deploy/collector/builder-config.yaml .
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go run go.opentelemetry.io/collector/cmd/builder@v0.162.0 --config builder-config.yaml

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /src/_build/evalsi-collector /evalsi-collector
COPY deploy/collector/config.yaml deploy/collector/config-dev.yaml /etc/evalsi-collector/
EXPOSE 4317 4318 13133
ENTRYPOINT ["/evalsi-collector"]
CMD ["--config", "/etc/evalsi-collector/config.yaml"]
