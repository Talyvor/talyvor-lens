package smartaccount

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// UserOp is an ERC-4337 v0.6 user operation.
type UserOp struct {
	Sender               string
	Nonce                *big.Int
	InitCode             []byte
	CallData             []byte
	CallGasLimit         *big.Int
	VerificationGasLimit *big.Int
	PreVerificationGas   *big.Int
	MaxFeePerGas         *big.Int
	MaxPriorityFeePerGas *big.Int
	PaymasterAndData     []byte
	Signature            []byte
}

// Hash is what op's signer signs on chainID: EntryPoint v0.6's getUserOpHash.
func (op UserOp) Hash(chainID int64) ([32]byte, error) {
	sender, err := addressW(op.Sender)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	ep, _ := addressW(EntryPoint)
	initCode, callData, pm := keccak(op.InitCode), keccak(op.CallData), keccak(op.PaymasterAndData)
	packed := keccak(tuple{sender, bigW(op.Nonce), word(initCode), word(callData), bigW(op.CallGasLimit), bigW(op.VerificationGasLimit),
		bigW(op.PreVerificationGas), bigW(op.MaxFeePerGas), bigW(op.MaxPriorityFeePerGas), word(pm)}.enc())
	return keccak(tuple{word(packed), ep, bigW(big.NewInt(chainID))}.enc()), nil
}

// wireOp is a user operation as a bundler's JSON-RPC takes it: quantities and bytes as 0x hex.
type wireOp struct {
	Sender               string `json:"sender"`
	Nonce                string `json:"nonce"`
	InitCode             string `json:"initCode"`
	CallData             string `json:"callData"`
	CallGasLimit         string `json:"callGasLimit"`
	VerificationGasLimit string `json:"verificationGasLimit"`
	PreVerificationGas   string `json:"preVerificationGas"`
	MaxFeePerGas         string `json:"maxFeePerGas"`
	MaxPriorityFeePerGas string `json:"maxPriorityFeePerGas"`
	PaymasterAndData     string `json:"paymasterAndData"`
	Signature            string `json:"signature"`
}

func quantity(n *big.Int) string {
	if n == nil {
		return "0x0"
	}
	return "0x" + n.Text(16)
}

func hexBytes(b []byte) string { return "0x" + hex.EncodeToString(b) }

// MarshalJSON is op as a bundler takes it.
func (op UserOp) MarshalJSON() ([]byte, error) {
	return json.Marshal(wireOp{op.Sender, quantity(op.Nonce), hexBytes(op.InitCode), hexBytes(op.CallData), quantity(op.CallGasLimit),
		quantity(op.VerificationGasLimit), quantity(op.PreVerificationGas), quantity(op.MaxFeePerGas), quantity(op.MaxPriorityFeePerGas),
		hexBytes(op.PaymasterAndData), hexBytes(op.Signature)})
}

// UnmarshalJSON reads op as a bundler takes it.
func (op *UserOp) UnmarshalJSON(b []byte) error {
	var w wireOp
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	q := func(s string) (*big.Int, error) {
		n, ok := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
		if !ok || !strings.HasPrefix(s, "0x") {
			return nil, fmt.Errorf("%w: %q is not a quantity", ErrInvalid, s)
		}
		return n, nil
	}
	d := func(s string) ([]byte, error) {
		if !strings.HasPrefix(s, "0x") {
			return nil, fmt.Errorf("%w: %q is not 0x hex", ErrInvalid, s)
		}
		return hex.DecodeString(s[2:])
	}
	var errs []error
	pick := func(n *big.Int, err error) *big.Int { errs = append(errs, err); return n }
	bytesOf := func(b []byte, err error) []byte { errs = append(errs, err); return b }
	*op = UserOp{Sender: w.Sender, Nonce: pick(q(w.Nonce)), InitCode: bytesOf(d(w.InitCode)), CallData: bytesOf(d(w.CallData)),
		CallGasLimit: pick(q(w.CallGasLimit)), VerificationGasLimit: pick(q(w.VerificationGasLimit)), PreVerificationGas: pick(q(w.PreVerificationGas)),
		MaxFeePerGas: pick(q(w.MaxFeePerGas)), MaxPriorityFeePerGas: pick(q(w.MaxPriorityFeePerGas)),
		PaymasterAndData: bytesOf(d(w.PaymasterAndData)), Signature: bytesOf(d(w.Signature))}
	return errors.Join(errs...)
}

// ErrRefused: the bundler or the chain refused the operation — a signature the account does not accept among them.
var ErrRefused = errors.New("smartaccount: refused")

// Chain is one EVM chain: its id, a node, and an ERC-4337 bundler (the provider's, or a local one in tests).
type Chain struct {
	ID           int64
	RPCURL       string
	BundlerURL   string
	PaymasterURL string // B48.5: an ERC-7677 paymaster that pays the chain's network fees, such as CDP's on Base
	HTTP         *http.Client
}

func (c Chain) rpc(ctx context.Context, url, method string, params []any, out any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: the node's URL", ErrInvalid) // the URL carries the node's key: never in an error
	}
	req.Header.Set("Content-Type", "application/json")
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return fmt.Errorf("smartaccount: %s: the node did not answer", method)
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return fmt.Errorf("smartaccount: %s answered %d", method, resp.StatusCode)
	}
	if r.Error != nil {
		return fmt.Errorf("%w: %s: %d %s", ErrRefused, method, r.Error.Code, r.Error.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Result, out)
}

func (c Chain) quantity(ctx context.Context, method string, params []any) (*big.Int, error) {
	var s string
	if err := c.rpc(ctx, c.RPCURL, method, params, &s); err != nil {
		return nil, err
	}
	n, ok := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("smartaccount: %s answered %q", method, s)
	}
	return n, nil
}

