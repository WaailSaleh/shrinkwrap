// backend/database.go — SQLite persistence layer (schema v3 compatible with Python app).
package backend

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite connection pool.
type DB struct {
	conn *sql.DB
}

// dbPath returns the path to the vault database file.
func dbPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".zenithvault")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "vault.db"), nil
}

// OpenDB opens (or creates) the vault database and runs migrations.
func OpenDB() (*DB, error) {
	path, err := dbPath()
	if err != nil {
		return nil, err
	}
	conn, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1) // SQLite is single-writer
	db := &DB{conn: conn}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, err
	}
	return db, nil
}

// Close closes the underlying database connection.
func (db *DB) Close() error {
	return db.conn.Close()
}

// ── Schema migration ───────────────────────────────────────────────────────────

func (db *DB) migrate() error {
	// Settings table always survives migrations.
	if _, err := db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT ''
		)`); err != nil {
		return fmt.Errorf("create settings: %w", err)
	}

	// vault_chunks is safe to create in all cases.
	if _, err := db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS vault_chunks (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			vault_id    INTEGER NOT NULL,
			chunk_index INTEGER NOT NULL,
			file_id     TEXT    NOT NULL,
			message_id  INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("create vault_chunks: %w", err)
	}

	// Inspect vaults columns.
	cols, err := db.tableColumns("vaults")
	if err != nil {
		return err
	}

	switch {
	case len(cols) == 0:
		// Fresh install — create v3 schema.
		if _, err := db.conn.Exec(`
			CREATE TABLE vaults (
				id              INTEGER PRIMARY KEY AUTOINCREMENT,
				filename        TEXT    NOT NULL,
				encryption_key  TEXT    NOT NULL,
				file_size       INTEGER DEFAULT 0,
				chunk_count     INTEGER DEFAULT 1,
				uploaded_at     TEXT    NOT NULL,
				source_path     TEXT    NOT NULL DEFAULT ''
			)`); err != nil {
			return fmt.Errorf("create vaults: %w", err)
		}

	case cols["file_id"]:
		// v1 detected: file_id/message_id are in vaults — migrate to v2.
		if err := db.migrateV1toV2(); err != nil {
			return fmt.Errorf("v1→v2 migration: %w", err)
		}
		cols, _ = db.tableColumns("vaults")
		fallthrough

	default:
		// v2 → v3: add source_path if missing.
		if !cols["source_path"] {
			if _, err := db.conn.Exec(
				`ALTER TABLE vaults ADD COLUMN source_path TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("add source_path: %w", err)
			}
		}
	}
	return nil
}

