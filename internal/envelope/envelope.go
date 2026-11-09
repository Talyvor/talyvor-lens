// Package envelope is Lens's envelope encryption for provider secrets — the customer-supplied
// upstream credentials BYOK needs Lens to take custody of (W6.10).
//
// WHY ENVELOPE AND NOT A SINGLE STATIC KEY, MEASURED RATHER THAN PREFERRED. W6.10 says to read how
// talyvor-track does it and mirror it "unless there is a measured reason not to". Track's
// internal/integrations/crypto.go is a SINGLE static AES-256-GCM key supplied whole via
// TRACK_INTEGRATION_ENCRYPTION_KEY. Measured in talyvor-track at 2026-08-26: its integrations
// package contains ZERO rotation code (`git grep -licE 'rotat' -- internal/integrations` = 0
// production files, positive control `encrypt` = 4 files). That is the measured reason: under a
// single key, rotating means re-encrypting every stored secret, so in practice nobody rotates. A
// credential store that cannot rotate its own key is the same product gap W1.9.1 exists for, one
// layer down. Envelope splits it: each secret gets its own random data key (DEK), the DEK is
// wrapped by the operator's key-encryption key (KEK), and rotation rewraps DEKs while the
// ciphertext of the secret itself is never touched (see Rewrap, and TestP5).
//
// WHAT IS MIRRORED FROM TRACK, VERBATIM, BECAUSE IT IS THE BETTER HALF OF THE POINTER: the posture.
// Unset ⇒ the capability is disabled and there is NO plaintext fallback (Lens still boots). Set ⇒
// validated to exactly 32 decoded bytes AT BOOT, so a wrong length is fail-loud at startup rather
// than a broken-crypto surprise at first use. See internal/config.
//
// WRITE-ONLY, ALWAYS. Nothing in this package hands a plaintext back to its caller. The only way to
// see a secret is Use, which lends it to a callback and wipes the buffer before returning — so a
// caller cannot keep it by accident, and no handler can return one. TestP6 enforces that over the
// package's exported surface, so a future edit that adds a getter goes red rather than unnoticed.
//
// WHAT HAPPENS IF THE KEK IS LOST: every secret wrapped under it is unrecoverable. There is no
// escrow, no recovery code and no Talyvor-side copy — that is the property that lets Talyvor say a
// compromise of the database alone does not hand over customer provider accounts. The operator-
// visible symptom is deliberately specific: Use returns ErrUnknownKey NAMING the key id it needed
// (TestP8), so "you dropped a KEK rows still reference" never looks like data corruption. Recovery
// is re-entry of the provider credential by the customer, not decryption. docs/provider-secret-envelope.md
// carries the operator runbook.
package envelope

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// KEKLen is the key-encryption-key length in bytes. AES-256 and nothing else: a shorter key is a
// silently weaker store, so it is refused rather than padded.
const KEKLen = 32

// dekLen is the per-secret data key length. Same cipher, so the same length.
const dekLen = 32

// keyIDLen is how many bytes of SHA-256(KEK) name a key. Four bytes is enough to tell a handful of
// operator keys apart and far too little to say anything about the key itself.
const keyIDLen = 4

// dekWrapAAD domain-separates the DEK-wrapping layer from the data layer, so a wrapped DEK can
// never be opened as if it were a secret's ciphertext (or the reverse) under the same KEK.
//
// It is deliberately CONSTANT rather than the caller's aad: binding the wrap to a row's identity
// would mean a rotation job needs every row's aad to rewrap it, and a rotation nobody can run in
// one pass is the single-key problem again. Moving a whole row between owners is still caught —
// the DATA layer carries the caller's aad (see Seal/Use, TestP4).
var dekWrapAAD = []byte("lens/envelope/v1/dek-wrap")

// ErrUnknownKey is returned when a Sealed names a key id the ring does not hold. It is a distinct
// error precisely so an operator can tell key loss from ciphertext corruption; the message names
// the missing id.
var ErrUnknownKey = errors.New("envelope: no key in the ring matches this record's key id")

// Sealed is one encrypted provider secret. Every field is ciphertext, a nonce or a key id — there
// is no field here that is worth reading, which is why the whole struct can be persisted, logged
// or returned by an API without leaking anything.
type Sealed struct {
	// KeyID names the KEK whose wrap must be undone first. Rotation changes this and nothing else
	// about the record's meaning.
	KeyID string
	// WrappedDEK is the per-secret data key, encrypted under the KEK named by KeyID.
	WrappedDEK []byte
	// DEKNonce is the AES-GCM nonce for the wrap.
	DEKNonce []byte
	// Ciphertext is the secret itself, encrypted under the DEK. Rewrap never touches this.
	Ciphertext []byte
	// CTNonce is the AES-GCM nonce for Ciphertext. Rewrap never touches this either.
	CTNonce []byte
}

