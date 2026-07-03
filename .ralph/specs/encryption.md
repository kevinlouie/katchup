# Encryption Specification

## Overview
Katchup encrypts `.eml` files at rest using a hybrid encryption scheme backed by YubiKey PIV. The design ensures that plaintext email content and symmetric keys never leave the container, and the private key never leaves the YubiKey device.

## Architecture

### Key Hierarchy
```
YubiKey PIV Slot (9a, 9b, 9c, or 9d)
  └── RSA-2048 key pair (generated on YubiKey, private key never exportable)
        └── Public key encrypts content keys
              └── AES-256-GCM content key encrypts .eml files
```

### Flow: Encrypt (during sync)
1. Generate random 256-bit content key (CK)
2. Encrypt file with CK using AES-256-GCM → `ciphertext` + `nonce` + `tag`
3. Encrypt CK with YubiKey PIV public key using RSA-OAEP (SHA-256) → `encrypted_ck`
4. Store `encrypted_ck` prefix (first 16 hex chars) in `account_encryption.encrypted_content_key_prefix`
5. Write file: `nonce` (12 bytes) + `encrypted_ck` (256 bytes) + `tag` (16 bytes) + `ciphertext`

### Flow: Decrypt (during download)
1. Read file header: nonce, encrypted_ck_prefix, tag, ciphertext
2. Look up `account_encryption` row matching account_id + encrypted_ck_prefix
3. Send `encrypted_ck` to YubiKey PIV slot → YubiKey returns decrypted CK in-memory
4. Use CK to AES-256-GCM decrypt ciphertext
5. Serve decrypted .eml to user (CK discarded immediately)
6. If no matching `account_encryption` row → try all known slots on the connected YubiKey

## YubiKey PIV Integration

### Library
- `github.com/yubikit/yubikit-go/yubikit/piv`
- Communicates via USB (PC/SC)

### Slots
| Slot ID | Purpose |
|---------|---------|
| 9a | Primary encryption (default) |
| 9b | Secondary / backup YubiKey |
| 9c | Tertiary / backup YubiKey |
| 9d | Tertiary / backup YubiKey |

Multiple slots enable multiple YubiKeys — each email stores the prefix of the encrypted content key, and the lookup finds which slot to use for decryption.

### PIV Operations
```go
// Connect to YubiKey
card, err := piv.Connect()

// Get public key from slot (for encryption)
pubKey, err := card.PublicKey(piv.SlotAuthentication, piv.CapabilityRSA, piv.RSA2048)

// Decrypt content key (signature operation — YubiKey never outputs raw private key)
decryptedCK, err := card.Sign(piv.SlotAuthentication, encryptedCK)
```

### Key Injection (One-Time Setup)
```bash
# Generate RSA-2048 key in slot 9a
ykman piv generate-key -a rsa2048 9a

# Verify
ykman piv information
```

## File Format

Each encrypted `.eml.enc` file:
```
+------------------+------------------+--------+------------------+
| nonce (12 bytes) | encrypted_ck (256 bytes) | tag (16 bytes) | ciphertext (variable) |
+------------------+------------------+--------+------------------+
```

- `nonce` — AES-GCM nonce (12 bytes, random per file)
- `encrypted_ck` — RSA-OAEP encrypted content key (256 bytes for RSA-2048)
- `tag` — AES-GCM authentication tag (16 bytes, appended after ciphertext)
- `ciphertext` — encrypted .eml content (variable length)

## Encryption-at-Rest for Stored Credentials

IMAP passwords are stored encrypted in the `accounts` table. The encryption key is derived from:
1. `KATCHUP_MASTER_KEY` environment variable (required), OR
2. A host-derived key (fallback, less secure)

Password encryption: AES-256-GCM with a random nonce per password. Stored as: `nonce` + `ciphertext` + `tag` (hex-encoded).

## Security Notes
- Plaintext never leaves the container
- Private keys never leave the YubiKey
- Content keys are ephemeral — generated per-file, discarded after use
- Multiple YubiKeys supported via slot-based key lookup
- Passwords encrypted at rest with `KATCHUP_MASTER_KEY`
