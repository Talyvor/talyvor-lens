package economy

import (
	"errors"
	"strings"
	"testing"
)

// B62.1 — a payment is sent under an approval only when it is exactly the payment shown: another payee, a larger or
// a smaller amount (no approval says "up to"), another currency or another agent is refused, in words.
func TestB621_ApprovalMatchesOnlyThePaymentShown(t *testing.T) {
	shown := ProofShown{ApprovalID: "apr_1", AgentID: "agt_payer", AgentName: "Payer", Payee: Payee{Kind: "agent", ID: "agt_ledger", Name: "Ledger"},
		AmountMinor: 15_000, Currency: "EUR"}
	sent := ProofSent{AgentID: "agt_payer", Payee: Payee{Kind: "agent", ID: "agt_ledger", Name: "Ledger"}, AmountMinor: 15_000, Currency: "EUR"}
	if err := approvalMatches("ws", shown, sent); err != nil {
		t.Fatalf("the payment shown: %v, want it sent", err)
	}
	for name, change := range map[string]func(*ProofSent){
		"another payee":    func(s *ProofSent) { s.Payee = Payee{Kind: "agent", ID: "agt_supplier", Name: "Supplier"} },
		"a larger amount":  func(s *ProofSent) { s.AmountMinor = 15_001 },
		"a smaller amount": func(s *ProofSent) { s.AmountMinor = 14_999 },
		"another currency": func(s *ProofSent) { s.Currency = "USD" },
		"another agent":    func(s *ProofSent) { s.AgentID = "agt_other" },
	} {
		s := sent
		change(&s)
		err := approvalMatches("ws", shown, s)
		var changed *ApprovalChangedError
		if !errors.As(err, &changed) || !errors.Is(err, ErrAgentRule) || !strings.Contains(err.Error(), "You approved €150.00 to Ledger") {
			t.Errorf("%s: %v, want refused as not the payment approved", name, err)
		}
	}
}
