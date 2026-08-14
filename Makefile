VERSION ?= 0.1.0
DIST := $(CURDIR)/dist
CHART_BOOTSTRAP := $(CURDIR)/chart/bootstrap
CHART_PACKAGE := $(DIST)/embedded-cluster-disaster-recovery-$(VERSION).tgz
MAX_HELM_FILE_BYTES := 5242880
GOFLAGS ?=
GO_BUILD_FLAGS := -trimpath -buildvcs=false -ldflags="-s -w -buildid="

.PHONY: build test test-race vet verify chart package clean

build:
	CGO_ENABLED=0 go build $(GOFLAGS) $(GO_BUILD_FLAGS) -o embedded-cluster-dr ./cmd/embedded-cluster-dr

test:
	go test $(GOFLAGS) ./...

test-race:
	go test $(GOFLAGS) -race ./...

vet:
	go vet $(GOFLAGS) ./...

chart:
	helm lint chart
	helm template embedded-cluster-dr chart --namespace embedded-cluster-dr --include-crds >/dev/null
	@helm template embedded-cluster-dr chart --namespace embedded-cluster-dr | grep -q 'value: /var/run/embedded-cluster-dr/work'
	@helm template embedded-cluster-dr chart --namespace embedded-cluster-dr | grep -q 'path: /var/lib/ec/kubelet/pods'
	@helm template embedded-cluster-dr chart --namespace embedded-cluster-dr | grep -q 'path: /var/lib/ec/kubelet/plugins'
	@helm template embedded-cluster-dr chart --namespace embedded-cluster-dr | grep -q 'memory: 512Mi'

verify: test test-race vet chart

package: clean
	mkdir -p "$(DIST)"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GO_BUILD_FLAGS) -o "$(DIST)/embedded-cluster-dr-linux-amd64" ./cmd/embedded-cluster-dr
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GO_BUILD_FLAGS) -o "$(DIST)/embedded-cluster-dr-linux-arm64" ./cmd/embedded-cluster-dr
	@set -eu; \
		trap 'rm -rf "$(CHART_BOOTSTRAP)"' EXIT; \
		command -v xz >/dev/null; \
		mkdir -p "$(CHART_BOOTSTRAP)/linux-amd64" "$(CHART_BOOTSTRAP)/linux-arm64"; \
		xz -6 --threads=1 -c "$(DIST)/embedded-cluster-dr-linux-amd64" > "$(CHART_BOOTSTRAP)/linux-amd64/embedded-cluster-dr.xz"; \
		xz -6 --threads=1 -c "$(DIST)/embedded-cluster-dr-linux-arm64" > "$(CHART_BOOTSTRAP)/linux-arm64/embedded-cluster-dr.xz"; \
		test "$$(wc -c < "$(CHART_BOOTSTRAP)/linux-amd64/embedded-cluster-dr.xz" | tr -d ' ')" -le "$(MAX_HELM_FILE_BYTES)"; \
		test "$$(wc -c < "$(CHART_BOOTSTRAP)/linux-arm64/embedded-cluster-dr.xz" | tr -d ' ')" -le "$(MAX_HELM_FILE_BYTES)"; \
		helm package chart --destination "$(DIST)" --version "$(VERSION)" --app-version "$(VERSION)"
	@test "$$(tar -tzf "$(CHART_PACKAGE)" | grep -c '^embedded-cluster-disaster-recovery/bootstrap/linux-amd64/embedded-cluster-dr.xz$$')" -eq 1
	@test "$$(tar -tzf "$(CHART_PACKAGE)" | grep -c '^embedded-cluster-disaster-recovery/bootstrap/linux-arm64/embedded-cluster-dr.xz$$')" -eq 1
	cd "$(DIST)" && shasum -a 256 embedded-cluster-dr-linux-amd64 embedded-cluster-dr-linux-arm64 embedded-cluster-disaster-recovery-$(VERSION).tgz > checksums.txt

clean:
	rm -rf "$(DIST)" "$(CHART_BOOTSTRAP)" embedded-cluster-dr
