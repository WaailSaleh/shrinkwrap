// app.go — Wails application backend. All exported methods are bound to the JS frontend.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/zenithvault/app/backend"
)

const (
	chunkSize         = 18 * 1024 * 1024 // 18 MB — safe under Telegram's 20 MB limit
	uploadConcurrency = 3                // max concurrent chunk uploads per file

	// Event names emitted to the frontend
	evStatus        = "status"
	evProgress      = "progress"
	evSpeed         = "speed"
	evBusy          = "busy"
	evVaultsUpdated = "vaults_updated"
	evDownloadReady = "download_ready"
	evErrorMsg      = "error_msg"
	evTransfer      = "transfer_progress"

	// Color palette (matches Python app)
	colorAccent  = "#58a6ff"
	colorSuccess = "#3fb950"
	colorWarning = "#d29922"
	colorDanger  = "#f85149"
	colorMuted   = "#7d8590"
	colorText    = "#e6edf3"
)

// App is the main Wails application struct.
type App struct {
	ctx       context.Context
	db        *backend.DB
	masterKey []byte // nil when locked; base64url-encoded Fernet key
	telegram  *backend.TelegramClient
	tempFiles []string
	mu        sync.Mutex
}

// NewApp creates the application struct (called from main.go).
func NewApp() *App {
	return &App{}
}

// startup is called when the app starts. The context is saved so runtime
// functions (events, dialogs) can be used from any method.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	var err error
	a.db, err = backend.OpenDB()
	if err != nil {
		wruntime.LogErrorf(ctx, "Failed to open database: %v", err)
	}
}

// domReady is called once the frontend DOM is ready.
func (a *App) domReady(_ context.Context) {}

// beforeClose is called when the window is about to close.
func (a *App) beforeClose(_ context.Context) bool {
	a.cleanupTempFiles()
	return false // false = allow close
}

// shutdown is called after the window closes.
func (a *App) shutdown(_ context.Context) {
	if a.db != nil {
		a.db.Close()
	}
}

// ── Authentication ─────────────────────────────────────────────────────────────

// HasMasterPassword reports whether a master password has been set up.
func (a *App) HasMasterPassword() bool {
	if a.db == nil {
		return false
	}
	salt, _ := a.db.GetSetting("MASTER_SALT", "")
	return salt != ""
}

// HasExistingData reports whether there are vaults or credentials in the DB
// (used to pick the "migrate" mode instead of "create" on first unlock).
func (a *App) HasExistingData() bool {
	if a.db == nil {
		return false
	}
	vaults, _ := a.db.GetAllVaults()
	token, _ := a.db.GetSetting("BOT_TOKEN", "")
	chat, _ := a.db.GetSetting("CHAT_ID", "")
	return len(vaults) > 0 || token != "" || chat != ""
}

// CreateMasterPassword sets up a new master password (first run).
// Also migrates any plaintext credentials from legacy data if migrate=true.
func (a *App) CreateMasterPassword(password string, migrate bool) error {
	if a.db == nil {
		return errors.New("database not ready")
	}
	salt, err := backend.GenerateSalt()
	if err != nil {
		return err
	}
	masterKey, err := backend.DeriveMasterKey(password, salt)
	if err != nil {
		return err
	}

	verify, err := backend.WrapValue("zenithvault_v1", masterKey)
	if err != nil {
		return err
	}

	if err := a.db.SetSetting("MASTER_SALT", fmt.Sprintf("%x", salt)); err != nil {
		return err
	}
	if err := a.db.SetSetting("MASTER_VERIFY", verify); err != nil {
		return err
	}

	a.masterKey = masterKey

	if migrate {
		a.migratePlaintextData()
	}

	a.reloadTelegramClient()
	return nil
}

