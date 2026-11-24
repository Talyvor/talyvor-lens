// Package role names the roles a workspace's owner may give other people (B28.342). It imports nothing of Lens's own,
// so auth, which enforces a role on every request, can name them without importing workspace.
package role

import "errors"

const (
	Viewer   = "viewer"   // reads everything; moves, approves and changes nothing
	Finance  = "finance"  // reads, and funds, pays, sends, converts and takes money back
	Approver = "approver" // reads, and approves or declines what waits for an approval
	Admin    = "admin"    // everything but the workspace's keys, passkeys and roles, which stay the owner's
)

// All are the roles a person may be given, least power first.
var All = []string{Viewer, Finance, Approver, Admin}

// ErrUnknown refuses a role that is not one of All.
var ErrUnknown = errors.New("role must be viewer, finance, approver or admin")

// Check reports ErrUnknown unless r is one of All.
func Check(r string) error {
	for _, a := range All {
		if r == a {
			return nil
		}
	}
	return ErrUnknown
}
