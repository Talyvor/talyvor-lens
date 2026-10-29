// Package smartaccount is each agent's crypto wallet (B48.2): its own ERC-4337 smart account, a Coinbase Smart Wallet
// v1.1 (github.com/coinbase/smart-wallet at v1.1.0, audited by Cantina, Certora and Code4rena), owned by the person and
// signed for the agent by a key the wallet provider holds in its enclave.
//
// The account's owners are, in order: the person's passkey (a P-256 key, a browser passkey or the phone's device key),
// the agent's key (an address whose private key exists only inside the provider, money.Signers), and the recovery
// signer the person may add (an address). Lens works out the account's address before it exists — from the factory,
// its implementation and the owners, so it is the same on every EVM chain the factory is deployed on — and the account
// is deployed by its first user operation on each chain. Lens holds only addresses and public keys.
//
// The agent's key signs only a call out of the account (SignAsAgent): changing the account's owners, upgrading it or
// replaying an operation across chains is the person's to sign with their passkey.
package smartaccount

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// EntryPoint is ERC-4337's v0.6 EntryPoint, the one Coinbase Smart Wallet v1.1 accepts operations from, at the address
// it has on every chain.
const EntryPoint = "0x5FF137D4b0FDCD49DcA30c7CF57E578a026d2789"

// Factory is a deployment of Coinbase Smart Wallet: its factory and the implementation every account it makes runs.
type Factory struct {
	Address        string `json:"factory"`
	Implementation string `json:"implementation"`
}

// CoinbaseV11 is Coinbase Smart Wallet v1.1 where Coinbase deployed it, at the same addresses on every chain it is on
// (script/DeployFactory.s.sol at v1.1.0).
var CoinbaseV11 = Factory{Address: "0xBA5ED110eFDBa3D005bfC882d75358ACBbB85842", Implementation: "0x00000110dCdEdC9581cb5eCB8467282f2926534d"}

// The owners' places in an account.
const (
	OwnerPasskey  = 0 // the person's passkey
	OwnerAgent    = 1 // the agent's key, in the provider's enclave
	OwnerRecovery = 2 // the person's recovery signer, when they add one
)

// ErrInvalid wraps every refused input.
var ErrInvalid = errors.New("smartaccount: invalid")

// PasskeyOwner is a P-256 public key as an account owner: abi.encode(x, y).
func PasskeyOwner(pub *ecdsa.PublicKey) ([]byte, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: a passkey is a P-256 key", ErrInvalid)
	}
	b, err := pub.Bytes() // 0x04 || x || y
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return b[1:], nil
}

