# Common development tasks. Needs Go, buf and uv on PATH; `make tools` installs
# the protobuf plugins at the versions CI uses.

.PHONY: help tools proto operator-gen build check go-check py-sync py-check adapters-check e2e

help:
	@echo "make tools     install protoc-gen-go and protoc-gen-connect-go"
	@echo "make proto     lint, format and regenerate code from proto/"
	@echo "make operator-gen  regenerate the CRDs, RBAC, webhook manifests and deepcopy code"
	@echo "make build     static bin/evalsid and bin/evalsi-guest (the microVM init)"
	@echo "make check     run every check CI runs"
	@echo "make adapters-check  test each framework adapter in its own environment"
	@echo "make e2e       run evalsid against a real Python worker (EVALSI_E2E_IMAGES=1 adds image pulls)"

tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
	go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.21.0

proto: py-sync
	buf lint
	buf format -w
	buf generate
	scripts/gen-python-proto.sh

CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.22.0

operator-gen:
	$(CONTROLLER_GEN) object paths=./operator/api/...
	$(CONTROLLER_GEN) crd rbac:roleName=evalsi-operator webhook paths=./operator/... \
		output:crd:artifacts:config=operator/config/crd \
		output:rbac:artifacts:config=operator/config/rbac \
		output:webhook:artifacts:config=operator/config/webhook

# Static binaries: evalsid doubles as the in-sandbox egress forwarder (in any
# image root), and evalsi-guest is init inside Firecracker microVMs.
build:
	CGO_ENABLED=0 go build -o bin/evalsid ./cmd/evalsid
	CGO_ENABLED=0 go build -o bin/evalsi-guest ./cmd/evalsi-guest
	CGO_ENABLED=0 go build -o bin/evalsi-operator ./cmd/evalsi-operator

go-check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test ./...

py-sync:
	cd python && uv sync --all-packages

py-check: py-sync
	cd python && uv run ruff check . && uv run ruff format --check . && uv run mypy && uv run pytest -q

ADAPTERS ?= deepeval ragas inspect lm-eval swebench bfcl taubench

adapters-check:
	for a in $(ADAPTERS); do \
		(cd python/adapters/$$a && uv sync --locked && uv run --no-sync mypy && uv run --no-sync pytest -q) || exit 1; \
	done

check: go-check py-check
	buf lint
	buf format --diff --exit-code

e2e: py-sync
	cd python/adapters/swebench && uv sync --locked
	EVALSI_E2E_WORKER="$(CURDIR)/python/.venv/bin/python -m evalsi" \
	EVALSI_E2E_SWEBENCH_WORKER="$(CURDIR)/python/adapters/swebench/.venv/bin/python -m evalsi" \
	go test -count=1 ./tests/e2e/