// UnlockWithPassword verifies the password and loads the master key into memory.
func (a *App) UnlockWithPassword(password string) error {
	if a.db == nil {
		return errors.New("database not ready")
	}
	saltHex, err := a.db.GetSetting("MASTER_SALT", "")
	if err != nil || saltHex == "" {
		return errors.New("no master password configured")
	}

	salt := make([]byte, len(saltHex)/2)
	if _, err := fmt.Sscanf(saltHex, "%x", &salt); err != nil {
		// fallback: hex.DecodeString equivalent
		salt, err = hexDecode(saltHex)
		if err != nil {
			return fmt.Errorf("invalid salt encoding: %w", err)
		}
	}

	masterKey, err := backend.DeriveMasterKey(password, salt)
	if err != nil {
		return err
	}

	// Verify against stored token.
	verify, _ := a.db.GetSetting("MASTER_VERIFY", "")
	if verify != "" {
		plain, err := backend.UnwrapValue(verify, masterKey)
		if err != nil || plain != "zenithvault_v1" {
			return errors.New("incorrect password")
		}
	}

	a.masterKey = masterKey
	a.reloadTelegramClient()
	return nil
}

// IsUnlocked reports whether the master key is in memory.
func (a *App) IsUnlocked() bool {
	return len(a.masterKey) > 0
}

// ── Settings ──────────────────────────────────────────────────────────────────

// IsConfigured reports whether Telegram credentials are set.
func (a *App) IsConfigured() bool {
	return a.telegram != nil
}

// GetSettings returns the decrypted settings values.
func (a *App) GetSettings() (*backend.Settings, error) {
	if a.db == nil {
		return nil, errors.New("database not ready")
	}
	s := &backend.Settings{}

	if len(a.masterKey) > 0 {
		encToken, _ := a.db.GetSetting("BOT_TOKEN", "")
		encChat, _ := a.db.GetSetting("CHAT_ID", "")
		if encToken != "" {
			if plain, err := backend.UnwrapValue(encToken, a.masterKey); err == nil {
				s.BotToken = plain
			}
		}
		if encChat != "" {
			if plain, err := backend.UnwrapValue(encChat, a.masterKey); err == nil {
				s.ChatID = plain
			}
		}
	}

	s.UIScale, _ = a.db.GetSetting("UI_SCALE", "Auto")
	return s, nil
}

// SaveSettings persists encrypted credentials and reloads the Telegram client.
func (a *App) SaveSettings(botToken, chatID, uiScale string) error {
	if a.db == nil {
		return errors.New("database not ready")
	}
	if len(a.masterKey) == 0 {
		return errors.New("not authenticated")
	}

	if botToken != "" {
		wrapped, err := backend.WrapValue(botToken, a.masterKey)
		if err != nil {
			return err
		}
		if err := a.db.SetSetting("BOT_TOKEN", wrapped); err != nil {
			return err
		}
		if err := a.db.SetSetting("KEYS_ENCRYPTED", "1"); err != nil {
			return err
		}
	}
	if chatID != "" {
		wrapped, err := backend.WrapValue(chatID, a.masterKey)
		if err != nil {
			return err
		}
		if err := a.db.SetSetting("CHAT_ID", wrapped); err != nil {
			return err
		}
	}
	if uiScale != "" {
		if err := a.db.SetSetting("UI_SCALE", uiScale); err != nil {
			return err
		}
	}

	a.reloadTelegramClient()
	return nil
}

// TestConnection validates the current Telegram credentials.
func (a *App) TestConnection() error {
	if a.telegram == nil {
		return errors.New("Telegram not configured")
	}
	_, err := a.telegram.TestConnection()
	return err
}

// ── Vault operations ──────────────────────────────────────────────────────────

// GetVaults returns all vault entries from the database.
func (a *App) GetVaults() ([]backend.Vault, error) {
	if a.db == nil {
		return nil, errors.New("database not ready")
	}
	return a.db.GetAllVaults()
}

// StartUpload begins an async upload of the given paths.
// asZip: bundle all paths into a single ZIP before uploading.
// deleteOriginals: unlink source files after successful upload.
func (a *App) StartUpload(paths []string, asZip bool, deleteOriginals bool) error {
	if !a.IsUnlocked() {
		return errors.New("not authenticated")
	}
	if a.telegram == nil {
		return errors.New("Telegram not configured — please save settings first")
	}
	go a.doUpload(paths, asZip, deleteOriginals)
	return nil
}

