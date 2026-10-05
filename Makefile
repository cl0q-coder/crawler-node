# cl0q crawler node — release Makefile.
#
#   make build    compile all binaries to ./dist/
#   make test     run the full test suite
#   make sbom     generate the CycloneDX SBOM (needs cyclonedx-gomod)
#   make docker   build the node image tagged with VERSION
#   make release  test + build + sbom + docker (the full release gate)
#
# Tagging a release:
#   1. Update VERSION and CHANGELOG.md.
#   2. make release
#   3. git tag -a v$(cat VERSION) -m "cl0q node $(cat VERSION)"
#   4. git push origin v$(cat VERSION)
#   5. Push the image to your registry:
#        docker tag cl0q-node:$(cat VERSION) registry.example.com/cl0q-node:$(cat VERSION)
#        docker push registry.example.com/cl0q-node:$(cat VERSION)

VERSION := $(shell cat VERSION)
BINARIES := website-crawler domain-crawler dns-enricher asn-enricher cl0q-node preflight
LDFLAGS := -s -w -X cl0q/internal/version.Version=$(VERSION)

.PHONY: build test vet sbom docker release clean

build:
	mkdir -p dist
	set -e; for c in $(BINARIES); do \
		CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/$$c ./cmd/$$c; \
	done
	@echo "built $(VERSION): $$(ls dist/)"

test:
	go test -count=1 $$(go list ./... | grep -v /vendor/)

vet:
	go vet ./cmd/... ./internal/...

sbom:
	@which cyclonedx-gomod >/dev/null || (echo "install: go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest"; exit 1)
	cyclonedx-gomod mod -json -output dist/sbom-$(VERSION).json .
	@echo "SBOM: dist/sbom-$(VERSION).json"

docker:
	docker build --build-arg VERSION=$(VERSION) -t cl0q-node:$(VERSION) -t cl0q-node:latest .
	@echo "image: cl0q-node:$(VERSION)"

release: vet test build sbom docker
	@echo "release $(VERSION) ready: binaries in dist/, SBOM in dist/, image cl0q-node:$(VERSION)"

clean:
	rm -rf dist
