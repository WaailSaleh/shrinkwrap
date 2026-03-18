// backend/types.go — shared data types used across backend packages and JS bindings.
package backend

// Vault is a single encrypted file entry stored in the local database.
type Vault struct {
	ID            int64  `json:"id"`
	Filename      string `json:"filename"`
	FileSize      int64  `json:"file_size"`
	ChunkCount    int    `json:"chunk_count"`
	UploadedAt    string `json:"uploaded_at"`
	SourcePath    string `json:"source_path"`
	EncryptionKey string `json:"-"` // never sent to JS
}

// VaultChunk is one Telegram message/document for a multi-part upload.
type VaultChunk struct {
	ID         int64  `json:"id"`
	VaultID    int64  `json:"vault_id"`
	ChunkIndex int    `json:"chunk_index"`
	FileID     string `json:"file_id"`
	MessageID  int64  `json:"message_id"`
}

// Settings holds the decrypted user configuration values.
type Settings struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
	UIScale  string `json:"ui_scale"`
}

// StatusEvent is emitted when the status bar text/color changes.
type StatusEvent struct {
	Text  string `json:"text"`
	Color string `json:"color"`
}

// ProgressEvent is emitted for progress text updates.
type ProgressEvent struct {
	Text string `json:"text"`
}

// SpeedEvent is emitted when transfer speeds change.
type SpeedEvent struct {
	UploadBps   float64 `json:"uploadBps"`
	DownloadBps float64 `json:"downloadBps"`
}

// BusyEvent signals whether a background operation is in progress.
type BusyEvent struct {
	Active bool `json:"active"`
}

// DownloadReadyEvent is emitted when a file has been decrypted to disk.
type DownloadReadyEvent struct {
	Path     string `json:"path"`
	Filename string `json:"filename"`
}

// ErrorEvent carries a user-facing error message.
type ErrorEvent struct {
	Message string `json:"message"`
}

// TransferEvent is emitted repeatedly during upload/download with live progress.
type TransferEvent struct {
	Operation   string  `json:"operation"`   // "upload" | "download"
	Filename    string  `json:"filename"`
	ChunkDone   int     `json:"chunkDone"`
	ChunkTotal  int     `json:"chunkTotal"`
	BytesDone   int64   `json:"bytesDone"`
	BytesTotal  int64   `json:"bytesTotal"`
	SpeedBps    float64 `json:"speedBps"`
	PercentDone float64 `json:"percentDone"` // 0–100
	EtaSecs     float64 `json:"etaSecs"`     // -1 when unknown
}
