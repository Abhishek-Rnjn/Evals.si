# Common development tasks. Needs Go, buf and uv on PATH; `make tools` installs
# the protobuf plugins at the versions CI uses.

.PHONY: help tools proto check go-check py-sync py-check adapters-check e2e

help:
	@echo "make tools     install protoc-gen-go and protoc-gen-connect-go"
	@echo "make proto     lint, format and regenerate code from proto/"
	@echo "make check     run every check CI runs"
	@echo "make adapters-check  test each framework adapter in its own environment"
	@echo "make e2e       run evalsid against a real Python worker"

tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
	go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.21.0

proto: py-sync
	buf lint
	buf format -w
	buf generate
	scripts/gen-python-proto.sh

go-check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test ./...

py-sync:
	cd python && uv sync --all-packages

py-check: py-sync
	cd python && uv run ruff check . && uv run ruff format --check . && uv run mypy && uv run pytest -q

ADAPTERS := deepeval ragas inspect lm-eval

adapters-check:
	for a in $(ADAPTERS); do \
		(cd python/adapters/$$a && uv sync --locked && uv run --no-sync mypy && uv run --no-sync pytest -q) || exit 1; \
	done

check: go-check py-check
	buf lint
	buf format --diff --exit-code

e2e: py-sync
	EVALSI_E2E_WORKER="$(CURDIR)/python/.venv/bin/python -m evalsi" go test -count=1 ./tests/e2e/
