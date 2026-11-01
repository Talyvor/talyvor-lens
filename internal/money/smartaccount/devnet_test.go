package smartaccount

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	secpecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"

	"github.com/talyvor/lens/internal/money/coinbase/cdpfake"
)

// devnet_test.go — B48.2 on a local devnet: two anvil chains with the audited contracts deployed as Coinbase deploys
// them (the implementation and factory through the CREATE2 deployer, so at the same addresses on both), ERC-4337's
// EntryPoint at its address, and a local bundler in front of each. LENS_TEST_DEVNET_CONTRACTS names the directory
// scripts/devnet-contracts.sh built the contracts into; without it the test skips, and CI's devnet job sets it.

const (
	create2Deployer = "0x4e59b44847b379578588920ca78fbf26c0b4956c" // anvil has it, as most chains do
	funded          = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266" // anvil's first account, unlocked
)

func contracts(t *testing.T) string {
	dir := os.Getenv("LENS_TEST_DEVNET_CONTRACTS")
	if dir == "" {
		t.Skip("LENS_TEST_DEVNET_CONTRACTS is not set: run scripts/devnet-contracts.sh DIR and set it to DIR")
	}
	if _, err := exec.LookPath("anvil"); err != nil {
		t.Fatal("LENS_TEST_DEVNET_CONTRACTS is set but anvil is not on PATH")
	}
	return dir
}

