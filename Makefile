# codex-usage-guard — CLIProxyAPI usage plugin build
#
# The plugin is a cgo c-shared library. ONE source builds to every platform;
# only the output extension and the C cross-compiler differ.
#
#   make dll      # windows/amd64  -> dist/codex-usage-guard.dll   (needs mingw-w64)
#   make so       # linux/amd64    -> dist/codex-usage-guard.so    (needs linux cc / zig)
#   make dylib    # host (darwin)  -> dist/codex-usage-guard.dylib
#
# Cross-compilers (pick what you have):
#   - mingw-w64 for Windows:  brew install mingw-w64   (gives x86_64-w64-mingw32-gcc)
#   - zig as a universal cc:  brew install zig         (then CC="zig cc -target ...")
#
# On a Windows box with Go + a C toolchain you can simply run:
#   go build -buildmode=c-shared -o codex-usage-guard.dll .

ID      := codex-usage-guard
DIST    := dist
PKG     := .

WIN_CC  ?= x86_64-w64-mingw32-gcc
LINUX_CC ?= zig cc -target x86_64-linux-gnu

.PHONY: all dll so dylib clean tidy

all: dll so

$(DIST):
	mkdir -p $(DIST)

# Windows DLL (the production target for Windows nodes).
dll: | $(DIST)
	CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC="$(WIN_CC)" \
		go build -buildmode=c-shared -o $(DIST)/$(ID).dll $(PKG)
	rm -f $(DIST)/$(ID).h

# Linux shared object.
so: | $(DIST)
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC="$(LINUX_CC)" \
		go build -buildmode=c-shared -o $(DIST)/$(ID).so $(PKG)
	rm -f $(DIST)/$(ID).h

# Host build (darwin) — handy for a quick compile sanity check on a Mac.
dylib: | $(DIST)
	CGO_ENABLED=1 go build -buildmode=c-shared -o $(DIST)/$(ID).dylib $(PKG)
	rm -f $(DIST)/$(ID).h

tidy:
	go mod tidy

clean:
	rm -rf $(DIST)