// StartDownload begins an async download and decryption of a vault entry.
func (a *App) StartDownload(vaultID int64) error {
	if !a.IsUnlocked() {
		return errors.New("not authenticated")
	}
	if a.telegram == nil {
		return errors.New("Telegram not configured")
	}
	go a.doDownload(vaultID)
	return nil
}

// StartDelete begins an async deletion of a vault entry.
func (a *App) StartDelete(vaultID int64) error {
	if !a.IsUnlocked() {
		return errors.New("not authenticated")
	}
	go a.doDelete(vaultID)
	return nil
}

// OpenFile opens a file with the system default application.
func (a *App) OpenFile(path string) error {
	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}

// ── File dialogs ──────────────────────────────────────────────────────────────

// OpenFilesDialog opens a native multi-file picker and returns selected paths.
func (a *App) OpenFilesDialog() ([]string, error) {
	return wruntime.OpenMultipleFilesDialog(a.ctx, wruntime.OpenDialogOptions{
		Title: "Select files to vault",
	})
}

// OpenFolderDialog opens a native folder picker and returns the selected path.
func (a *App) OpenFolderDialog() (string, error) {
	return wruntime.OpenDirectoryDialog(a.ctx, wruntime.OpenDialogOptions{
		Title: "Select folder to vault",
	})
}

// ── Gift tokens ───────────────────────────────────────────────────────────────

// GenerateGiftToken creates a shareable gift token for a vault entry.
func (a *App) GenerateGiftToken(vaultID int64) (string, error) {
	if !a.IsUnlocked() {
		return "", errors.New("not authenticated")
	}
	vault, err := a.db.GetVaultByID(vaultID)
	if err != nil || vault == nil {
		return "", errors.New("vault not found")
	}
	chunks, err := a.db.GetVaultChunks(vaultID)
	if err != nil {
		return "", err
	}

	keyStr, err := backend.UnwrapValue(vault.EncryptionKey, a.masterKey)
	if err != nil {
		return "", err
	}
	if !backend.IsGCMKey(keyStr) {
		return "", errors.New("legacy Fernet vaults cannot generate gift tokens; re-upload the file first")
	}
	rawKey, err := backend.KeyFromStorage(keyStr)
	if err != nil {
		return "", err
	}

	fileIDs := make([]string, len(chunks))
	for i, c := range chunks {
		fileIDs[i] = c.FileID
	}
	return backend.GenerateGiftToken(fileIDs, rawKey)
}

// ImportGiftToken downloads and stores a vault from a gift token.
func (a *App) ImportGiftToken(token, filename string) error {
	if !a.IsUnlocked() {
		return errors.New("not authenticated")
	}
	if a.telegram == nil {
		return errors.New("Telegram not configured")
	}

	fileIDs, rawKey, err := backend.ParseGiftToken(token)
	if err != nil {
		return err
	}

	go a.doImportGift(fileIDs, rawKey, filename)
	return nil
}

// ── Internal: upload ──────────────────────────────────────────────────────────

func (a *App) doUpload(paths []string, asZip bool, deleteOriginals bool) {
	a.emitBusy(true)
	defer a.emitBusy(false)

	if asZip {
		a.emitStatus(fmt.Sprintf("Preparing ZIP of %d item(s)…", len(paths)), colorAccent)
		data, err := makeZip(paths)
		if err != nil {
			a.emitError(fmt.Sprintf("ZIP failed: %v", err))
			return
		}
		if err := a.runUpload("bundle.zip", bytes.NewReader(data), int64(len(data)),
			joinPaths(paths), deleteOriginals, paths); err != nil {
			a.emitError(fmt.Sprintf("Upload failed: %v", err))
			return
		}
	} else {
		all := expandPaths(paths)
		n := len(all)
		a.emitStatus(fmt.Sprintf("Uploading %d file(s)…", n), colorAccent)
		for i, p := range all {
			a.emitStatus(fmt.Sprintf("[%d/%d] Uploading %s…", i+1, n, filepath.Base(p)), colorAccent)
			f, err := os.Open(p)
			if err != nil {
				a.emitError(fmt.Sprintf("Failed to open %s: %v", p, err))
				continue
			}
			info, _ := f.Stat()
			var fileSize int64
			if info != nil {
				fileSize = info.Size()
			}
			if err := a.runUpload(filepath.Base(p), f, fileSize, p, deleteOriginals, []string{p}); err != nil {
				f.Close()
				a.emitError(fmt.Sprintf("Failed to upload %s: %v", filepath.Base(p), err))
				continue
			}
			f.Close()
		}
	}
	a.refreshVaults()
}

