VERSION ?= 0.1.0
DIST := $(CURDIR)/dist
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
	mkdir -p "$(DIST)/linux-amd64" "$(DIST)/linux-arm64"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$(DIST)/linux-amd64/embedded-cluster-dr" ./cmd/embedded-cluster-dr
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "$(DIST)/linux-arm64/embedded-cluster-dr" ./cmd/embedded-cluster-dr
	tar -C "$(DIST)/linux-amd64" -czf "$(DIST)/embedded-cluster-dr-linux-amd64.tar.gz" embedded-cluster-dr
	tar -C "$(DIST)/linux-arm64" -czf "$(DIST)/embedded-cluster-dr-linux-arm64.tar.gz" embedded-cluster-dr
	helm package chart --destination "$(DIST)" --version "$(VERSION)" --app-version "$(VERSION)"
	cd "$(DIST)" && shasum -a 256 embedded-cluster-dr-linux-amd64.tar.gz embedded-cluster-dr-linux-arm64.tar.gz embedded-cluster-disaster-recovery-$(VERSION).tgz > checksums.txt

clean:
	rm -rf "$(DIST)" embedded-cluster-dr

