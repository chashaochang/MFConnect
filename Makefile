GO ?= go
FRPC ?= dist/tools/frpc
FRPS ?= dist/tools/frps
FRP_LICENSE ?= dist/tools/FRP-LICENSE
EASYTIER_DIR ?= dist/tools
RELEASE_BASE ?=

.PHONY: build frp easytier test e2e e2e-easytier plugin plugin-local docker-bundle publication-check
build:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -buildvcs=false -trimpath -o dist/mfconnect-server ./cmd/mfconnect-server
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -buildvcs=false -trimpath -o dist/mfconnectd ./cmd/mfconnectd
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -buildvcs=false -trimpath -o dist/mfconnect-updater ./cmd/mfconnect-updater

frp:
	GO="$(GO)" bash scripts/build_frp.sh

easytier:
	python3 scripts/fetch_easytier.py --output "$(EASYTIER_DIR)"

test: build
	$(GO) test -buildvcs=false -race ./...
	python3 scripts/test_plugin.py
	python3 scripts/test_publication.py
	python3 scripts/test_cloud_installer.py

publication-check:
	python3 scripts/check_publication.py

plugin: build
	python3 scripts/package_plugin.py --binary dist/mfconnectd --frpc "$(FRPC)" --frp-license "$(FRP_LICENSE)" --release-base "$(RELEASE_BASE)"

plugin-local: build
	python3 scripts/package_plugin.py --binary dist/mfconnectd --frpc "$(FRPC)" --frp-license "$(FRP_LICENSE)" --local

docker-bundle: build
	python3 scripts/package_docker.py

e2e: build
	python3 scripts/e2e.py --frpc "$(FRPC)" --frps "$(FRPS)"

e2e-easytier: build
	python3 scripts/e2e.py --transport easytier --easytier "$(EASYTIER_DIR)/easytier-core"
	python3 scripts/easytier_probe.py --core "$(EASYTIER_DIR)/easytier-core" --cli "$(EASYTIER_DIR)/easytier-cli"