type chunkResult struct {
	index     int
	fileID    string
	messageID int64
	err       error
}

func (a *App) runUpload(displayName string, src io.Reader, fileSize int64, sourcePath string,
	deleteOriginals bool, originalPaths []string) error {

	rawKey, err := backend.GenerateKey()
	if err != nil {
		return err
	}

	// Read source in chunkSize blocks.
	var blocks [][]byte
	buf := make([]byte, chunkSize)
	for {
		n, readErr := io.ReadFull(src, buf)
		if n > 0 {
			block := make([]byte, n)
			copy(block, buf[:n])
			blocks = append(blocks, block)
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	totalChunks := len(blocks)
	if totalChunks > 1 {
		a.emitStatus(fmt.Sprintf("Uploading '%s' (%d parts, %d concurrent)…",
			displayName, totalChunks, uploadConcurrency), colorAccent)
	}

	t0 := time.Now()
	var sentBytes int64  // bytes of original plaintext uploaded so far
	var doneChunks int32 // atomic completed-chunk counter

	sem := make(chan struct{}, uploadConcurrency)
	results := make([]chunkResult, totalChunks)
	var wg sync.WaitGroup

	for i, block := range blocks {
		wg.Add(1)
		go func(idx int, blk []byte) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Encrypt block (CPU-bound, safe in goroutine).
			var encBuf bytes.Buffer
			if _, err := backend.EncryptStream(bytes.NewReader(blk), &encBuf, rawKey); err != nil {
				results[idx] = chunkResult{index: idx, err: err}
				return
			}

			chunkName := fmt.Sprintf("%s.part%03d.vault", displayName, idx+1)
			res, err := a.telegram.UploadFile(chunkName, encBuf.Bytes())
			if err != nil {
				results[idx] = chunkResult{index: idx, err: fmt.Errorf("upload chunk %d: %w", idx+1, err)}
				return
			}

			// Update byte counter and emit upload-progress event with speed metrics.
			newSent := atomic.AddInt64((*int64)(&sentBytes), int64(len(blk)))
			uploadDone := int(atomic.AddInt32(&doneChunks, 1))
			elapsed := time.Since(t0).Seconds()
			speedBps := 0.0
			if elapsed > 0 {
				speedBps = float64(newSent) / elapsed
			}
			pct := 0.0
			if fileSize > 0 {
				pct = float64(newSent) / float64(fileSize) * 100
			} else if totalChunks > 0 {
				pct = float64(uploadDone) / float64(totalChunks) * 100
			}
			eta := -1.0
			if speedBps > 0 && fileSize > 0 {
				remaining := fileSize - newSent
				if remaining > 0 {
					eta = float64(remaining) / speedBps
				}
			}
			a.emitTransfer(backend.TransferEvent{
				Operation:   "upload",
				Filename:    displayName,
				ChunkDone:   uploadDone,
				ChunkTotal:  totalChunks,
				BytesDone:   newSent,
				BytesTotal:  fileSize,
				SpeedBps:    speedBps,
				PercentDone: pct,
				EtaSecs:     eta,
			})
			a.emitSpeed(speedBps, 0)

			// Verify the chunk is accessible on Telegram's CDN.
			// A lightweight getFile call confirms persistence before we commit to DB.
			if verifyErr := a.telegram.VerifyChunk(res.FileID); verifyErr != nil {
				a.telegram.DeleteMessage(res.MessageID)
				results[idx] = chunkResult{index: idx, err: fmt.Errorf("chunk %d verification failed: %w", idx+1, verifyErr)}
				return
			}
			a.emitTransfer(backend.TransferEvent{
				Operation:   "verify",
				Filename:    displayName,
				ChunkDone:   uploadDone,
				ChunkTotal:  totalChunks,
				BytesDone:   newSent,
				BytesTotal:  fileSize,
				SpeedBps:    0,
				PercentDone: pct,
				EtaSecs:     -1,
			})

			results[idx] = chunkResult{
				index:     idx,
				fileID:    res.FileID,
				messageID: res.MessageID,
			}
		}(i, block)
	}
	wg.Wait()

	// Check for errors — roll back any successful uploads on partial failure.
	var firstErr error
	var successes []chunkResult
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
		} else {
			successes = append(successes, r)
		}
	}
	if firstErr != nil {
		for _, s := range successes {
			a.telegram.DeleteMessage(s.messageID)
		}
		return firstErr
	}

	// Persist to database.
	encKeyStr, err := backend.WrapValue(backend.KeyToStorage(rawKey), a.masterKey)
	if err != nil {
		return err
	}
	vaultID, err := a.db.InsertVault(displayName, encKeyStr, fileSize, len(blocks), sourcePath)
	if err != nil {
		return err
	}
	for _, r := range results {
		if err := a.db.InsertVaultChunk(vaultID, r.index, r.fileID, r.messageID); err != nil {
			return err
		}
	}

	if deleteOriginals {
		for _, p := range originalPaths {
			os.Remove(p)
		}
	}

	a.emitStatus(fmt.Sprintf("'%s' uploaded and encrypted.", displayName), colorSuccess)
	a.emitProgress("")
	a.emitSpeed(0, 0)
	return nil
}