// tableColumns returns a set of column names for the given table (empty set if table does not exist).
func (db *DB) tableColumns(table string) (map[string]bool, error) {
	rows, err := db.conn.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

func (db *DB) migrateV1toV2() error {
	type oldRow struct {
		id, fileID, messageID string
		filename, encKey      string
		fileSize              int64
		uploadedAt            string
	}

	rows, err := db.conn.Query(
		`SELECT id, filename, file_id, message_id, encryption_key, file_size, uploaded_at FROM vaults`,
	)
	if err != nil {
		return err
	}
	var old []oldRow
	for rows.Next() {
		var r oldRow
		if err := rows.Scan(&r.id, &r.filename, &r.fileID, &r.messageID,
			&r.encKey, &r.fileSize, &r.uploadedAt); err != nil {
			rows.Close()
			return err
		}
		old = append(old, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}

	if _, err := tx.Exec(`DROP TABLE vaults`); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`
		CREATE TABLE vaults (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			filename        TEXT    NOT NULL,
			encryption_key  TEXT    NOT NULL,
			file_size       INTEGER DEFAULT 0,
			chunk_count     INTEGER DEFAULT 1,
			uploaded_at     TEXT    NOT NULL
		)`); err != nil {
		tx.Rollback()
		return err
	}

	for _, r := range old {
		if _, err := tx.Exec(
			`INSERT INTO vaults (id,filename,encryption_key,file_size,chunk_count,uploaded_at) VALUES(?,?,?,?,1,?)`,
			r.id, r.filename, r.encKey, r.fileSize, r.uploadedAt,
		); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO vault_chunks (vault_id,chunk_index,file_id,message_id) VALUES(?,0,?,?)`,
			r.id, r.fileID, r.messageID,
		); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ── Write operations ──────────────────────────────────────────────────────────

// InsertVault inserts a vault header row and returns the new row id.
func (db *DB) InsertVault(filename, encryptionKey string, fileSize int64, chunkCount int, sourcePath string) (int64, error) {
	res, err := db.conn.Exec(
		`INSERT INTO vaults (filename,encryption_key,file_size,chunk_count,uploaded_at,source_path) VALUES(?,?,?,?,?,?)`,
		filename, encryptionKey, fileSize, chunkCount,
		time.Now().Format(time.RFC3339), sourcePath,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// InsertVaultChunk inserts a single chunk record.
func (db *DB) InsertVaultChunk(vaultID int64, chunkIndex int, fileID string, messageID int64) error {
	_, err := db.conn.Exec(
		`INSERT INTO vault_chunks (vault_id,chunk_index,file_id,message_id) VALUES(?,?,?,?)`,
		vaultID, chunkIndex, fileID, messageID,
	)
	return err
}

// DeleteVault removes the vault header and all its chunk records.
func (db *DB) DeleteVault(vaultID int64) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM vault_chunks WHERE vault_id=?`, vaultID); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM vaults WHERE id=?`, vaultID); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// UpdateVaultKey updates the encryption_key for a vault (used during migration).
func (db *DB) UpdateVaultKey(vaultID int64, encKey string) error {
	_, err := db.conn.Exec(`UPDATE vaults SET encryption_key=? WHERE id=?`, encKey, vaultID)
	return err
}

// ── Read operations ───────────────────────────────────────────────────────────

// GetAllVaults returns all vault headers, newest first.
func (db *DB) GetAllVaults() ([]Vault, error) {
	rows, err := db.conn.Query(
		`SELECT id,filename,encryption_key,file_size,chunk_count,uploaded_at,source_path FROM vaults ORDER BY uploaded_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vaults []Vault
	for rows.Next() {
		var v Vault
		if err := rows.Scan(&v.ID, &v.Filename, &v.EncryptionKey, &v.FileSize,
			&v.ChunkCount, &v.UploadedAt, &v.SourcePath); err != nil {
			return nil, err
		}
		vaults = append(vaults, v)
	}
	return vaults, rows.Err()
}

// GetVaultByID returns a single vault or nil if not found.
func (db *DB) GetVaultByID(vaultID int64) (*Vault, error) {
	row := db.conn.QueryRow(
		`SELECT id,filename,encryption_key,file_size,chunk_count,uploaded_at,source_path FROM vaults WHERE id=?`,
		vaultID,
	)
	var v Vault
	if err := row.Scan(&v.ID, &v.Filename, &v.EncryptionKey, &v.FileSize,
		&v.ChunkCount, &v.UploadedAt, &v.SourcePath); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &v, nil
}

// GetVaultChunks returns all chunks for a vault ordered by chunk_index.
func (db *DB) GetVaultChunks(vaultID int64) ([]VaultChunk, error) {
	rows, err := db.conn.Query(
		`SELECT id,vault_id,chunk_index,file_id,message_id FROM vault_chunks WHERE vault_id=? ORDER BY chunk_index ASC`,
		vaultID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []VaultChunk
	for rows.Next() {
		var c VaultChunk
		if err := rows.Scan(&c.ID, &c.VaultID, &c.ChunkIndex, &c.FileID, &c.MessageID); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// GetAllVaultKeyRows returns (id, encryption_key) for every vault (used for master-key migration).
func (db *DB) GetAllVaultKeyRows() ([]struct {
	ID  int64
	Key string
}, error) {
	rows, err := db.conn.Query(`SELECT id,encryption_key FROM vaults`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []struct {
		ID  int64
		Key string
	}
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, err
		}
		out = append(out, struct {
			ID  int64
			Key string
		}{id, key})
	}
	return out, rows.Err()
}

// ── Settings ──────────────────────────────────────────────────────────────────

// GetSetting reads a setting value; returns defaultVal if not set.
func (db *DB) GetSetting(key, defaultVal string) (string, error) {
	var value string
	err := db.conn.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return defaultVal, nil
	}
	if err != nil {
		return defaultVal, err
	}
	return value, nil
}

// SetSetting persists a setting value (upsert).
func (db *DB) SetSetting(key, value string) error {
	_, err := db.conn.Exec(
		`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value,
	)
	return err
}
