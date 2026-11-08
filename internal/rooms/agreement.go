package rooms

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// agreement.go — B41.150: THE CONTRIBUTOR AGREEMENT, E-SIGNED BEFORE POINTS COUNT (migration 0353).
//
// Each contributor's workspace signs the room's contributor agreement before its points count: its work in the room is
// assigned to the company formed to own it, with a licence back if no company is ever formed (CDPA s90(3) needs it in
// writing). Until the lawyer's text replaces it, the agreement is a draft marked "Test rooms only"; capability
// room_equity keeps every route of it to synthetic rooms until an operator records its clearance. A signature keeps the
// version and the hash of the exact text signed, the name typed and the person who signed, append-only. A points row
// for a workspace that has not signed the current version is never written: work accepted before it signs earns
// nothing, and its next accepted work after signing earns as usual.

// AgreementVersion names the agreement's current text; a new text gets a new version, and each contributor signs again.
const AgreementVersion = "draft-1"

// AgreementMarked is what the draft says on its face until the lawyer's text replaces it.
const AgreementMarked = "Test rooms only"

// agreementText is the draft. shortcut: written to the item's brief, not by a lawyer; replace it with the lawyer's
// text under a new AgreementVersion, and set Draft false, before room_equity is cleared.
const agreementText = `Contributor agreement

1. Your contributions are the work you add to this room and the changes you make to it.

2. You assign to the company formed to own this room's work the copyright and every other intellectual property right in your contributions. The assignment covers each contribution from the moment you make it, including rights that come into being later.

3. Once the company owns your contributions, it licenses them back to you to use for any purpose, free of charge and for ever.

4. If no company is ever formed, you keep your rights in your contributions, and you grant everyone who signed this agreement in this room a free, non-exclusive, worldwide and permanent licence to use, change and share them as part of the room's work.

5. Your points in this room count only once you have signed this agreement.`

// maxSignedName bounds the name a contributor types to sign.
const maxSignedName = 200

// Signature is a workspace's e-signature of the agreement.
type Signature struct {
	WorkspaceID string    `json:"workspace_id"`
	UserID      string    `json:"user_id"` // the person who signed for it
	SignedName  string    `json:"signed_name"`
	Version     string    `json:"version"`
	TextSHA256  string    `json:"text_sha256"`
	SignedAt    time.Time `json:"signed_at"`
}

// Agreement is the room's contributor agreement as a member reads it, with the member's signature of it.
type Agreement struct {
	RoomID     string     `json:"room_id"`
	Version    string     `json:"version"`
	Draft      bool       `json:"draft"`
	Marked     string     `json:"marked,omitempty"`
	Text       string     `json:"text"`
	TextSHA256 string     `json:"text_sha256"`
	Signature  *Signature `json:"signature"` // nil: not signed yet, and its points do not count
}

func currentAgreement(roomID string) Agreement {
	sum := sha256.Sum256([]byte(agreementText))
	return Agreement{RoomID: roomID, Version: AgreementVersion, Draft: true, Marked: AgreementMarked, Text: agreementText,
		TextSHA256: hex.EncodeToString(sum[:])}
}

// readSignature is ws's signature of the current agreement in roomID; nil when it has not signed it.
func readSignature(ctx context.Context, tx pgx.Tx, roomID, ws string) (*Signature, error) {
	s := Signature{WorkspaceID: ws}
	err := tx.QueryRow(ctx, `SELECT user_id, signed_name, agreement_version, text_sha256, signed_at FROM room_agreement_signatures
		WHERE room_id = $1 AND workspace_id = $2 AND agreement_version = $3`, roomID, ws, AgreementVersion).
		Scan(&s.UserID, &s.SignedName, &s.Version, &s.TextSHA256, &s.SignedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &s, err
}

// signedAgreement answers whether ws signed the current agreement in roomID.
func signedAgreement(ctx context.Context, tx pgx.Tx, roomID, ws string) (bool, error) {
	s, err := readSignature(ctx, tx, roomID, ws)
	return s != nil, err
}

// Agreement is roomID's contributor agreement and ws's signature of it, for a member that accepted the current terms.
func (s *Store) Agreement(ctx context.Context, ws, roomID string) (a Agreement, err error) {
	a = currentAgreement(roomID)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := ownershipMember(ctx, tx, roomID, ws); err != nil {
			return err
		}
		a.Signature, err = readSignature(ctx, tx, roomID, ws)
		return err
	})
	return a, err
}

// SignAgreement records user's e-signature, as signedName, of the agreement at version for ws, a member that accepted
// the room's current terms. version must be the current one. Signing again answers the signature already kept.
func (s *Store) SignAgreement(ctx context.Context, ws, user, roomID, version, signedName string) (a Agreement, created bool, err error) {
	a = currentAgreement(roomID)
	signedName = strings.TrimSpace(signedName)
	if signedName == "" || len([]rune(signedName)) > maxSignedName {
		return a, false, invalid("signed_name is the signer's full name, 1–%d characters", maxSignedName)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := ownershipMember(ctx, tx, roomID, ws); err != nil {
			return err
		}
		switch {
		case user == "":
			return forbidden("the contributor agreement is signed by a person signed in for the workspace, not by a key")
		case version != AgreementVersion:
			return fmt.Errorf("%w: the contributor agreement is at version %s: read it and sign that version", ErrConflict, AgreementVersion)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO room_agreement_signatures (room_id, workspace_id, agreement_version, text_sha256, signed_name, user_id)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, roomID, ws, AgreementVersion, a.TextSHA256, signedName, user)
		if err != nil {
			return err
		}
		created = tag.RowsAffected() == 1
		a.Signature, err = readSignature(ctx, tx, roomID, ws)
		return err
	})
	return a, created, err
}