// ── Internal: download ────────────────────────────────────────────────────────

func (a *App) doDownload(vaultID int64) {
	a.emitBusy(true)
	defer a.emitBusy(false)

	vault, err := a.db.GetVaultByID(vaultID)
	if err != nil || vault == nil {
		a.emitError("Vault not found")
		return
	}
	chunks, err := a.db.GetVaultChunks(vaultID)
	if err != nil || len(chunks) == 0 {
		a.emitError("No chunks found for vault")
		return
	}

	a.emitStatus(fmt.Sprintf("Loading '%s'…", vault.Filename), colorAccent)

	keyStr, err := backend.UnwrapValue(vault.EncryptionKey, a.masterKey)
	if err != nil {
		a.emitError(fmt.Sprintf("Key decryption failed: %v", err))
		return
	}

	tmpFile, err := makeTempFile(filepath.Ext(vault.Filename))
	if err != nil {
		a.emitError(fmt.Sprintf("Cannot create temp file: %v", err))
		return
	}
	tmpPath := tmpFile.Name()

	t0 := time.Now()
	var dlBytes int64
	n := len(chunks)

	// totalBytes is unknown for downloads (Telegram doesn't report file sizes upfront),
	// so we use chunk count as the denominator for progress.
	emitDlTransfer := func(chunkIdx int, dlBytesNow int64, speedBps float64) {
		pct := float64(chunkIdx) / float64(n) * 100
		eta := -1.0
		if speedBps > 0 && n > chunkIdx {
			// Estimate remaining time based on average bytes/chunk so far.
			if chunkIdx > 0 {
				bytesPerChunk := float64(dlBytesNow) / float64(chunkIdx)
				eta = bytesPerChunk * float64(n-chunkIdx) / speedBps
			}
		}
		a.emitTransfer(backend.TransferEvent{
			Operation:   "download",
			Filename:    vault.Filename,
			ChunkDone:   chunkIdx,
			ChunkTotal:  n,
			BytesDone:   dlBytesNow,
			BytesTotal:  vault.FileSize,
			SpeedBps:    speedBps,
			PercentDone: pct,
			EtaSecs:     eta,
		})
		a.emitSpeed(0, speedBps)
	}

	if backend.IsGCMKey(keyStr) {
		fileKey, err := backend.KeyFromStorage(keyStr)
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			a.emitError(fmt.Sprintf("Invalid key format: %v", err))
			return
		}

		for i, chunk := range chunks {
			a.emitProgress(fmt.Sprintf("Downloading part %d/%d…", chunk.ChunkIndex+1, n))

			var chunkBuf bytes.Buffer
			written, err := a.telegram.DownloadFile(chunk.FileID, &chunkBuf)
			if err != nil {
				tmpFile.Close()
				os.Remove(tmpPath)
				a.emitError(detectNetworkError(err))
				return
			}
			dlBytes += written
			elapsed := time.Since(t0).Seconds()
			speedBps := 0.0
			if elapsed > 0 {
				speedBps = float64(dlBytes) / elapsed
			}
			emitDlTransfer(i+1, dlBytes, speedBps)

			if err := backend.DecryptStream(bytes.NewReader(chunkBuf.Bytes()), tmpFile, fileKey); err != nil {
				tmpFile.Close()
				os.Remove(tmpPath)
				a.emitError(fmt.Sprintf("Decryption failed: %v", err))
				return
			}
		}
	} else {
		// Legacy Fernet: collect all chunks, concatenate, decrypt at once.
		var allData []byte
		for i, chunk := range chunks {
			a.emitProgress(fmt.Sprintf("Downloading part %d/%d…", chunk.ChunkIndex+1, n))
			var chunkBuf bytes.Buffer
			written, err := a.telegram.DownloadFile(chunk.FileID, &chunkBuf)
			if err != nil {
				tmpFile.Close()
				os.Remove(tmpPath)
				a.emitError(detectNetworkError(err))
				return
			}
			dlBytes += written
			elapsed := time.Since(t0).Seconds()
			speedBps := 0.0
			if elapsed > 0 {
				speedBps = float64(dlBytes) / elapsed
			}
			emitDlTransfer(i+1, dlBytes, speedBps)
			allData = append(allData, chunkBuf.Bytes()...)
		}
		a.emitProgress("Decrypting (legacy)…")
		plain, err := backend.DecryptLegacy(allData, []byte(keyStr))
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			a.emitError(fmt.Sprintf("Legacy decryption failed: %v", err))
			return
		}
		if _, err := tmpFile.Write(plain); err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			a.emitError(fmt.Sprintf("Write temp file failed: %v", err))
			return
		}
	}

	tmpFile.Close()

	a.mu.Lock()
	a.tempFiles = append(a.tempFiles, tmpPath)
	a.mu.Unlock()

	a.emitStatus(fmt.Sprintf("Opened '%s'  —  temp copy wiped on close.", vault.Filename), colorSuccess)
	a.emitProgress("")
	a.emitSpeed(0, 0)

	wruntime.EventsEmit(a.ctx, evDownloadReady, backend.DownloadReadyEvent{
		Path:     tmpPath,
		Filename: vault.Filename,
	})
}

