# Determine version from git root, if available and VERSION is not already defined
VERSION ?= $(shell \
  sh -c 'command -v git >/dev/null 2>&1 && \
  [ "$$(git rev-parse --is-inside-work-tree 2>/dev/null)" = "true" ] && \
  [ -z "$$(git rev-parse --show-prefix 2>/dev/null)" ] && \
  ver=$$(git describe --tags 2>/dev/null || git rev-parse --short HEAD 2>/dev/null) || ver=dev; \
  echo $${ver#v}' \
)


all: zdns

generate:
	go generate ./...

zdns: generate
	go build -o zdns -ldflags "-X github.com/zmap/zdns/v2/src/zdns.Version=$(VERSION)"

clean:
	rm -f zdns

install: zdns
	go install

test: zdns
	go test -v ./...
	pip3 install -r testing/requirements.txt
	pytest -n 4 testing/integration_tests.py

integration-tests: zdns
	pip3 install -r testing/requirements.txt
	pytest -n auto testing/integration_tests.py
	python3 testing/large_scan_integration/large_scan_integration_tests.py

# Not all hosts support this, so this will be a custom make target
ipv6-tests: zdns
	pip3 install -r testing/requirements.txt
	python3 testing/ipv6_tests.py

lint:
	goimports -w -local "github.com/zmap/zdns" ./
	gofmt -s -w ./
	golangci-lint run
	@if ! command -v black >/dev/null 2>&1; then pip3 install black; fi
	black --check ./

license-check:
	./.github/workflows/check_license.sh

benchmark: zdns
	cd ./benchmark && go run main.go stats.go

ci: zdns lint test integration-tests license-check

.PHONY: generate zdns clean test integration-tests lint ci license-check benchmark