type kekEntry struct {
	id   string
	aead cipher.AEAD
}

// Keyring is the operator's ordered set of KEKs. The FIRST is primary — every new Seal and every
// Rewrap lands on it. The rest are decrypt-only, held so that rows sealed before a rotation still
// open while a rewrap job works through them.
type Keyring struct {
	entries []kekEntry
	byID    map[string]cipher.AEAD
}

// NewKeyring builds a ring from raw 32-byte keys, first = primary.
//
// Refuses: no keys at all (a ring that seals under nothing), any key that is not exactly KEKLen
// bytes, and a duplicate — a ring holding the same KEK twice looks rotated and is not.
func NewKeyring(keys ...[]byte) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, errors.New("envelope: a keyring needs at least one key")
	}
	r := &Keyring{byID: make(map[string]cipher.AEAD, len(keys))}
	for i, k := range keys {
		if len(k) != KEKLen {
			return nil, fmt.Errorf("envelope: key %d must be exactly %d bytes for AES-256, got %d", i, KEKLen, len(k))
		}
		id := keyID(k)
		if _, dup := r.byID[id]; dup {
			return nil, fmt.Errorf("envelope: key %d (id %s) is already in the ring. A duplicate KEK is not a rotation", i, id)
		}
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, fmt.Errorf("envelope: key %d: %w", i, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("envelope: key %d: %w", i, err)
		}
		r.entries = append(r.entries, kekEntry{id: id, aead: aead})
		r.byID[id] = aead
	}
	return r, nil
}

// ParseKeyring reads the operator wire form: one or more base64 keys separated by commas, primary
// first. This is what LENS_PROVIDER_SECRET_KEK holds; `openssl rand -base64 32` produces one.
//
// To rotate, PREPEND the new key and keep the old one listed until a rewrap pass has moved every
// row (KeyIDs reports what the ring can open; a row's KeyID says what it needs).
func ParseKeyring(spec string) (*Keyring, error) {
	parts := strings.Split(spec, ",")
	keys := make([][]byte, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("envelope: key %d in the list is empty", i)
		}
		k, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			return nil, fmt.Errorf("envelope: key %d is not valid base64: %w", i, err)
		}
		keys = append(keys, k)
	}
	return NewKeyring(keys...)
}

// PrimaryKeyID is the id of the key every new Seal lands on.
func (r *Keyring) PrimaryKeyID() string { return r.entries[0].id }

// KeyIDs lists every key id the ring can open, primary first. An operator compares this against the
// distinct KeyIDs stored in the secrets table to see whether a rotation has finished.
func (r *Keyring) KeyIDs() []string {
	out := make([]string, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.id)
	}
	return out
}

// Seal encrypts plaintext under a FRESH random DEK and wraps that DEK under the primary KEK.
//
// aad binds the record to its context and is NOT stored: the caller must supply the identical bytes
// to Use. Pass something that identifies the row's owner and slot (a workspace id and provider, for
// example) — then copying a row into another workspace's slot fails to open instead of silently
// billing the first workspace's provider account (TestP4).
func (r *Keyring) Seal(plaintext, aad []byte) (Sealed, error) {
	dek := make([]byte, dekLen)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return Sealed{}, fmt.Errorf("envelope: generating a data key: %w", err)
	}
	defer wipe(dek)

	dataAEAD, err := aeadFor(dek)
	if err != nil {
		return Sealed{}, err
	}
	ctNonce, err := freshNonce(dataAEAD)
	if err != nil {
		return Sealed{}, err
	}
	ciphertext := dataAEAD.Seal(nil, ctNonce, plaintext, aad)

	primary := r.entries[0]
	dekNonce, err := freshNonce(primary.aead)
	if err != nil {
		return Sealed{}, err
	}
	wrapped := primary.aead.Seal(nil, dekNonce, dek, dekWrapAAD)

	return Sealed{
		KeyID:      primary.id,
		WrappedDEK: wrapped,
		DEKNonce:   dekNonce,
		Ciphertext: ciphertext,
		CTNonce:    ctNonce,
	}, nil
}