// ── Internal: delete ──────────────────────────────────────────────────────────

func (a *App) doDelete(vaultID int64) {
	a.emitBusy(true)
	defer a.emitBusy(false)

	vault, err := a.db.GetVaultByID(vaultID)
	if err != nil || vault == nil {
		a.emitError("Vault not found")
		return
	}
	chunks, _ := a.db.GetVaultChunks(vaultID)

	a.emitStatus(fmt.Sprintf("Deleting '%s'…", vault.Filename), colorWarning)

	if a.telegram != nil {
		var wg sync.WaitGroup
		for _, c := range chunks {
			wg.Add(1)
			go func(msgID int64) {
				defer wg.Done()
				a.telegram.DeleteMessage(msgID)
			}(c.MessageID)
		}
		wg.Wait()
	}

	if err := a.db.DeleteVault(vaultID); err != nil {
		a.emitError(fmt.Sprintf("Database delete failed: %v", err))
		return
	}

	a.emitStatus(fmt.Sprintf("'%s' deleted.", vault.Filename), colorMuted)
	a.refreshVaults()
}

// ── Internal: gift token import ───────────────────────────────────────────────

func (a *App) doImportGift(fileIDs []string, rawKey []byte, filename string) {
	a.emitBusy(true)
	defer a.emitBusy(false)

	if filename == "" {
		filename = "imported_gift"
	}

	a.emitStatus(fmt.Sprintf("Importing gift token (%d part(s))…", len(fileIDs)), colorAccent)

	encKeyStr, err := backend.WrapValue(backend.KeyToStorage(rawKey), a.masterKey)
	if err != nil {
		a.emitError(fmt.Sprintf("Key wrap failed: %v", err))
		return
	}

	vaultID, err := a.db.InsertVault(filename, encKeyStr, 0, len(fileIDs), "gift_token")
	if err != nil {
		a.emitError(fmt.Sprintf("Database insert failed: %v", err))
		return
	}

	for i, fid := range fileIDs {
		if err := a.db.InsertVaultChunk(vaultID, i, fid, 0); err != nil {
			a.emitError(fmt.Sprintf("Chunk insert failed: %v", err))
			return
		}
	}

	a.emitStatus(fmt.Sprintf("Gift token '%s' imported.", filename), colorSuccess)
	a.refreshVaults()
}

