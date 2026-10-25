package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

type fakeRequests struct {
	key string
	in  economy.PaymentRequestIn
}

func (f *fakeRequests) PreparePaymentRequest(_ context.Context, _, _, key string, in economy.PaymentRequestIn) (economy.PaymentRequest, bool, error) {
	f.key, f.in = key, in
	return economy.PaymentRequest{ID: "preq_1", Status: "pending_approval"}, false, nil
}
func (f *fakeRequests) PaymentRequest(context.Context, string, string, string) (economy.PaymentRequest, error) {
	return economy.PaymentRequest{}, economy.ErrPaymentRequestNotFound
}
func (f *fakeRequests) PrepareBillsPayment(_ context.Context, _, _, key, client string, billIDs []string) (economy.PaymentRequest, bool, error) {
	f.key, f.in = key, economy.PaymentRequestIn{Client: client}
	for _, id := range billIDs {
		f.in.Items = append(f.in.Items, economy.PaymentRequestItemIn{BillID: id})
	}
	return economy.PaymentRequest{ID: "preq_1", Status: "pending_approval"}, false, nil
}
func (f *fakeRequests) Bills(context.Context, string) ([]economy.Bill, error) { return nil, nil }
func (f *fakeRequests) PayeesNamed(context.Context, string, string) ([]economy.OutsidePayee, error) {
	return nil, nil
}
func (f *fakeRequests) CancelPaymentRequest(context.Context, string, string, string, string) (economy.PaymentRequest, error) {
	return economy.PaymentRequest{}, economy.ErrPaymentRequestNotFound
}

// B40.3: prepare_payment and request_marketplace_purchase each prepare one item, as the owner's named client; bank
// details in the arguments are refused before anything is prepared.
func TestMCP_PaymentRequestToolsPrepareOnly(t *testing.T) {
	s := newServer(nil, nil, nil, nil, nil, "test")
	f := &fakeRequests{}
	s.SetPaymentRequests(f)
	owner := &auth.AuthContext{WorkspaceID: "ws-owner", AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(args string) map[string]any {
		t.Helper()
		resp := s.dispatch(auth.WithAuthContext(context.Background(), owner), rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"),
			Method: "tools/call", Params: json.RawMessage(args)})
		raw, _ := json.Marshal(resp)
		var out struct {
			Result map[string]any `json:"result"`
		}
		_ = json.Unmarshal(raw, &out)
		return out.Result
	}
	call(`{"name":"prepare_payment","arguments":{"idempotency_key":"k1","client":"voice","payee_id":"payee_1","currency":"GBP","amount_minor":900}}`)
	if f.key != "k1" || f.in.Client != "voice" || len(f.in.Items) != 1 || f.in.Items[0].PayeeID != "payee_1" || f.in.Items[0].AmountMinor != 900 {
		t.Errorf("prepare_payment prepared %q %+v", f.key, f.in)
	}
	call(`{"name":"request_marketplace_purchase","arguments":{"idempotency_key":"k2","client":"chat","listing_id":"lst_1","amount_minor":1}}`)
	if len(f.in.Items) != 1 || f.in.Items[0] != (economy.PaymentRequestItemIn{ListingID: "lst_1"}) {
		t.Errorf("request_marketplace_purchase prepared %+v", f.in)
	}
	// B41.102: prepare_bills_payment prepares the bills named, as the owner's named client.
	call(`{"name":"prepare_bills_payment","arguments":{"idempotency_key":"k4","client":"chat","bill_ids":["bill_1","bill_2"]}}`)
	if f.key != "k4" || f.in.Client != "chat" || len(f.in.Items) != 2 || f.in.Items[1].BillID != "bill_2" {
		t.Errorf("prepare_bills_payment prepared %q %+v", f.key, f.in)
	}
	f.key = ""
	res := call(`{"name":"prepare_batch","arguments":{"idempotency_key":"k3","client":"chat","items":[{"payee_id":"p","currency":"GBP","amount_minor":1,"sort_code":"200000"}]}}`)
	if text, _ := json.Marshal(res); f.key != "" || res["isError"] != true || !strings.Contains(string(text), "payee details supplied by the assistant") {
		t.Errorf("bank details in a batch = %v (prepared %q)", res, f.key)
	}
}
