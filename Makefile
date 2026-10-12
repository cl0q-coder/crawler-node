# cl0q crawler node — release Makefile.
#
#   make build    compile all binaries to ./dist/ (clean tree required;
#               embeds commit SHA + build time provenance stamp)
#   make dev-build dirty-tree-friendly dev builds (stamp reads dev-<sha>)
#   make test     run the full test suite
#   make sbom     generate the CycloneDX SBOM (needs cyclonedx-gomod)
#   make docker   build the node image tagged with VERSION
#   make release  test + build + sbom + docker (the full release gate)
#   make tag      create + push the v$(VERSION) tag (one command release)
#
# Tagging a release:
#   1. Update VERSION and CHANGELOG.md.
#   2. make release
#   3. make tag
#   4. Push the image to your registry:
#        docker tag cl0q-node:$(cat VERSION) registry.example.com/cl0q-node:$(cat VERSION)
#        docker push registry.example.com/cl0q-node:$(cat VERSION)

VERSION := $(shell cat VERSION)
# Provenance stamp (Track O, O2/O3): every binary embeds the exact source
# commit it was built from, plus the UTC build time. A stamp is only
# meaningful on a clean tree — `build` refuses dirty trees below.
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILDTIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BINARIES := website-crawler domain-crawler dns-enricher asn-enricher cl0q-node preflight
LDFLAGS := -s -w -X cl0q/internal/version.Version=$(VERSION) -X cl0q/internal/version.Commit=$(COMMIT) -X cl0q/internal/version.BuildTime=$(BUILDTIME)

.PHONY: build dev-build test vet sbom docker release clean

# Canonical build: from a clean tree only, so the embedded commit SHA is a
# true provenance stamp. This is how prod binaries are built (Track O O2):
# pin a commit, build from it, verify with `<binary> --version`.
build:
	@test -z "$$(git status --porcelain)" || (echo "refusing: working tree not clean — commit or stash first (or use 'make dev-build')" && exit 1)
	mkdir -p dist
	set -e; for c in $(BINARIES); do \
		CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/$$c ./cmd/$$c; \
	done
	@echo "built $(VERSION) @ $(COMMIT): $$(ls dist/)"

# Dev builds: no clean-tree requirement; commit stamp reads dev-<sha>.
dev-build:
	mkdir -p dist
	set -e; for c in $(BINARIES); do \
		CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X cl0q/internal/version.Version=$(VERSION) -X cl0q/internal/version.Commit=dev-$(COMMIT) -X cl0q/internal/version.BuildTime=$(BUILDTIME)" -o dist/$$c ./cmd/$$c; \
	done
	@echo "dev-built $(VERSION): $$(ls dist/)"

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

# One-command release: gate on `make release` first, then tag + push.
tag:
	@test -z "$$(git status --porcelain)" || (echo "refusing: working tree not clean" && exit 1)
	@git rev-parse -q --verify "refs/tags/v$(VERSION)" >/dev/null && (echo "refusing: tag v$(VERSION) already exists" && exit 1)
	git tag -a "v$(VERSION)" -m "cl0q node $(VERSION)"
	git push origin "v$(VERSION)"
	@echo "tagged + pushed v$(VERSION)"

clean:
	rm -rf dist
