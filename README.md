# Shrinkwrap

Zero-trust encrypted file storage, backed by Telegram.

## What it does

Shrinkwrap encrypts your files locally and stores them as documents in a private Telegram chat. Your encryption keys never leave your machine unprotected — they are wrapped with a master key derived from your password before being written to disk. Telegram is used purely as dumb cloud storage; it sees only opaque encrypted blobs.

Files larger than 18 MB are split into chunks (staying under Telegram's 20 MB document limit) and uploaded concurrently. Downloads reassemble and decrypt the chunks on the fly, writing the plaintext to a temporary file that is securely wiped when the app closes.

## Download

Pre-built binaries are published on the [Releases](../../releases) page for every tagged version.

| Platform | File |
|---|---|
| Linux x64 | `shrinkwrap-linux-amd64` |
| Windows x64 | `shrinkwrap-windows-amd64.exe` (or the NSIS installer) |
| macOS (Intel + Apple Silicon) | `Shrinkwrap-macos-universal.zip` |

On first launch you will be prompted to create a master password, then enter your Telegram bot token and chat ID in Settings.

> You need a Telegram bot token (create one via [@BotFather](https://t.me/BotFather)) and a private chat or channel ID where the bot has permission to send documents.

> **Linux transparency** — the window requests a translucent compositor surface. A compositing WM (KWin, Mutter, Picom, etc.) with RGBA support is needed for the glass effect. Without a compositor it falls back to an opaque dark background.

## Building from source

Requires Go 1.22+ and the [Wails CLI](https://wails.io).

```sh
make setup   # installs Wails CLI and runs go mod tidy
make dev     # development server with hot reload
```

```sh
make build-linux        # dist/linux/shrinkwrap
make build-linux-arm64  # dist/linux-arm64/shrinkwrap
make build-windows      # dist/windows/shrinkwrap.exe  (NSIS installer)
make build-macos        # dist/macos/Shrinkwrap.app    (universal binary)
```

## How encryption works

| Layer | Mechanism |
|---|---|
| Key derivation | PBKDF2-SHA256, random 32-byte salt, 200 000 iterations |
| File encryption | AES-256-GCM streaming (one nonce per chunk) |
| Key storage | Per-file raw key wrapped with the master key via AES-256-GCM, stored in SQLite |
| Credentials | Telegram bot token and chat ID wrapped with the same master key |
| Temp files | Written to `/dev/shm` (Linux) when available; zero-wiped on app close |

Legacy vaults created before the GCM rewrite used Fernet encryption and can be downloaded but cannot generate gift tokens — re-uploading the file upgrades them.

## Gift tokens

A gift token is a self-contained, shareable blob that includes the Telegram file IDs and the raw decryption key for one vault entry. Anyone with a configured Telegram bot can import the token and download the file. Only GCM-encrypted vaults support gift tokens.

## Project layout

```
.
├── main.go              # Wails entry point, window config
├── app.go               # All Go→JS bindings (upload, download, auth, settings)
├── backend/
│   ├── types.go         # Shared structs and event types
│   ├── database.go      # SQLite operations
│   ├── encryption.go    # AES-GCM key generation, encrypt/decrypt streams
│   └── telegram.go      # Telegram Bot API client (upload, download, delete)
└── frontend/
    ├── index.html        # App shell and all dialog markup
    └── src/
        ├── main.js       # Frontend logic and Wails event handling
        └── style.css     # Liquid glass theme
```

## Settings

| Setting | Description |
|---|---|
| Bot Token | Telegram bot token from @BotFather |
| Chat ID | ID of the private chat or channel the bot posts to |
| UI Scale | Webview zoom level (Auto / 100 % – 200 %) |

## License

MIT
