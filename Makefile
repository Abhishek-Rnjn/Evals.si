# Common development tasks. Needs Go, buf and uv on PATH; `make tools` installs
# the protobuf plugins at the versions CI uses.

.PHONY: help tools proto check go-check py-sync py-check

help:
	@echo "make tools     install protoc-gen-go and protoc-gen-connect-go"
	@echo "make proto     lint, format and regenerate code from proto/"
	@echo "make check     run every check CI runs"

tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
	go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.21.0

proto:
	buf lint
	buf format -w
	buf generate

go-check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test ./...

py-sync:
	cd python && uv sync --all-packages

py-check: py-sync
	cd python && uv run ruff check . && uv run ruff format --check . && uv run mypy && uv run pytest -q

check: go-check py-check
	buf lint
	buf format --diff --exit-code