// Use lends the plaintext to fn and wipes it before returning. It is the ONLY way a secret leaves
// this package, and it does not return one — so "verify by USE, never by read-back" is what the
// type system offers a caller, not a convention they have to remember.
//
// fn must not retain the slice: the bytes it points at are zeroed the moment fn returns (TestP7).
// On any failure — unknown key id, tampered wrap, tampered ciphertext, wrong aad — fn is never
// called at all.
func (r *Keyring) Use(s Sealed, aad []byte, fn func(plaintext []byte) error) error {
	dek, err := r.unwrap(s)
	if err != nil {
		return err
	}
	defer wipe(dek)

	dataAEAD, err := aeadFor(dek)
	if err != nil {
		return err
	}
	if len(s.CTNonce) != dataAEAD.NonceSize() {
		return fmt.Errorf("envelope: data nonce is %d bytes, want %d", len(s.CTNonce), dataAEAD.NonceSize())
	}
	plaintext, err := dataAEAD.Open(nil, s.CTNonce, s.Ciphertext, aad)
	if err != nil {
		// Deliberately not wrapped with the aad or any part of the record: an attacker probing this
		// path learns only that it failed.
		return fmt.Errorf("envelope: the secret failed authentication (tampered ciphertext, or the wrong aad): %w", err)
	}
	defer wipe(plaintext)
	return fn(plaintext)
}

// Rewrap moves a record onto the primary KEK WITHOUT decrypting or re-encrypting the secret: it
// unwraps the DEK under the key the record names, wraps it under the primary, and returns a record
// whose Ciphertext and CTNonce are the same bytes it was given (TestP5).
//
// That is the whole operational difference from a single-key store. A rotation job needs the
// keyring and nothing else — no aad, no plaintext, no knowledge of what any row means — so it can
// walk the table in one pass. Rewrap deliberately does NOT verify the secret still opens: doing so
// would need each row's aad, which is what makes a one-pass rotation impossible.
func (r *Keyring) Rewrap(s Sealed) (Sealed, error) {
	dek, err := r.unwrap(s)
	if err != nil {
		return Sealed{}, err
	}
	defer wipe(dek)

	primary := r.entries[0]
	dekNonce, err := freshNonce(primary.aead)
	if err != nil {
		return Sealed{}, err
	}
	out := s
	out.KeyID = primary.id
	out.WrappedDEK = primary.aead.Seal(nil, dekNonce, dek, dekWrapAAD)
	out.DEKNonce = dekNonce
	return out, nil
}

// unwrap recovers the DEK, or says exactly why it could not.
func (r *Keyring) unwrap(s Sealed) ([]byte, error) {
	aead, ok := r.byID[s.KeyID]
	if !ok {
		return nil, fmt.Errorf("%w: key id %q (the ring holds %v)", ErrUnknownKey, s.KeyID, r.KeyIDs())
	}
	if len(s.DEKNonce) != aead.NonceSize() {
		return nil, fmt.Errorf("envelope: wrap nonce is %d bytes, want %d", len(s.DEKNonce), aead.NonceSize())
	}
	dek, err := aead.Open(nil, s.DEKNonce, s.WrappedDEK, dekWrapAAD)
	if err != nil {
		return nil, fmt.Errorf("envelope: the wrapped data key failed authentication under key %q: %w", s.KeyID, err)
	}
	if len(dek) != dekLen {
		wipe(dek)
		return nil, fmt.Errorf("envelope: unwrapped data key is %d bytes, want %d", len(dek), dekLen)
	}
	return dek, nil
}

func aeadFor(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("envelope: building the cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("envelope: building the AEAD: %w", err)
	}
	return aead, nil
}

func freshNonce(a cipher.AEAD) ([]byte, error) {
	n := make([]byte, a.NonceSize())
	if _, err := io.ReadFull(rand.Reader, n); err != nil {
		return nil, fmt.Errorf("envelope: generating a nonce: %w", err)
	}
	return n, nil
}

// keyID names a KEK by a prefix of its SHA-256. Four bytes of a hash of a 32-byte random key says
// nothing usable about the key; it exists so a record can state which KEK it needs and an operator
// can see which rows a rotation has not reached yet.
func keyID(k []byte) string {
	sum := sha256.Sum256(k)
	return hex.EncodeToString(sum[:keyIDLen])
}

// wipe zeroes a buffer. subtle.ConstantTimeCopy is used rather than a range-assign so the compiler
// cannot elide the write as dead (the buffer is genuinely unread afterwards, which is exactly the
// condition under which a plain loop may be optimised away).
func wipe(b []byte) {
	if len(b) == 0 {
		return
	}
	subtle.ConstantTimeCopy(1, b, make([]byte, len(b)))
}
