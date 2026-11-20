package workspace

import "strings"

// B61.114: the identity the BFF presents may hold a zero byte (a Google one is issuer + "\x00" + sub), and
// PostgreSQL's text refuses one. people.identity holds StoredIdentity of it — "%" as "%25" and the zero byte as "%00" —
// and every read answers IdentityFromStored of the column. An identity with neither character is stored as it is.

var (
	identityToStored   = strings.NewReplacer("%", "%25", "\x00", "%00")
	identityFromStored = strings.NewReplacer("%25", "%", "%00", "\x00")
)

// StoredIdentity is the form of identity that people.identity holds.
func StoredIdentity(identity string) string { return identityToStored.Replace(identity) }

// IdentityFromStored is the identity a people.identity value holds, byte for byte as it was given.
func IdentityFromStored(stored string) string { return identityFromStored.Replace(stored) }
