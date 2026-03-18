// backend/encryption.go — Zero-knowledge local encryption layer.
//
// Byte-format compatible with the Python ZenithVault implementation:
//
//	Master key:  scrypt(password, salt, N=2^17, r=8, p=1, len=32) → base64url → Fernet key
//	File key:    32 raw bytes, stored as "gcm:" + hex64 (wrapped with master key via Fernet)
//	Streaming:   per-segment: [4-byte BE plaintext_len][12-byte nonce][ciphertext][16-byte GCM tag]
//	Legacy:      Fernet-encrypted blob (detected by absence of "gcm:" prefix)
//	Gift token:  base64url( '{"fids":[...],"k":"gcm:<64-hex>"}' )
package backend

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	fernetpkg "github.com/fernet/fernet-go"
	"golang.org/x/crypto/scrypt"
)

const (
	EncryptBuffer = 2 * 1024 * 1024 // 2 MB streaming segment buffer
	gcmKeySize    = 32
	gcmNonceSize  = 12
	gcmTagSize    = 16
	gcmKeyPrefix  = "gcm:"
	scryptN       = 1 << 17 // 2^17
	scryptR       = 8
	scryptP       = 1
	saltSize      = 32
)

// ── Key management ────────────────────────────────────────────────────────────

// GenerateKey returns a fresh 32-byte AES-256-GCM key.
func GenerateKey() ([]byte, error) {
	key := make([]byte, gcmKeySize)
	_, err := rand.Read(key)
	return key, err
}

// GenerateSalt returns a fresh 32-byte cryptographic salt.
func GenerateSalt() ([]byte, error) {
	salt := make([]byte, saltSize)
	_, err := rand.Read(salt)
	return salt, err
}

// IsGCMKey reports whether the storage string represents a GCM key.
func IsGCMKey(keyStr string) bool {
	return strings.HasPrefix(keyStr, gcmKeyPrefix)
}

// KeyToStorage encodes a raw 32-byte key for DB / gift-token embedding.
func KeyToStorage(key []byte) string {
	return gcmKeyPrefix + hex.EncodeToString(key)
}

// KeyFromStorage decodes a GCM key string produced by KeyToStorage.
func KeyFromStorage(keyStr string) ([]byte, error) {
	if !IsGCMKey(keyStr) {
		return nil, errors.New("not a GCM key string; caller must handle legacy Fernet path")
	}
	return hex.DecodeString(keyStr[len(gcmKeyPrefix):])
}

// ── Master-key derivation ─────────────────────────────────────────────────────

// DeriveMasterKey derives a Fernet-compatible master key from password and salt.
// Returns the base64url-encoded 32-byte scrypt output (same format as Python).
func DeriveMasterKey(password string, salt []byte) ([]byte, error) {
	raw, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, 32)
	if err != nil {
		return nil, err
	}
	encoded := base64.URLEncoding.EncodeToString(raw)
	return []byte(encoded), nil
}

// fernetKey converts the masterKey (base64url bytes) into a *fernetpkg.Key.
func fernetKey(masterKey []byte) (*fernetpkg.Key, error) {
	k, err := fernetpkg.DecodeKey(string(masterKey))
	if err != nil {
		return nil, fmt.Errorf("invalid master key: %w", err)
	}
	return k, nil
}

// WrapValue encrypts a string value with the master key (Fernet).
// Returns a base64 Fernet token string — matches Python's wrap_value().
func WrapValue(value string, masterKey []byte) (string, error) {
	k, err := fernetKey(masterKey)
	if err != nil {
		return "", err
	}
	tok, err := fernetpkg.EncryptAndSign([]byte(value), k)
	if err != nil {
		return "", err
	}
	return string(tok), nil
}

// UnwrapValue decrypts a Fernet token string produced by WrapValue.
// Returns an error if the master key is wrong or the token is tampered with.
func UnwrapValue(wrapped string, masterKey []byte) (string, error) {
	k, err := fernetKey(masterKey)
	if err != nil {
		return "", err
	}
	plain := fernetpkg.VerifyAndDecrypt([]byte(wrapped), 0, []*fernetpkg.Key{k})
	if plain == nil {
		return "", errors.New("invalid Fernet token or wrong master key")
	}
	return string(plain), nil
}

// ── Streaming AES-256-GCM encrypt / decrypt ───────────────────────────────────

// EncryptSegment encrypts a plaintext block and returns the serialised segment:
//
//	[4-byte BE plaintext_len][12-byte nonce][ciphertext][16-byte GCM tag]
func EncryptSegment(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcmNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	// gcm.Seal returns ciphertext || tag (tag is 16 bytes at the end)
	sealed := gcm.Seal(nil, nonce, plaintext, nil)
	ct := sealed[:len(plaintext)]
	tag := sealed[len(plaintext):]

	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(plaintext)))

	out := make([]byte, 0, 4+gcmNonceSize+len(ct)+gcmTagSize)
	out = append(out, header...)
	out = append(out, nonce...)
	out = append(out, ct...)
	out = append(out, tag...)
	return out, nil
}

