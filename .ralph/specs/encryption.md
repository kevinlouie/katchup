# Encryption Specification

## Reality note
An earlier design called for YubiKey PIV + RSA-2048 key wrapping. **It was
never implemented** — there is no PIV library, no `ykman` runtime path. The
`FormatRSA = 0x01` branch exists as a stub but is never written. Katchup
encrypts with a **master key only** (`FormatMasterKey = 0x02`). Treat PIV as a
possible future format, not a current feature.

## Overview
Katchup encrypts `.eml.enc` files at rest with AES-256-GCM. The content key is
per-file (random) and wrapped by a key derived from `KATCHUP_MASTER_KEY`.

## Key hierarchy (as built)
```
KATCHUP_MASTER_KEY (env, required for encryption)
  └── SHA-256(master key) = 32-byte key-wrapping key   (MasterKeyWrapper)
        └── wraps a random 256-bit content key (CK) per file  (AES-256-GCM)
              └── CK encrypts the .eml with AES-256-GCM
```

## File format (`FormatMasterKey = 0x02`)
The `.eml.enc` file is **self-describing** — it carries its own wrapped content
key. There is no external RSA blob and no dependency on the DB to decrypt beyond
the master key.
```
[version 0x02][wrapped-CK + nonce/tag][file nonce][ciphertext + GCM tag]
```
- `crypto.EncryptFile` / `crypto.DecryptFile` handle the framing.
- `KeyWrapper` is an interface; `MasterKeyWrapper` is the only implementation.
  `NewMasterKeyWrapper(masterKey)` does `sha256.Sum256([]byte(masterKey))`.

## Dedup interaction (v2 / S7)
- One content key per **blob** (per unique `sha256(raw)`), not per logical
  message. A blob shared across folders is encrypted once; `messages` rows
  reference it. Refcount governs deletion.

## Search interaction (v2 / S10)
- Headers (to/from/subject/date/message-id) are indexed into Meilisearch in
  plaintext (accepted on LAN). **Message bodies are NEVER indexed** — they stay
  in the encrypted `.eml.enc` blobs only.

## Stored credentials
IMAP passwords are AES-256-GCM encrypted with the same master-key-derived key,
stored as `nonce + ciphertext + tag` (hex) in `accounts.encrypted_password`.
Decrypted only in-memory at connection time.

## Operational
- **Lose `KATCHUP_MASTER_KEY` = all `.eml.enc` and stored passwords are
  unrecoverable.** Back it up out of band.
- Plaintext never leaves the process; content keys are ephemeral per file.

## Vestigial schema
The `account_encryption` table (yubikey_slot_id, slot_fingerprint,
encrypted_content_key_prefix) is a leftover from the PIV design. It is not on
the decrypt path for format 0x02. Leave it in place (harmless) but do not build
new features on it.
