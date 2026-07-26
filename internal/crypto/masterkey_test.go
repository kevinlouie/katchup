package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
)

func TestMasterKeyWrapper_WrapUnwrap_RandomNonce(t *testing.T) {
	masterKey := "test-master-key-for-unit-tests"
	wrapper, err := NewMasterKeyWrapper(masterKey)
	if err != nil {
		t.Fatalf("NewMasterKeyWrapper: %v", err)
	}

	// Create a 32-byte content key
	ck := make([]byte, 32)
	if _, err := rand.Read(ck); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	// Wrap twice — each wrap must produce a DIFFERENT ciphertext
	// because the nonce is now random (fix #5).
	wrapped1, err := wrapper.Wrap(ck)
	if err != nil {
		t.Fatalf("Wrap 1: %v", err)
	}
	wrapped2, err := wrapper.Wrap(ck)
	if err != nil {
		t.Fatalf("Wrap 2: %v", err)
	}

	// Same content key + same master key + DIFFERENT nonces = DIFFERENT ciphertexts
	if bytes.Equal(wrapped1, wrapped2) {
		t.Fatal("Wrap produced identical ciphertexts for same key — nonce reuse bug still present")
	}

	// Both must unwrap to the same plaintext
	plain1, err := wrapper.Unwrap(wrapped1)
	if err != nil {
		t.Fatalf("Unwrap 1: %v", err)
	}
	plain2, err := wrapper.Unwrap(wrapped2)
	if err != nil {
		t.Fatalf("Unwrap 2: %v", err)
	}

	if !bytes.Equal(plain1, ck) || !bytes.Equal(plain2, ck) {
		t.Fatal("Unwrap produced wrong plaintext")
	}
}

func TestMasterKeyWrapper_Wrap_DeterministicOutputSize(t *testing.T) {
	masterKey := "test-key"
	wrapper, err := NewMasterKeyWrapper(masterKey)
	if err != nil {
		t.Fatalf("NewMasterKeyWrapper: %v", err)
	}

	ck := make([]byte, 32)
	if _, err := rand.Read(ck); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	// Wrap multiple times and verify output size is constant
	// (nonce is random but included in output, so blob size is fixed)
	sizes := make(map[int]int)
	for i := 0; i < 10; i++ {
		wrapped, err := wrapper.Wrap(ck)
		if err != nil {
			t.Fatalf("Wrap %d: %v", i, err)
		}
		sizes[len(wrapped)]++
	}

	if len(sizes) != 1 {
		t.Fatalf("Wrap output size not constant across 10 wraps: %v", sizes)
	}

	expectedSize := nonceSize + aesKeySize + tagSize
	if got := sizes[expectedSize]; got != 10 {
		t.Fatalf("expected all 10 wraps to produce size %d, got %d of %d", expectedSize, got, 10)
	}
}

func TestEncryptDecryptFile_RoundTrip(t *testing.T) {
	masterKey := "test-key"
	wrapper, err := NewMasterKeyWrapper(masterKey)
	if err != nil {
		t.Fatalf("NewMasterKeyWrapper: %v", err)
	}

	plaintext := []byte("Hello, World! This is a test email body.")

	dir := t.TempDir()
	path := dir + "/test.eml.enc"

	if err := EncryptFile(path, plaintext, wrapper); err != nil {
		t.Fatalf("EncryptFile: %v", err)
	}

	// File should exist
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}

	// Decrypt
	got, err := DecryptFile(path, wrapper)
	if err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}

	if !bytes.Equal(got, plaintext) {
		t.Fatalf("decrypted file doesn't match: got %q, want %q", got, plaintext)
	}
}

func TestEncryptDecryptPassword_RoundTrip(t *testing.T) {
	masterKey := "test-master-key"
	password := "my-secret-password"

	encrypted, err := EncryptPassword(masterKey, password)
	if err != nil {
		t.Fatalf("EncryptPassword: %v", err)
	}

	// Should be hex-encoded (no binary)
	if _, err := hex.DecodeString(encrypted); err != nil {
		t.Fatalf("encrypted password is not hex-encoded: %v", err)
	}

	decrypted, err := DecryptPassword(masterKey, encrypted)
	if err != nil {
		t.Fatalf("DecryptPassword: %v", err)
	}

	if decrypted != password {
		t.Fatalf("decrypted password mismatch: got %q, want %q", decrypted, password)
	}
}

func TestEncryptPassword_NoMasterKey(t *testing.T) {
	_, err := EncryptPassword("", "password")
	if err != ErrNoMasterKey {
		t.Fatalf("expected ErrNoMasterKey, got %v", err)
	}
}

func TestDecryptPassword_BadCiphertext(t *testing.T) {
	_, err := DecryptPassword("key", "not-valid-hex!!!")
	if err == nil {
		t.Fatal("expected error for invalid hex, got nil")
	}
}

func TestDecryptPassword_WrongKey(t *testing.T) {
	encrypted, err := EncryptPassword("correct-key", "password")
	if err != nil {
		t.Fatalf("EncryptPassword: %v", err)
	}

	_, err = DecryptPassword("wrong-key", encrypted)
	if err != ErrDecryptionFailed {
		t.Fatalf("expected ErrDecryptionFailed, got %v", err)
	}
}
