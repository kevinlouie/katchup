package imap

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"katchup/internal/account"
	"katchup/internal/crypto"
)

// keyedSyncer builds a Syncer over db/dataDir configured with masterKey ("" =
// encryption off), the way main wires it.
func keyedSyncer(t *testing.T, db *sql.DB, dataDir, masterKey string) *Syncer {
	t.Helper()
	acctStore, err := account.New(db, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(db, acctStore)
	if err != nil {
		t.Fatal(err)
	}
	var kw crypto.KeyWrapper
	if masterKey != "" {
		if kw, err = crypto.NewMasterKeyWrapper(masterKey); err != nil {
			t.Fatal(err)
		}
	}
	return NewSyncer(store, dataDir, kw, acctStore)
}

func TestCheckMasterKey(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "k.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migrateTestDB(t, db)
	dataDir := t.TempDir()

	// Archive written under key k1: encrypted password, encryption row, blob.
	orig := keyedSyncer(t, db, dataDir, "k1")
	pw, err := orig.store.accountSt.EncryptPassword("imap-pass")
	if err != nil {
		t.Fatal(err)
	}
	acct, err := orig.store.accountSt.CreateAccount(ctx, "Mail", "imap.example.com", 993, "u", pw, true, []string{"INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orig.store.accountSt.UpsertEncryption(ctx, acct.ID, "master", "fp", "fp"); err != nil {
		t.Fatal(err)
	}
	rel := orig.relEmlPath(acct.ID, "INBOX", "2026-01-01", 1, 1)
	if err := orig.writeEML(filepath.Join(dataDir, rel), []byte(sampleEML)); err != nil {
		t.Fatal(err)
	}
	if _, err := orig.store.UpsertBlob(ctx, acct.ID, "sha", rel, 1); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		key  string
		want []string // substrings, one per expected problem
	}{
		{"same key", "k1", nil},
		{"changed key", "k2", []string{"password doesn't decrypt", "don't decrypt with this key"}},
		{"key removed", "", []string{"set up with a master key", "encrypted but no master key"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems, err := keyedSyncer(t, db, dataDir, tt.key).CheckMasterKey(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) != len(tt.want) {
				t.Fatalf("problems = %q, want %d", problems, len(tt.want))
			}
			for i, w := range tt.want {
				if !strings.Contains(problems[i], w) {
					t.Errorf("problem %d = %q, want it to mention %q", i, problems[i], w)
				}
			}
		})
	}
}

// TestReadMessageBlobByFormat: the file name, not the configuration, decides
// whether a blob is decrypted — plaintext mail archived before a master key
// was set stays readable, and an encrypted blob without a key fails clearly.
func TestReadMessageBlobByFormat(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "old.eml"), []byte(sampleEML), 0600); err != nil {
		t.Fatal(err)
	}
	kw, _ := crypto.NewMasterKeyWrapper("k1")
	if err := crypto.EncryptFile(filepath.Join(dataDir, "new.eml.enc"), []byte(sampleEML), kw); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"old.eml", "new.eml.enc"} {
		got, err := ReadMessageBlob(dataDir, name, kw)
		if err != nil || string(got) != sampleEML {
			t.Errorf("read %s with key: err=%v, match=%v", name, err, string(got) == sampleEML)
		}
	}
	if got, err := ReadMessageBlob(dataDir, "old.eml", nil); err != nil || string(got) != sampleEML {
		t.Errorf("read plaintext without key: err=%v", err)
	}
	if _, err := ReadMessageBlob(dataDir, "new.eml.enc", nil); err == nil || !strings.Contains(err.Error(), "KATCHUP_MASTER_KEY") {
		t.Errorf("encrypted blob without key: err=%v, want a missing-key error", err)
	}
}