// Deployed is whether addr has code on the chain: an account is deployed by its first operation there.
func (c Chain) Deployed(ctx context.Context, addr string) (bool, error) {
	var code string
	if err := c.rpc(ctx, c.RPCURL, "eth_getCode", []any{addr, "latest"}, &code); err != nil {
		return false, err
	}
	return code != "" && code != "0x", nil
}

// Nonce is the next nonce of addr's default sequence at the EntryPoint.
func (c Chain) Nonce(ctx context.Context, addr string) (*big.Int, error) {
	a, err := addressW(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	data := call("getNonce(address,uint192)", a, uintW(0))
	return c.quantity(ctx, "eth_call", []any{map[string]string{"to": EntryPoint, "data": hexBytes(data)}, "latest"})
}

// Gas an operation is given.
//
// shortcut: fixed limits, ample for a call with a deployment and a passkey checked without the P-256 precompile; the
// EntryPoint charges only the gas used, but the account, or its paymaster, must hold the limits' worth, and the fee an
// owner is quoted (MaxFee) is those limits' worth. Upgrade with eth_estimateUserOperationGas once the provider's bundler
// serves the chain.
var (
	callGas         = big.NewInt(500_000)
	verificationGas = big.NewInt(2_000_000)
	preVerification = big.NewInt(100_000)
)

// Prepare is the unsigned operation by which a calls with callData on the chain: it deploys a there first when this is
// its first operation.
func (c Chain) Prepare(ctx context.Context, a Account, callData []byte) (UserOp, error) {
	deployed, err := c.Deployed(ctx, a.Address)
	if err != nil {
		return UserOp{}, err
	}
	nonce, err := c.Nonce(ctx, a.Address)
	if err != nil {
		return UserOp{}, err
	}
	price, err := c.quantity(ctx, "eth_gasPrice", nil)
	if err != nil {
		return UserOp{}, err
	}
	op := UserOp{Sender: a.Address, Nonce: nonce, CallData: callData, CallGasLimit: callGas, VerificationGasLimit: verificationGas,
		PreVerificationGas: preVerification, MaxFeePerGas: new(big.Int).Mul(price, big.NewInt(2)), MaxPriorityFeePerGas: price}
	if !deployed {
		op.InitCode = a.InitCode()
	}
	return op, nil
}

// Signer signs a hash with a key held inside a provider's enclave (money.Signers): 65 bytes, r‖s‖v.
type Signer interface {
	SignHash(ctx context.Context, signer string, hash [32]byte) ([]byte, error)
}

var executeSelector = selector("execute(address,uint256,bytes)")

// SignAsAgent has the agent's key agentKey, in the provider's enclave, sign op for a on the chain. It signs only a
// call out of the account: an operation that would deploy another account, call the account itself (its owners, an
// upgrade) or be replayed on other chains is refused before anything is signed.
func (c Chain) SignAsAgent(ctx context.Context, s Signer, a Account, agentKey string, op *UserOp) error {
	if len(a.Owners) <= OwnerAgent {
		return fmt.Errorf("%w: the account has no agent key", ErrInvalid)
	}
	if owner, err := AddressOwner(agentKey); err != nil || !bytes.Equal(owner, a.Owners[OwnerAgent]) {
		return fmt.Errorf("%w: %s is not the agent's key of %s", ErrInvalid, agentKey, a.Address)
	}
	if !strings.EqualFold(op.Sender, a.Address) || (len(op.InitCode) > 0 && !bytes.Equal(op.InitCode, a.InitCode())) {
		return fmt.Errorf("%w: the operation is not %s's", ErrInvalid, a.Address)
	}
	if len(op.CallData) < 4+32 || !bytes.Equal(op.CallData[:4], executeSelector) {
		return fmt.Errorf("%w: the agent's key signs only a call out of the account", ErrInvalid)
	}
	self, _ := addressW(a.Address)
	if bytes.Equal(op.CallData[4:36], self[:]) {
		return fmt.Errorf("%w: the account's owners and code are the person's to change", ErrInvalid)
	}
	h, err := op.Hash(c.ID)
	if err != nil {
		return err
	}
	sig, err := s.SignHash(ctx, agentKey, h)
	if err != nil {
		return err
	}
	op.Signature, err = KeySignature(OwnerAgent, sig)
	return err
}

// SignAsOwner puts the person's passkey assertion over op's hash on the chain on op. Their device signs: Lens never
// can.
func (c Chain) SignAsOwner(op *UserOp, a WebAuthn) error {
	sig, err := PasskeySignature(OwnerPasskey, a)
	if err != nil {
		return err
	}
	op.Signature = sig
	return nil
}

// Send hands op to the bundler and answers its hash.
func (c Chain) Send(ctx context.Context, op UserOp) (string, error) {
	var hash string
	if err := c.rpc(ctx, c.BundlerURL, "eth_sendUserOperation", []any{op, EntryPoint}, &hash); err != nil {
		return "", err
	}
	return hash, nil
}

// Receipt is how operation hash ended: nil while it is not yet on the chain.
type Receipt struct {
	Success       bool   `json:"success"`
	ActualGasCost string `json:"actualGasCost"` // wei the EntryPoint took for the operation's network fee
	Receipt       struct {
		TransactionHash string `json:"transactionHash"`
	} `json:"receipt"`
}

// Receipt asks the bundler how operation hash ended.
func (c Chain) Receipt(ctx context.Context, hash string) (*Receipt, error) {
	var r *Receipt
	if err := c.rpc(ctx, c.BundlerURL, "eth_getUserOperationReceipt", []any{hash}, &r); err != nil {
		return nil, err
	}
	return r, nil
}
