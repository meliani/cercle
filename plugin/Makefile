# Cercle Navidrome plugin
#
#   make test          run unit tests (native Go >= 1.25)
#   make build         compile plugin.wasm (GOOS=wasip1 GOARCH=wasm, c-shared)
#   make package       build dist/cercle.ndp (zip of manifest.json + plugin.wasm)
#   make docker-build  test + package inside golang:1.25 (no local Go needed)
#   make clean

GO          ?= go
DIST        := dist
NDP         := $(DIST)/cercle.ndp
WASM        := $(DIST)/plugin.wasm
GO_IMAGE    ?= golang:1.25-alpine
SOURCES     := $(shell find . -name '*.go' -not -path './dist/*' -not -path './.cache/*') go.mod go.sum

.PHONY: all test vet build package docker-build clean

all: package

test:
	$(GO) test ./internal/...

vet:
	$(GO) vet ./internal/...
	GOOS=wasip1 GOARCH=wasm $(GO) vet .

build: $(WASM)

$(WASM): $(SOURCES)
	@mkdir -p $(DIST)
	GOOS=wasip1 GOARCH=wasm $(GO) build -trimpath -ldflags='-s -w' -buildmode=c-shared -o $@ .

package: $(NDP)

# The archive must contain exactly manifest.json and plugin.wasm at its root.
$(NDP): $(WASM) manifest.json
	@rm -f $@
	cd $(DIST) && cp ../manifest.json manifest.json && zip -j -X cercle.ndp manifest.json plugin.wasm && rm -f manifest.json
	@ls -l $@

HOST_UID    := $(shell id -u)
HOST_GID    := $(shell id -g)

docker-build:
	docker run --rm -v "$(CURDIR)":/src -w /src $(GO_IMAGE) \
		sh -c 'apk add --no-cache make zip >/dev/null && make test vet package; s=$$?; chown -R $(HOST_UID):$(HOST_GID) $(DIST) 2>/dev/null; exit $$s'

clean:
	rm -rf $(DIST)
