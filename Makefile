.PHONY: all setup dev build-linux build-windows build-macos clean

# ── Setup (install Wails CLI if missing) ─────────────────────────────────────
setup:
	@which wails > /dev/null 2>&1 || go install github.com/wailsapp/wails/v2/cmd/wails@latest
	@go mod tidy
	@wails doctor

# ── Development server ────────────────────────────────────────────────────────
dev: setup
	wails dev -tags webkit2_41

# ── Run the built binary (clears VS Code snap GTK interference) ───────────────
run:
	env -u GTK_PATH -u GTK_EXE_PREFIX -u GIO_MODULE_DIR -u LOCPATH -u GSETTINGS_SCHEMA_DIR \
	    ./build/bin/shrinkwrap

# ── Production builds ─────────────────────────────────────────────────────────
build-linux: setup
	mkdir -p dist/linux
	wails build -platform linux/amd64 -tags webkit2_41 -o dist/linux/shrinkwrap
	@echo "→ dist/linux/shrinkwrap"

build-linux-arm64: setup
	mkdir -p dist/linux-arm64
	wails build -platform linux/arm64 -tags webkit2_41 -o dist/linux-arm64/shrinkwrap
	@echo "→ dist/linux-arm64/shrinkwrap"

build-windows: setup
	mkdir -p dist/windows
	wails build -platform windows/amd64 -nsis -o dist/windows/shrinkwrap.exe
	@echo "→ dist/windows/shrinkwrap.exe"

build-macos: setup
	mkdir -p dist/macos
	wails build -platform darwin/universal -o dist/macos/Shrinkwrap.app
	@echo "→ dist/macos/Shrinkwrap.app"

build-macos-amd64: setup
	mkdir -p dist/macos-amd64
	wails build -platform darwin/amd64 -o dist/macos-amd64/Shrinkwrap.app
	@echo "→ dist/macos-amd64/Shrinkwrap.app"

# ── Build for current platform ────────────────────────────────────────────────
all: setup
	mkdir -p dist
	wails build -tags webkit2_41 -o dist/shrinkwrap

# ── Clean ─────────────────────────────────────────────────────────────────────
clean:
	rm -rf build dist