func creationCode(t *testing.T, dir, name string) []byte {
	raw, err := os.ReadFile(filepath.Join(dir, name+".hex"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(raw)), "0x"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// anvil starts a local chain with chainID and answers it, with no bundler yet.
func anvil(t *testing.T, chainID int64) Chain {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command("anvil", "--silent", "--host", "127.0.0.1", "--port", fmt.Sprint(port), "--chain-id", fmt.Sprint(chainID))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	c := Chain{ID: chainID, RPCURL: fmt.Sprintf("http://127.0.0.1:%d", port)}
	for i := 0; ; i++ {
		var id string
		if err := c.rpc(context.Background(), c.RPCURL, "eth_chainId", nil, &id); err == nil {
			break
		} else if i == 100 {
			t.Fatalf("anvil did not start: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return c
}

// transact sends data from anvil's funded account and answers the mined receipt.
func transact(t *testing.T, c Chain, to string, data []byte) map[string]any {
	t.Helper()
	return transactValue(t, c, to, new(big.Int), data)
}

// transactValue is transact, sending wei with data.
func transactValue(t *testing.T, c Chain, to string, wei *big.Int, data []byte) map[string]any {
	t.Helper()
	tx := map[string]string{"from": funded, "data": hexBytes(data), "gas": "0x1c9c380", "value": quantity(wei)}
	if to != "" {
		tx["to"] = to
	}
	var hash string
	if err := c.rpc(context.Background(), c.RPCURL, "eth_sendTransaction", []any{tx}, &hash); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		var r map[string]any
		if err := c.rpc(context.Background(), c.RPCURL, "eth_getTransactionReceipt", []any{hash}, &r); err != nil || (r != nil && r["status"] != "0x1") {
			t.Fatalf("transaction %s: %v %v", hash, r, err)
		}
		if r != nil {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("transaction %s was not mined", hash)
	return nil
}

func ethCall(c Chain, to string, data []byte) ([]byte, error) {
	var out string
	if err := c.rpc(context.Background(), c.RPCURL, "eth_call", []any{map[string]string{"from": funded, "to": to, "data": hexBytes(data)}, "latest"}, &out); err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimPrefix(out, "0x"))
}

// create2 deploys initCode through the CREATE2 deployer with a zero salt and answers its address.
func create2(t *testing.T, c Chain, initCode []byte) string {
	t.Helper()
	dw, _ := addressW(create2Deployer)
	codeHash := keccak(initCode)
	addr := keccak([]byte{0xff}, dw[12:], make([]byte, 32), codeHash[:])
	transact(t, c, create2Deployer, append(make([]byte, 32), initCode...))
	return checksum(addr[12:])
}

// deploy puts the contracts on c: EntryPoint v0.6 at its address, Coinbase Smart Wallet's implementation and factory.
func deploy(t *testing.T, c Chain, dir string) Factory {
	t.Helper()
	r := transact(t, c, "", creationCode(t, dir, "EntryPoint"))
	var code string
	if err := c.rpc(context.Background(), c.RPCURL, "eth_getCode", []any{r["contractAddress"], "latest"}, &code); err != nil {
		t.Fatal(err)
	}
	if err := c.rpc(context.Background(), c.RPCURL, "anvil_setCode", []any{EntryPoint, code}, nil); err != nil {
		t.Fatal(err)
	}
	impl := create2(t, c, creationCode(t, dir, "CoinbaseSmartWallet"))
	iw, _ := addressW(impl)
	factory := create2(t, c, append(creationCode(t, dir, "CoinbaseSmartWalletFactory"), iw[:]...))
	return Factory{Address: factory, Implementation: impl}
}

// opTuple is op as handleOps takes it.
func opTuple(op UserOp) tuple {
	s, _ := addressW(op.Sender)
	return tuple{s, bigW(op.Nonce), bytesV(op.InitCode), bytesV(op.CallData), bigW(op.CallGasLimit), bigW(op.VerificationGasLimit),
		bigW(op.PreVerificationGas), bigW(op.MaxFeePerGas), bigW(op.MaxPriorityFeePerGas), bytesV(op.PaymasterAndData), bytesV(op.Signature)}
}

const opType = "(address,uint256,bytes,bytes,uint256,uint256,uint256,uint256,uint256,bytes,bytes)"

// bundle starts a local ERC-4337 bundler in front of c: it simulates each operation it is handed, refuses one the
// EntryPoint would, and submits the rest in handleOps from anvil's funded account.
func bundle(t *testing.T, c Chain) Chain {
	var mu sync.Mutex
	txOf := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		answer := func(result any, err error) {
			out := map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}
			if err != nil {
				out = map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32500, "message": err.Error()}}
			}
			_ = json.NewEncoder(w).Encode(out)
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Params) == 0 {
			answer(nil, errors.New("bad request"))
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch req.Method {
		case "eth_sendUserOperation":
			var op UserOp
			if err := json.Unmarshal(req.Params[0], &op); err != nil {
				answer(nil, err)
				return
			}
			fw, _ := addressW(funded)
			data := call("handleOps("+opType+"[],address)", array{opTuple(op)}, fw)
			if _, err := ethCall(c, EntryPoint, data); err != nil {
				answer(nil, err) // FailedOp: AA24 signature error, among others
				return
			}
			h, err := ethCall(c, EntryPoint, call("getUserOpHash("+opType+")", opTuple(op)))
			if err != nil {
				answer(nil, err)
				return
			}
			r := transact(t, c, EntryPoint, data)
			hash := hexBytes(h)
			txOf[hash] = r["transactionHash"].(string)
			answer(hash, nil)
		case "eth_getUserOperationReceipt":
			var hash string
			_ = json.Unmarshal(req.Params[0], &hash)
			tx, ok := txOf[hash]
			if !ok {
				answer(nil, nil)
				return
			}
			var rec struct {
				Logs []struct {
					Topics []string `json:"topics"`
					Data   string   `json:"data"`
				} `json:"logs"`
			}
			if err := c.rpc(r.Context(), c.RPCURL, "eth_getTransactionReceipt", []any{tx}, &rec); err != nil {
				answer(nil, err)
				return
			}
			event := keccak([]byte("UserOperationEvent(bytes32,address,address,uint256,bool,uint256,uint256)"))
			for _, l := range rec.Logs {
				if len(l.Topics) > 1 && l.Topics[0] == hexBytes(event[:]) && l.Topics[1] == hash {
					d, _ := hex.DecodeString(strings.TrimPrefix(l.Data, "0x"))
					// UserOperationEvent's data: nonce, success, actualGasCost, actualGasUsed.
					answer(map[string]any{"success": len(d) >= 64 && d[63] == 1, "actualGasCost": quantity(new(big.Int).SetBytes(d[64:96])),
						"receipt": map[string]string{"transactionHash": tx}}, nil)
					return
				}
			}
			answer(nil, errors.New("no UserOperationEvent"))
		default:
			answer(nil, errors.New("not offered: "+req.Method))
		}
	}))
	t.Cleanup(srv.Close)
	c.BundlerURL = srv.URL
	return c
}

