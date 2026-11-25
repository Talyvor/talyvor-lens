package smartaccount

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ens.go — B48.49: an ENS name (docs.ens.domains, "Resolution") read on the chain a send goes out on, through its
// registry: the name's resolver, then that resolver's addr. Only names ending .eth, written in lowercase ASCII letters,
// digits and hyphens, are read: anything else could look like another name, so it is refused rather than normalised.
// A resolver that answers by offchain lookup (ERC-3668) is not followed; its name resolves to nothing here.

var (
	// ErrENSName: not a name this reads.
	ErrENSName = errors.New("smartaccount: not an ENS name")
	// ErrENSNotFound: the name has no resolver, or its resolver no address, on this chain.
	ErrENSNotFound = errors.New("smartaccount: the ENS name has no address set")
	// ErrENSOff: this chain has no ENS registry set.
	ErrENSOff = errors.New("smartaccount: ENS names are not read on this network")
)

// IsENSName is whether to is written as an ENS name: one ending .eth.
func IsENSName(to string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(to)), ".eth")
}

// ENSName is name as it is resolved and shown: lowercase, each label of letters, digits and hyphens.
func ENSName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !strings.HasSuffix(name, ".eth") || len(name) > 255 {
		return "", fmt.Errorf("%w: %q", ErrENSName, name)
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", fmt.Errorf("%w: %q", ErrENSName, name)
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", fmt.Errorf("%w: %q has a character other than a-z, 0-9 and -", ErrENSName, name)
			}
		}
	}
	return name, nil
}

// ENSNamehash is EIP-137's namehash of a name ENSName gave.
func ENSNamehash(name string) [32]byte {
	var node [32]byte
	labels := strings.Split(name, ".")
	for i := len(labels) - 1; i >= 0; i-- {
		label := keccak([]byte(labels[i]))
		node = keccak(node[:], label[:])
	}
	return node
}

// ResolveENS is the address name is set to on c, read from c's ENSRegistry, checksummed.
func (c Chain) ResolveENS(ctx context.Context, name string) (string, error) {
	if c.ENSRegistry == "" {
		return "", ErrENSOff
	}
	name, err := ENSName(name)
	if err != nil {
		return "", err
	}
	node := word(ENSNamehash(name))
	resolver, err := c.addressCall(ctx, c.ENSRegistry, call("resolver(bytes32)", node))
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, name)
	}
	addr, err := c.addressCall(ctx, resolver, call("addr(bytes32)", node))
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, name)
	}
	return addr, nil
}

// addressCall is the address a view call to to answers; ErrENSNotFound for the zero address or no answer.
func (c Chain) addressCall(ctx context.Context, to string, data []byte) (string, error) {
	b, err := c.ethCallData(ctx, to, data)
	if err != nil {
		return "", err
	}
	if len(b) < 32 {
		return "", ErrENSNotFound
	}
	a := b[12:32]
	for _, x := range a {
		if x != 0 {
			return checksum(a), nil
		}
	}
	return "", ErrENSNotFound
}
