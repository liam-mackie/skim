.PHONY: build run check desktop package-macos package-windows package-linux

VERSION := $(shell sed -n 's/.*"productVersion": *"\([^"]*\)".*/\1/p' cmd/skim-desktop/wails.json)
DESKTOP := cmd/skim-desktop
WAILS := cd $(DESKTOP) && wails build -skipbindings -clean
# Go 1.27 binaries need macOS 13; Wails defaults older. Keep in sync with build/darwin/Info.plist.
MACOS := -ldflags "-extldflags=-mmacosx-version-min=13.0"
LINUX_ARCH ?= amd64

build:
	npm --prefix web ci
	npm --prefix web run build
	go build -o bin/skim ./cmd/skim

run:
	go run ./cmd/skim

check:
	npm --prefix web run build
	go test ./...
	npm --prefix web run check

# Desktop app for this machine in cmd/skim-desktop/build/bin. macOS or Windows
# hosts; Linux builds use package-linux (WebKitGTK headers come from Docker).
desktop:
	$(WAILS) $(if $(filter Darwin,$(shell uname -s)),$(MACOS))

# Universal (arm64 + x86_64) Skim.app and bin/Skim-$(VERSION).dmg. Run on macOS.
# Signs with MACOS_SIGNING_IDENTITY and notarizes when NOTARY_* is set; see
# packaging/macos/package.sh.
package-macos:
	$(WAILS) $(MACOS) -platform darwin/universal
	packaging/macos/package.sh $(DESKTOP)/build/bin/Skim.app bin/Skim-$(VERSION).dmg

# Skim.exe and NSIS installer (installs WebView2 if missing). Cross-builds from
# any OS; needs makensis (brew install makensis / apt install nsis).
package-windows:
	$(WAILS) -platform windows/amd64 -nsis
	mkdir -p bin
	cp $(DESKTOP)/build/bin/Skim-amd64-installer.exe bin/Skim-$(VERSION)-windows-amd64-setup.exe
	cp $(DESKTOP)/build/bin/Skim.exe bin/Skim-$(VERSION)-windows-amd64.exe

# Linux binary and .deb built in Docker (needs WebKitGTK headers). LINUX_ARCH=amd64|arm64.
package-linux:
	docker build --platform linux/$(LINUX_ARCH) --build-arg VERSION=$(VERSION) -f packaging/linux/Dockerfile --output type=local,dest=bin/linux-$(LINUX_ARCH) .