// webAuthn is passkey's assertion over hash, as a browser's navigator.credentials.get gives it with the hash as its
// challenge.
func webAuthn(t *testing.T, passkey *ecdsa.PrivateKey, hash [32]byte) WebAuthn {
	t.Helper()
	rp := sha256.Sum256([]byte("app.talyvor.com"))
	auth := append(rp[:], 0x05, 0, 0, 0, 1) // user present and verified, counter 1
	client := `{"type":"webauthn.get","challenge":"` + base64.RawURLEncoding.EncodeToString(hash[:]) + `","origin":"https://app.talyvor.com","crossOrigin":false}`
	ch := sha256.Sum256([]byte(client))
	msg := sha256.Sum256(append(append([]byte{}, auth...), ch[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, passkey, msg[:])
	if err != nil {
		t.Fatal(err)
	}
	return WebAuthn{AuthenticatorData: auth, ClientDataJSON: client, Signature: sig}
}

// keySign is a secp256k1 key's r‖s‖v signature of hash, and the key's address.
func keySign(k *secp256k1.PrivateKey, hash [32]byte) []byte {
	c := secpecdsa.SignCompact(k, hash[:], false)
	return append(c[1:], c[0])
}

func keyAddress(k *secp256k1.PrivateKey) string {
	h := keccak(k.PubKey().SerializeUncompressed()[1:])
	return checksum(h[12:])
}

func balance(t *testing.T, c Chain, addr string) *big.Int {
	n, err := c.quantity(context.Background(), "eth_getBalance", []any{addr, "latest"})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// B48.2 — an agent's account is worked out before it exists, at the same address on two chains; its first operation
// deploys it; it accepts the agent's key (inside the provider), the person's passkey and their recovery signer, and
// refuses any other signer; the agent's key is never asked to change the account.
func TestAgentSmartAccountOnDevnet(t *testing.T) {
	dir := contracts(t)
	ctx := context.Background()
	a, b := bundle(t, anvil(t, 31337)), bundle(t, anvil(t, 31338))
	factory := deploy(t, a, dir)
	if fb := deploy(t, b, dir); fb != factory {
		t.Fatalf("the factory is %v on one chain and %v on the other", factory, fb)
	}

	cdp := cdpfake.New(t)
	signers := cdp.Signers(t)
	agentKey, err := signers.NewSigner(ctx, "agent_signer:agt_1", "agt_1")
	if err != nil {
		t.Fatal(err)
	}
	passkey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	recoveryKey, _ := secp256k1.GeneratePrivateKey()
	acct, err := New(factory, &passkey.PublicKey, agentKey, keyAddress(recoveryKey))
	if err != nil {
		t.Fatal(err)
	}

	// The same address on both chains, worked out here as each chain's factory works it out, and nothing there yet.
	for _, c := range []Chain{a, b} {
		got, err := ethCall(c, factory.Address, call("getAddress(bytes[],uint256)", ownersV(acct.Owners), uintW(0)))
		if err != nil {
			t.Fatal(err)
		}
		if w, _ := addressW(acct.Address); !bytes.Equal(got, w[:]) {
			t.Fatalf("chain %d's factory puts the account at %x, Lens at %s", c.ID, got, acct.Address)
		}
		if deployed, err := c.Deployed(ctx, acct.Address); err != nil || deployed {
			t.Fatalf("chain %d: deployed before its first operation (%v)", c.ID, err)
		}
		// shortcut: the account pays its own gas; B48.5's paymaster pays it instead.
		if err := c.rpc(ctx, c.RPCURL, "anvil_setBalance", []any{acct.Address, "0xde0b6b3a7640000"}, nil); err != nil {
			t.Fatal(err)
		}
	}

	payee := "0x000000000000000000000000000000000000b0b0"
	pay := func(wei int64) []byte {
		data, err := Execute(payee, big.NewInt(wei), nil)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	send := func(c Chain, op UserOp) error {
		hash, err := c.Send(ctx, op)
		if err != nil {
			return err
		}
		if want, _ := op.Hash(c.ID); hash != hexBytes(want[:]) {
			t.Fatalf("the EntryPoint hashes the operation %s, Lens %x", hash, want)
		}
		r, err := c.Receipt(ctx, hash)
		if err != nil || r == nil || !r.Success {
			t.Fatalf("operation %s: %+v %v", hash, r, err)
		}
		return nil
	}

	// The agent's first payment on chain a deploys the account, signed by its key inside the provider.
	op, err := a.Prepare(ctx, acct, pay(1000))
	if err != nil {
		t.Fatal(err)
	}
	if len(op.InitCode) == 0 {
		t.Fatal("the first operation does not deploy the account")
	}
	if err := a.SignAsAgent(ctx, signers, acct, agentKey, &op); err != nil {
		t.Fatal(err)
	}
	if err := send(a, op); err != nil {
		t.Fatal(err)
	}
	if deployed, _ := a.Deployed(ctx, acct.Address); !deployed {
		t.Fatal("the first operation did not deploy the account")
	}
	if got := balance(t, a, payee); got.Int64() != 1000 {
		t.Fatalf("the payee holds %s wei, not 1000", got)
	}

	// The person's passkey signs the next one; it deploys nothing.
	op, err = a.Prepare(ctx, acct, pay(2000))
	if err != nil {
		t.Fatal(err)
	}
	if len(op.InitCode) != 0 {
		t.Fatal("a deployed account is deployed again")
	}
	h, _ := op.Hash(a.ID)
	if err := a.SignAsOwner(&op, webAuthn(t, passkey, h)); err != nil {
		t.Fatal(err)
	}
	if err := send(a, op); err != nil {
		t.Fatal(err)
	}

	// Any other signer is refused: a stranger's key in the agent's place, a stranger's passkey in the person's.
	stranger, _ := secp256k1.GeneratePrivateKey()
	strangerPasskey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	op, err = a.Prepare(ctx, acct, pay(3000))
	if err != nil {
		t.Fatal(err)
	}
	h, _ = op.Hash(a.ID)
	op.Signature, _ = KeySignature(OwnerAgent, keySign(stranger, h))
	if _, err := a.Send(ctx, op); !errors.Is(err, ErrRefused) {
		t.Fatalf("a stranger's key: %v, want refused", err)
	}
	if err := a.SignAsOwner(&op, webAuthn(t, strangerPasskey, h)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send(ctx, op); !errors.Is(err, ErrRefused) {
		t.Fatalf("a stranger's passkey: %v, want refused", err)
	}
	if got := balance(t, a, payee); got.Int64() != 3000 {
		t.Fatalf("the payee holds %s wei, not 3000", got)
	}

	// The agent's key is never asked to change the account: removing the person's passkey is refused before it signs.
	removeOwner, _ := Execute(acct.Address, nil, call("removeOwnerAtIndex(uint256,bytes)", uintW(OwnerPasskey), bytesV(acct.Owners[OwnerPasskey])))
	op, err = a.Prepare(ctx, acct, removeOwner)
	if err != nil {
		t.Fatal(err)
	}
	before := cdp.Signed()
	if err := a.SignAsAgent(ctx, signers, acct, agentKey, &op); !errors.Is(err, ErrInvalid) || cdp.Signed() != before {
		t.Fatalf("the agent's key was asked to remove the person's passkey: %v", err)
	}

	// On chain b the same address is deployed by its first operation, here signed by the person's passkey, and the
	// recovery signer signs the next.
	op, err = b.Prepare(ctx, acct, pay(500))
	if err != nil {
		t.Fatal(err)
	}
	if len(op.InitCode) == 0 {
		t.Fatal("the first operation on the second chain does not deploy the account")
	}
	h, _ = op.Hash(b.ID)
	if err := b.SignAsOwner(&op, webAuthn(t, passkey, h)); err != nil {
		t.Fatal(err)
	}
	if err := send(b, op); err != nil {
		t.Fatal(err)
	}
	op, err = b.Prepare(ctx, acct, pay(700))
	if err != nil {
		t.Fatal(err)
	}
	h, _ = op.Hash(b.ID)
	op.Signature, _ = KeySignature(OwnerRecovery, keySign(recoveryKey, h))
	if err := send(b, op); err != nil {
		t.Fatal(err)
	}
	if got := balance(t, b, payee); got.Int64() != 1200 {
		t.Fatalf("the payee holds %s wei on the second chain, not 1200", got)
	}
}