// ── Credentials reload ────────────────────────────────────────────────────────

func (a *App) reloadTelegramClient() {
	if a.db == nil || len(a.masterKey) == 0 {
		a.telegram = nil
		return
	}
	encToken, _ := a.db.GetSetting("BOT_TOKEN", "")
	encChat, _ := a.db.GetSetting("CHAT_ID", "")
	if encToken == "" || encChat == "" {
		a.telegram = nil
		return
	}
	token, err := backend.UnwrapValue(encToken, a.masterKey)
	if err != nil {
		a.telegram = nil
		return
	}
	chatID, err := backend.UnwrapValue(encChat, a.masterKey)
	if err != nil {
		a.telegram = nil
		return
	}
	a.telegram = backend.NewTelegramClient(token, chatID)
}

// ── Migration helper ──────────────────────────────────────────────────────────

func (a *App) migratePlaintextData() {
	if a.db == nil {
		return
	}
	keysEncrypted, _ := a.db.GetSetting("KEYS_ENCRYPTED", "")
	if keysEncrypted == "1" {
		return
	}
	rows, err := a.db.GetAllVaultKeyRows()
	if err == nil {
		for _, row := range rows {
			wrapped, err := backend.WrapValue(row.Key, a.masterKey)
			if err == nil {
				a.db.UpdateVaultKey(row.ID, wrapped)
			}
		}
	}
	rawToken, _ := a.db.GetSetting("BOT_TOKEN", "")
	if rawToken != "" {
		if wrapped, err := backend.WrapValue(rawToken, a.masterKey); err == nil {
			a.db.SetSetting("BOT_TOKEN", wrapped)
		}
	}
	rawChat, _ := a.db.GetSetting("CHAT_ID", "")
	if rawChat != "" {
		if wrapped, err := backend.WrapValue(rawChat, a.masterKey); err == nil {
			a.db.SetSetting("CHAT_ID", wrapped)
		}
	}
	a.db.SetSetting("KEYS_ENCRYPTED", "1")
}

// ── Event emitters ────────────────────────────────────────────────────────────

func (a *App) emitStatus(text, color string) {
	wruntime.EventsEmit(a.ctx, evStatus, backend.StatusEvent{Text: text, Color: color})
}

func (a *App) emitProgress(text string) {
	wruntime.EventsEmit(a.ctx, evProgress, backend.ProgressEvent{Text: text})
}

