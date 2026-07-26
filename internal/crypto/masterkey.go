package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// FormatMasterKey is the .eml.enc file format version byte: AES master key
	// wrap. (0x01 was reserved for a YubiKey PIV RSA wrap that never shipped.)
	FormatMasterKey = 0x02

	// AES-GCM constants
	nonceSize          = 12
	tagSize            = 16
	aesKeySize         = 32                               // AES-256 key size in bytes
	masterWrapSize     = nonceSize + aesKeySize + tagSize // AES-GCM seal output: nonce + ciphertext + tag
	masterKeyPrefixLen = 16                               // hex chars stored as prefix for DB lookup
)

var (
	ErrNoMasterKey      = errors.New("crypto: KATCHUP_MASTER_KEY is not set")
	ErrDecryptionFailed = errors.New("crypto: decryption failed — invalid tag or wrong key")
	ErrUnknownFormat    = errors.New("crypto: unknown file format version")
)

// KeyWrapper wraps and unwraps a 32-byte content key.
type KeyWrapper interface {
	// Wrap encrypts the content key and returns the encrypted bytes.
	Wrap(ck []byte) ([]byte, error)
	// Unwrap decrypts an encrypted content key and returns the plaintext 32-byte key.
	Unwrap(encrypted []byte) ([]byte, error)
	// ID returns a short identifier for this wrapper (e.g. slot ID or "master").
	ID() string
	// Fingerprint returns a hex string identifying the wrapping key material.
	Fingerprint() string
}

// MasterKeyWrapper uses KATCHUP_MASTER_KEY with AES-256-GCM to wrap/unwrap content keys.
type MasterKeyWrapper struct {
	key    []byte
	finger string
}

// NewMasterKeyWrapper derives a 256-bit key from the master key string.
func NewMasterKeyWrapper(masterKey string) (*MasterKeyWrapper, error) {
	if masterKey == "" {
		return nil, ErrNoMasterKey
	}
	h := sha256.Sum256([]byte(masterKey))
	// The fingerprint is derived through a second, domain-separated hash so the
	// stored/displayed value shares no bytes with the actual encryption key.
	fp := sha256.Sum256([]byte("katchup-fingerprint:" + masterKey))
	return &MasterKeyWrapper{
		key:    h[:],
		finger: hex.EncodeToString(fp[:4]),
	}, nil
}

func (m *MasterKeyWrapper) Wrap(ck []byte) ([]byte, error) {
	if len(ck) != 32 {
		return nil, fmt.Errorf("crypto: content key must be 32 bytes, got %d", len(ck))
	}
	block, err := aes.NewCipher(m.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// Use a random nonce.
	// All-zero nonces with the same content key under the same master key
	// leak the XOR of plaintexts and enable authentication-key
	// distinguishability attacks. gcm.Seal prepends the nonce to the
	// output, so a random nonce keeps the blob size fixed.
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: generate nonce: %w", err)
	}
	// Seal produces nonce + ciphertext + tag (16 bytes).
	return gcm.Seal(nonce, nonce, ck, nil), nil
}

func (m *MasterKeyWrapper) Unwrap(encrypted []byte) ([]byte, error) {
	block, err := aes.NewCipher(m.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(encrypted) < nonceSize+tagSize {
		return nil, errors.New("crypto: encrypted key too short")
	}
	nonce := encrypted[:nonceSize]
	ciphertext := encrypted[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func (m *MasterKeyWrapper) ID() string          { return "master" }
func (m *MasterKeyWrapper) Fingerprint() string { return m.finger }

// GenerateContentKey returns a random 256-bit content key.
func GenerateContentKey() ([]byte, error) {
	ck := make([]byte, 32)
	if _, err := rand.Read(ck); err != nil {
		return nil, fmt.Errorf("crypto: generate content key: %w", err)
	}
	return ck, nil
}

// EncryptFile encrypts a plaintext file using AES-256-GCM with a per-file content key,
// wrapped by the provided KeyWrapper. Writes the encrypted file atomically.
//
// File layout:
//
//	version (1 byte) | nonce (12) | wrapped_ck (varies by wrapper) | tag (16) | ciphertext (var)
func EncryptFile(path string, plaintext []byte, wrapper KeyWrapper) error {
	ck, err := GenerateContentKey()
	if err != nil {
		return err
	}

	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}

	wrappedCK, err := wrapper.Wrap(ck)
	if err != nil {
		return fmt.Errorf("crypto: wrap content key: %w", err)
	}

	block, err := aes.NewCipher(ck)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	// Version byte: only the master-key wrap format exists. A future wrapper
	// type must claim its own version byte here.
	if _, ok := wrapper.(*MasterKeyWrapper); !ok {
		return fmt.Errorf("crypto: no file format version for wrapper %q", wrapper.ID())
	}
	version := byte(FormatMasterKey)

	// Determine directory for temp file (must be same device as target for atomic rename).
	dir := filepath.Dir(path)

	tmpFile, err := os.CreateTemp(dir, ".eml.enc.tmp.*")
	if err != nil {
		return fmt.Errorf("crypto: create temp file: %w", err)
	}
	tmpName := tmpFile.Name()

	writeBuf := make([]byte, 0, 1+nonceSize+len(wrappedCK)+tagSize+len(ciphertext))
	writeBuf = append(writeBuf, version)
	writeBuf = append(writeBuf, nonce...)
	writeBuf = append(writeBuf, wrappedCK...)
	writeBuf = append(writeBuf, ciphertext...)

	if _, err := tmpFile.Write(writeBuf); err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return fmt.Errorf("crypto: write encrypted file: %w", err)
	}
	if err := tmpFile.Chmod(0600); err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return fmt.Errorf("crypto: set permissions: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}

	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("crypto: rename encrypted file: %w", err)
	}

	return nil
}

// DecryptFile reads an encrypted file and returns the plaintext.
func DecryptFile(path string, wrapper KeyWrapper) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("crypto: read file: %w", err)
	}
	return DecryptBytes(data, wrapper)
}

