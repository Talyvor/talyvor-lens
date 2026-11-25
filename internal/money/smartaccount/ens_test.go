package smartaccount

import (
	"encoding/hex"
	"testing"
)

// B48.49: EIP-137's own namehash vectors, and names that could look like another refused.
func TestENSNamehash(t *testing.T) {
	for name, want := range map[string]string{
		"eth":     "93cdeb708b7545dc668eb9280176169d1c33cfd8ed6f04690a0bcc88a93fc4ae",
		"foo.eth": "de9b09fd7c5f901e23a3f19fecc54828e9c848539801e86591bd9801b019f84f",
	} {
		if h := ENSNamehash(name); hex.EncodeToString(h[:]) != want {
			t.Errorf("namehash(%s) = %x, want %s", name, h, want)
		}
	}
	if n, err := ENSName(" Shop.ETH "); err != nil || n != "shop.eth" {
		t.Errorf("ENSName(Shop.ETH) = %q %v", n, err)
	}
	for _, bad := range []string{"shop.com", "ѕhop.eth", "sh op.eth", ".eth", "-shop.eth"} {
		if _, err := ENSName(bad); err == nil {
			t.Errorf("ENSName(%q) passed", bad)
		}
	}
}
