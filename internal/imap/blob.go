package imap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"katchup/internal/crypto"
)

// ErrInvalidBlobPath is returned by ReadMessageBlob when a blob path resolves
// outside the data dir.
var ErrInvalidBlobPath = errors.New("blob path escapes data dir")

// ReadMessageBlob returns the plaintext .eml for a stored blob. blobPath is
// relative to dataDir (as recorded in blobs.path); it is trusted (written by the
// sync path) but still confirmed to stay under dataDir before reading. When
// keyWrapper is nil the file is read as-is (encryption disabled). A missing file
// is reported as an error wrapping os.ErrNotExist.
func ReadMessageBlob(dataDir, blobPath string, keyWrapper crypto.KeyWrapper) ([]byte, error) {
	path := filepath.Join(dataDir, filepath.Clean(blobPath))
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data dir: %w", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve blob path: %w", err)
	}
	if !strings.HasPrefix(absPath, absDataDir+string(filepath.Separator)) && absPath != absDataDir {
		return nil, ErrInvalidBlobPath
	}

	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	if keyWrapper != nil {
		return crypto.DecryptFile(path, keyWrapper)
	}
	return os.ReadFile(path)
}