// AddressOwner is an address as an account owner: abi.encode(address).
func AddressOwner(addr string) ([]byte, error) {
	w, err := addressW(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return w[:], nil
}

// Account is an agent's smart account.
type Account struct {
	Factory Factory
	Owners  [][]byte // PasskeyOwner, then AddressOwner of the agent's key, then of the recovery signer if any
	Address string   // EIP-55
}

// New is the account factory f makes for passkey, the agent's key and, unless "", the recovery signer: its address is
// worked out here, before it exists anywhere.
func New(f Factory, passkey *ecdsa.PublicKey, agentKey, recovery string) (Account, error) {
	pk, err := PasskeyOwner(passkey)
	if err != nil {
		return Account{}, err
	}
	agent, err := AddressOwner(agentKey)
	if err != nil {
		return Account{}, err
	}
	owners := [][]byte{pk, agent}
	if recovery != "" {
		r, err := AddressOwner(recovery)
		if err != nil {
			return Account{}, err
		}
		owners = append(owners, r)
	}
	fw, err := addressW(f.Address)
	if err != nil {
		return Account{}, fmt.Errorf("%w: the factory: %v", ErrInvalid, err)
	}
	iw, err := addressW(f.Implementation)
	if err != nil {
		return Account{}, fmt.Errorf("%w: the implementation: %v", ErrInvalid, err)
	}
	// The factory's address for owners: LibClone.predictDeterministicAddress(initCodeHashERC1967(implementation),
	// keccak256(abi.encode(owners, nonce)), factory), with nonce 0.
	salt := keccak(tuple{ownersV(owners), uintW(0)}.enc())
	proxy := keccak(mustHex("603d3d8160223d3973"), iw[12:], mustHex("6009"),
		mustHex("5155f3363d3d373d3d363d7f360894a13ba1a3210667c828492db98dca3e2076"),
		mustHex("cc3735a920a3ca505d382bbc545af43d6000803e6038573d6000fd5b3d6000f3"))
	addr := keccak([]byte{0xff}, fw[12:], salt[:], proxy[:])
	return Account{Factory: f, Owners: owners, Address: checksum(addr[12:])}, nil
}

func ownersV(owners [][]byte) array {
	a := make(array, len(owners))
	for i, o := range owners {
		a[i] = bytesV(o)
	}
	return a
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// InitCode is what deploys the account with its first user operation: the factory, then createAccount(owners, 0).
func (a Account) InitCode() []byte {
	fw, _ := addressW(a.Factory.Address)
	return append(fw[12:], call("createAccount(bytes[],uint256)", ownersV(a.Owners), uintW(0))...)
}

// Execute is the account's call of target with value wei and data.
func Execute(target string, value *big.Int, data []byte) ([]byte, error) {
	t, err := addressW(target)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if value == nil {
		value = new(big.Int)
	}
	if value.Sign() < 0 {
		return nil, fmt.Errorf("%w: a value is not negative", ErrInvalid)
	}
	return call("execute(address,uint256,bytes)", t, bigW(value), bytesV(data)), nil
}

// KeySignature is an address owner's 65-byte r‖s‖v signature, as the account reads it: a SignatureWrapper naming the
// owner's index.
func KeySignature(index uint64, sig []byte) ([]byte, error) {
	if len(sig) != 65 {
		return nil, fmt.Errorf("%w: a key's signature is 65 bytes, not %d", ErrInvalid, len(sig))
	}
	return tuple{tuple{uintW(index), bytesV(sig)}}.enc(), nil
}

// WebAuthn is a passkey's assertion over a challenge, as the browser hands it over.
type WebAuthn struct {
	AuthenticatorData []byte
	ClientDataJSON    string
	Signature         []byte // ASN.1 DER ECDSA, as the authenticator gives it
}

// p256HalfN is half the order of P-256: the account accepts only the low s of a signature.
var p256HalfN = new(big.Int).Rsh(elliptic.P256().Params().N, 1)

// PasskeySignature is a passkey's assertion as the account reads it: a SignatureWrapper naming the owner's index, over
// abi.encode(WebAuthnAuth(authenticatorData, clientDataJSON, challengeIndex, typeIndex, r, s)).
func PasskeySignature(index uint64, a WebAuthn) ([]byte, error) {
	var rs struct{ R, S *big.Int }
	if rest, err := asn1.Unmarshal(a.Signature, &rs); err != nil || len(rest) != 0 || rs.R == nil || rs.S == nil {
		return nil, fmt.Errorf("%w: a passkey's signature is DER", ErrInvalid)
	}
	if rs.S.Cmp(p256HalfN) > 0 { // s and n-s are the same signature
		rs.S = new(big.Int).Sub(elliptic.P256().Params().N, rs.S)
	}
	challenge := strings.Index(a.ClientDataJSON, `"challenge":"`)
	typ := strings.Index(a.ClientDataJSON, `"type":"webauthn.get"`)
	if challenge < 0 || typ < 0 || len(a.AuthenticatorData) < 37 {
		return nil, fmt.Errorf("%w: not a WebAuthn assertion", ErrInvalid)
	}
	auth := tuple{bytesV(a.AuthenticatorData), bytesV(a.ClientDataJSON), uintW(uint64(challenge)), uintW(uint64(typ)), bigW(rs.R), bigW(rs.S)}
	return tuple{tuple{uintW(index), bytesV(tuple{auth}.enc())}}.enc(), nil
}