// DecryptBytes decrypts an in-memory encrypted blob.
func DecryptBytes(data []byte, wrapper KeyWrapper) ([]byte, error) {
	if len(data) < 1+nonceSize+tagSize {
		return nil, errors.New("crypto: file too short")
	}

	version := data[0]
	offset := 1

	var wrappedCKSize int
	switch version {
	case FormatMasterKey:
		wrappedCKSize = masterWrapSize // ciphertext + GCM tag from sealing
	default:
		return nil, fmt.Errorf("%w: 0x%02x", ErrUnknownFormat, version)
	}

	headerLen := 1 + nonceSize + wrappedCKSize + tagSize
	if len(data) < headerLen {
		return nil, errors.New("crypto: file too short for header")
	}

	nonce := data[offset : offset+nonceSize]
	offset += nonceSize

	encryptedCK := data[offset : offset+wrappedCKSize]
	offset += wrappedCKSize

	ciphertext := data[offset:]

	// Unwrap the content key.
	ck, err := wrapper.Unwrap(encryptedCK)
	if err != nil {
		return nil, fmt.Errorf("crypto: unwrap content key: %w", err)
	}

	block, err := aes.NewCipher(ck)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrDecryptionFailed
	}

	return plaintext, nil
}

// EncryptPassword encrypts a password string using AES-256-GCM with a key derived from the master key.
// Returns hex-encoded: nonce + ciphertext + tag.
func EncryptPassword(masterKey, password string) (string, error) {
	if masterKey == "" {
		return "", ErrNoMasterKey
	}
	h := sha256.Sum256([]byte(masterKey))
	block, err := aes.NewCipher(h[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, []byte(password), nil)
	result := make([]byte, 0, hex.EncodedLen(len(nonce)+len(ct)))
	result = append(result, []byte(hex.EncodeToString(nonce))...)
	result = append(result, []byte(hex.EncodeToString(ct))...)
	return string(result), nil
}

// DecryptPassword decrypts a hex-encoded password.
func DecryptPassword(masterKey, encrypted string) (string, error) {
	if masterKey == "" {
		return "", ErrNoMasterKey
	}
	encoded, err := hex.DecodeString(encrypted)
	if err != nil {
		return "", fmt.Errorf("crypto: decode password: %w", err)
	}
	if len(encoded) < nonceSize+tagSize {
		return "", errors.New("crypto: encrypted password too short")
	}
	nonce, ct := encoded[:nonceSize], encoded[nonceSize:]
	h := sha256.Sum256([]byte(masterKey))
	block, err := aes.NewCipher(h[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", ErrDecryptionFailed
	}
	return string(plaintext), nil
}

// EncryptedKeyPrefix returns the first masterKeyPrefixLen hex characters of the
// encrypted content key, suitable for storing in the database.
func EncryptedKeyPrefix(encryptedCK []byte) string {
	if len(encryptedCK) < masterKeyPrefixLen/2 {
		return hex.EncodeToString(encryptedCK)
	}
	return hex.EncodeToString(encryptedCK[:masterKeyPrefixLen/2])
}

// MatchPrefix checks if the encryptedCK starts with the stored prefix.
func MatchPrefix(encryptedCK, prefix string) bool {
	return len(encryptedCK) >= len(prefix) && string(encryptedCK[:len(prefix)]) == prefix
}
