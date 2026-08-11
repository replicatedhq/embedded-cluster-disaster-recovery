VERSION ?= 0.1.0
DIST := $(CURDIR)/dist
CHART_BOOTSTRAP := $(CURDIR)/chart/bootstrap
CHART_PACKAGE := $(DIST)/embedded-cluster-disaster-recovery-$(VERSION).tgz
GOFLAGS ?=

.PHONY: build test test-race vet verify chart package clean

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -trimpath -ldflags="-s -w" -o embedded-cluster-dr ./cmd/embedded-cluster-dr

test:
	go test $(GOFLAGS) ./...

test-race:
	go test $(GOFLAGS) -race ./...

vet:
	go vet $(GOFLAGS) ./...

chart:
	helm lint chart
	helm template embedded-cluster-dr chart --namespace embedded-cluster-dr --include-crds >/dev/null

verify: test test-race vet chart

package: clean
	mkdir -p "$(DIST)"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$(DIST)/embedded-cluster-dr-linux-amd64" ./cmd/embedded-cluster-dr
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "$(DIST)/embedded-cluster-dr-linux-arm64" ./cmd/embedded-cluster-dr
	@set -eu; \
		trap 'rm -rf "$(CHART_BOOTSTRAP)"' EXIT; \
		mkdir -p "$(CHART_BOOTSTRAP)/linux-amd64" "$(CHART_BOOTSTRAP)/linux-arm64"; \
		cp "$(DIST)/embedded-cluster-dr-linux-amd64" "$(CHART_BOOTSTRAP)/linux-amd64/embedded-cluster-dr"; \
		cp "$(DIST)/embedded-cluster-dr-linux-arm64" "$(CHART_BOOTSTRAP)/linux-arm64/embedded-cluster-dr"; \
		helm package chart --destination "$(DIST)" --version "$(VERSION)" --app-version "$(VERSION)"
	@test "$$(tar -tzf "$(CHART_PACKAGE)" | grep -c '^embedded-cluster-disaster-recovery/bootstrap/linux-amd64/embedded-cluster-dr$$')" -eq 1
	@test "$$(tar -tzf "$(CHART_PACKAGE)" | grep -c '^embedded-cluster-disaster-recovery/bootstrap/linux-arm64/embedded-cluster-dr$$')" -eq 1
	cd "$(DIST)" && shasum -a 256 embedded-cluster-dr-linux-amd64 embedded-cluster-dr-linux-arm64 embedded-cluster-disaster-recovery-$(VERSION).tgz > checksums.txt

clean:
	rm -rf "$(DIST)" "$(CHART_BOOTSTRAP)" embedded-cluster-dr
