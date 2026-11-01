package smartaccount

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"golang.org/x/crypto/sha3"
)

// abi.go — the few Solidity ABI encodings the account needs (docs.soliditylang.org, "Contract ABI Specification"):
// 32-byte words, dynamic bytes, arrays and tuples, head then tail.

// value is one ABI-encoded argument.
type value interface {
	dynamic() bool
	enc() []byte
}

// word is a static 32-byte value: a uint256, an address or a bytes32.
type word [32]byte

func (word) dynamic() bool { return false }
func (w word) enc() []byte { return w[:] }

// bytesV is a dynamic bytes or string.
type bytesV []byte

func (bytesV) dynamic() bool { return true }
func (b bytesV) enc() []byte {
	n := uint64(len(b))
	out := append(uintW(n).enc(), b...)
	return append(out, make([]byte, (32-len(b)%32)%32)...)
}

// tuple is a struct, or the arguments of a call: dynamic when any of its fields is.
type tuple []value

func (t tuple) dynamic() bool {
	for _, v := range t {
		if v.dynamic() {
			return true
		}
	}
	return false
}

func (t tuple) enc() []byte {
	var head, tail []byte
	for _, v := range t {
		if v.dynamic() {
			head = append(head, uintW(uint64(32*len(t)+len(tail))).enc()...)
			tail = append(tail, v.enc()...)
			continue
		}
		head = append(head, v.enc()...)
	}
	return append(head, tail...)
}

// array is a dynamic-length array T[].
type array []value

func (array) dynamic() bool { return true }
func (a array) enc() []byte { return append(uintW(uint64(len(a))).enc(), tuple(a).enc()...) }

func uintW(n uint64) word { return bigW(new(big.Int).SetUint64(n)) }

// bigW is n, which is not negative and fits 256 bits, as a word.
func bigW(n *big.Int) word {
	var w word
	if n != nil {
		n.FillBytes(w[:])
	}
	return w
}

var addressRE = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// addressW is a 0x address as a word.
func addressW(addr string) (word, error) {
	var w word
	if !addressRE.MatchString(addr) {
		return w, fmt.Errorf("smartaccount: %q is not an address", addr)
	}
	b, _ := hex.DecodeString(addr[2:])
	copy(w[12:], b)
	return w, nil
}

func keccak(parts ...[]byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	for _, p := range parts {
		h.Write(p)
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// selector is a function's 4-byte selector, from its canonical signature.
func selector(signature string) []byte {
	h := keccak([]byte(signature))
	return h[:4]
}

// call is the calldata of a call to the function with signature, given its arguments.
func call(signature string, args ...value) []byte {
	return append(selector(signature), tuple(args).enc()...)
}

// checksum is addr in EIP-55 mixed case, the form an address is shown and stored in.
func checksum(addr []byte) string {
	lower := hex.EncodeToString(addr)
	h := keccak([]byte(lower))
	var b strings.Builder
	b.WriteString("0x")
	for i, c := range lower {
		if c >= 'a' && h[i/2]>>(4*(1-uint(i%2)))&0xf >= 8 {
			c -= 'a' - 'A'
		}
		b.WriteRune(c)
	}
	return b.String()
}

// Checksum is the 0x address addr in EIP-55 mixed case; "" when addr is not an address.
func Checksum(addr string) string {
	if !addressRE.MatchString(addr) {
		return ""
	}
	b, _ := hex.DecodeString(addr[2:])
	return checksum(b)
}