func (a *App) emitSpeed(uploadBps, downloadBps float64) {
	wruntime.EventsEmit(a.ctx, evSpeed, backend.SpeedEvent{
		UploadBps:   uploadBps,
		DownloadBps: downloadBps,
	})
}

func (a *App) emitBusy(active bool) {
	wruntime.EventsEmit(a.ctx, evBusy, backend.BusyEvent{Active: active})
}

func (a *App) emitError(message string) {
	wruntime.EventsEmit(a.ctx, evErrorMsg, backend.ErrorEvent{Message: message})
	a.emitStatus(message, colorDanger)
}

func (a *App) emitTransfer(ev backend.TransferEvent) {
	wruntime.EventsEmit(a.ctx, evTransfer, ev)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// detectNetworkError wraps common network/timeout errors with a friendlier message.
func detectNetworkError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case containsAny(msg, "connection refused", "no such host", "network is unreachable",
		"i/o timeout", "EOF", "connection reset", "TLS handshake"):
		return "Network error — check your internet connection and try again."
	case containsAny(msg, "context deadline exceeded", "timeout"):
		return "Request timed out — the connection may be too slow or the server is unreachable."
	default:
		return fmt.Sprintf("Transfer failed: %v", err)
	}
}

func (a *App) refreshVaults() {
	vaults, _ := a.db.GetAllVaults()
	if vaults == nil {
		vaults = []backend.Vault{}
	}
	wruntime.EventsEmit(a.ctx, evVaultsUpdated, vaults)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func makeTempFile(ext string) (*os.File, error) {
	if goruntime.GOOS == "linux" {
		if fi, err := os.Stat("/dev/shm"); err == nil && fi.IsDir() {
			if f, err := os.CreateTemp("/dev/shm", "zv_*"+ext); err == nil {
				return f, nil
			}
		}
	}
	return os.CreateTemp("", "zv_*"+ext)
}

func (a *App) cleanupTempFiles() {
	a.mu.Lock()
	paths := a.tempFiles
	a.tempFiles = nil
	a.mu.Unlock()

	for _, p := range paths {
		secureDelete(p)
	}
}

func secureDelete(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err == nil {
		zeros := make([]byte, info.Size())
		f.Write(zeros)
		f.Sync()
		f.Close()
	}
	os.Remove(path)
}

func expandPaths(paths []string) []string {
	var result []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.IsDir() {
			filepath.Walk(p, func(path string, fi os.FileInfo, err error) error {
				if err == nil && !fi.IsDir() {
					result = append(result, path)
				}
				return nil
			})
		} else {
			result = append(result, p)
		}
	}
	return result
}

func makeZip(paths []string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.IsDir() {
			err = filepath.Walk(p, func(fpath string, fi os.FileInfo, err error) error {
				if err != nil || fi.IsDir() {
					return err
				}
				rel, _ := filepath.Rel(filepath.Dir(p), fpath)
				w, err := zw.Create(rel)
				if err != nil {
					return err
				}
				f, err := os.Open(fpath)
				if err != nil {
					return err
				}
				defer f.Close()
				_, err = io.Copy(w, f)
				return err
			})
		} else {
			w, err := zw.Create(filepath.Base(p))
			if err != nil {
				return nil, err
			}
			f, err := os.Open(p)
			if err != nil {
				return nil, err
			}
			_, err = io.Copy(w, f)
			f.Close()
			if err != nil {
				return nil, err
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func joinPaths(paths []string) string {
	result := ""
	for i, p := range paths {
		if i > 0 {
			result += "\n"
		}
		result += p
	}
	return result
}

// hexDecode decodes a hex string to bytes (fallback for fmt.Sscanf limitation).
func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, errors.New("odd hex string length")
	}
	b := make([]byte, len(s)/2)
	for i := range b {
		hi := hexNibble(s[i*2])
		lo := hexNibble(s[i*2+1])
		if hi < 0 || lo < 0 {
			return nil, fmt.Errorf("invalid hex char at %d", i*2)
		}
		b[i] = byte(hi<<4 | lo)
	}
	return b, nil
}

func hexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