// DecryptSegment reads one segment from r and returns the plaintext.
// Returns io.EOF on clean end-of-stream.
func DecryptSegment(r io.Reader, key []byte) ([]byte, error) {
	var ptLenBuf [4]byte
	if _, err := io.ReadFull(r, ptLenBuf[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("read segment header: %w", err)
	}
	ptLen := binary.BigEndian.Uint32(ptLenBuf[:])

	nonce := make([]byte, gcmNonceSize)
	if _, err := io.ReadFull(r, nonce); err != nil {
		return nil, fmt.Errorf("truncated nonce: %w", err)
	}

	ct := make([]byte, ptLen)
	if _, err := io.ReadFull(r, ct); err != nil {
		return nil, fmt.Errorf("truncated ciphertext: %w", err)
	}

	tag := make([]byte, gcmTagSize)
	if _, err := io.ReadFull(r, tag); err != nil {
		return nil, fmt.Errorf("truncated GCM tag: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	// Reconstruct ciphertext+tag as GCM expects
	ciphertextWithTag := append(ct, tag...) //nolint:gocritic
	plaintext, err := gcm.Open(nil, nonce, ciphertextWithTag, nil)
	if err != nil {
		return nil, fmt.Errorf("AES-GCM authentication failed: %w", err)
	}
	return plaintext, nil
}

// EncryptStream reads from src in EncryptBuffer-sized blocks, encrypts each,
// and writes the serialised segments to dst. Returns total bytes written.
func EncryptStream(src io.Reader, dst io.Writer, key []byte) (int64, error) {
	buf := make([]byte, EncryptBuffer)
	var total int64
	for {
		n, readErr := io.ReadFull(src, buf)
		if n > 0 {
			seg, err := EncryptSegment(buf[:n], key)
			if err != nil {
				return total, err
			}
			written, err := dst.Write(seg)
			total += int64(written)
			if err != nil {
				return total, err
			}
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return total, readErr
		}
	}
	return total, nil
}

// DecryptStream reads AES-GCM segments from src and writes plaintext to dst.
func DecryptStream(src io.Reader, dst io.Writer, key []byte) error {
	for {
		plain, err := DecryptSegment(src, key)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := dst.Write(plain); err != nil {
			return err
		}
	}
}

// ── Legacy Fernet (backward-compat read-only) ─────────────────────────────────

// DecryptLegacy decrypts a Fernet-encrypted blob (old ZenithVault format).
// fernetKey is the raw Fernet key bytes (base64url-encoded 32-byte key, as stored in DB).
func DecryptLegacy(data, fernetKeyBytes []byte) ([]byte, error) {
	k, err := fernetpkg.DecodeKey(string(fernetKeyBytes))
	if err != nil {
		return nil, fmt.Errorf("invalid legacy fernet key: %w", err)
	}
	plain := fernetpkg.VerifyAndDecrypt(data, 0, []*fernetpkg.Key{k})
	if plain == nil {
		return nil, errors.New("legacy Fernet decryption failed: wrong key or corrupted data")
	}
	return plain, nil
}

// ── Gift tokens ───────────────────────────────────────────────────────────────

type giftPayload struct {
	FileIDs []string `json:"fids"`
	Key     string   `json:"k"`
}

// GenerateGiftToken encodes a shareable gift token for the given file IDs and raw AES key.
func GenerateGiftToken(fileIDs []string, rawKey []byte) (string, error) {
	payload := giftPayload{
		FileIDs: fileIDs,
		Key:     KeyToStorage(rawKey),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(data), nil
}

// ParseGiftToken decodes a gift token → (fileIDs, rawKey).
// Returns an error for malformed input or legacy tokens that cannot be imported.
func ParseGiftToken(token string) ([]string, []byte, error) {
	raw, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		// Try without padding (Python urlsafe_b64encode may omit it)
		padded := token
		switch len(token) % 4 {
		case 2:
			padded += "=="
		case 3:
			padded += "="
		}
		raw, err = base64.URLEncoding.DecodeString(padded)
		if err != nil {
			return nil, nil, errors.New("gift token is not valid base64")
		}
	}

	if !strings.HasPrefix(string(raw), "{") {
		return nil, nil, errors.New("this is a legacy v1 gift token and cannot be imported; the sender must re-upload the file")
	}

	var p giftPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, fmt.Errorf("malformed JSON gift token: %w", err)
	}

	if !IsGCMKey(p.Key) {
		return nil, nil, errors.New("this gift token uses the legacy Fernet format and cannot be imported; the sender must re-upload the file with the current version")
	}

	rawKey, err := KeyFromStorage(p.Key)
	if err != nil {
		return nil, nil, err
	}
	return p.FileIDs, rawKey, nil
}
