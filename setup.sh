#!/usr/bin/env bash
# setup.sh — Install build dependencies for ZenithVault (Wails/Go)
# Run once with: bash setup.sh
set -e

echo "==> Installing system dependencies..."

if command -v apt-get &>/dev/null; then
    sudo apt-get update -q
    # Wails v2.9+ supports webkit2gtk-4.1 (GTK4) via build tag
    sudo apt-get install -y \
        pkg-config \
        libgtk-3-dev \
        libwebkit2gtk-4.1-dev \
        build-essential \
        nsis
elif command -v dnf &>/dev/null; then
    sudo dnf install -y \
        pkg-config gtk3-devel webkit2gtk4.1-devel \
        gcc nsis
elif command -v pacman &>/dev/null; then
    sudo pacman -S --needed \
        pkgconf gtk3 webkit2gtk-4.1 \
        base-devel nsis
else
    echo "Unsupported package manager. Install manually: pkg-config gtk3-dev webkit2gtk-4.1-dev"
fi

echo "==> Installing Go version manager (g)..."
if ! command -v g &>/dev/null; then
    curl -sSL https://git.io/g-install | sh -s -- -y
    export PATH="$HOME/.go/bin:$PATH"
fi

echo "==> Installing Go (if needed)..."
export PATH="$HOME/.go/bin:$HOME/go/bin:$PATH"
if ! command -v go &>/dev/null; then
    g install latest
fi
go version

echo "==> Installing Wails CLI..."
go install github.com/wailsapp/wails/v2/cmd/wails@latest

echo "==> Checking Wails doctor..."
wails doctor

echo ""
echo "Setup complete! Run:"
echo "  make dev          # development server with hot-reload"
echo "  make build-linux  # production build for Linux"
echo "  make build-macos  # production build for macOS (on macOS)"
echo "  make build-windows# production build for Windows (on Windows)"
