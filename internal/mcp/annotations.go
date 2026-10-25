package mcp

// toolAnnotation is what a client shows and asks before calling a tool (B40.1): its title, and whether it only
// reads (readOnlyHint) or moves money or sends something to someone (destructiveHint).
type toolAnnotation struct {
	title string
	kind  int
}

const (
	reads  = iota // readOnlyHint: changes nothing
	writes        // changes something of the caller's own; moves no money, sends nothing
	moves         // destructiveHint: moves money, or sends something to someone
)

// toolAnnotations names every tool /mcp offers; TestMCP_EveryToolIsAnnotated fails for one missing here.
var toolAnnotations = map[string]toolAnnotation{
	"get_spend_summary":         {"Spend summary", reads},
	"get_cache_stats":           {"Cache statistics", reads},
	"get_model_recommendations": {"Caching recommendations", reads},
	"check_circuit_breakers":    {"Circuit breakers", reads},
	"get_session_summary":       {"Session summary", reads},
	"route_model":               {"Choose a model", reads},
	"get_mandates":              {"Permission mandates", reads},

	"prepare_payment":              {"Prepare a payment for approval", moves},
	"prepare_batch":                {"Prepare payments for approval", moves},
	"prepare_bills_payment":        {"Prepare paying the bills due for approval", moves},
	"list_bills_due":               {"Bills due", reads},
	"prepare_transfer_to_contact":  {"Prepare a transfer to a saved contact for approval", moves},
	"request_marketplace_purchase": {"Prepare a marketplace purchase for approval", moves},
	"cancel_request":               {"Cancel a payment request", moves},
	"get_request_status":           {"Payment request status", reads},

	"agent_balance":          {"Your balance and rules", reads},
	"agent_request_approval": {"Ask a person to approve a payment", moves},
	"agent_pay":              {"Pay an agent", moves},
	"agent_receipt":          {"Receipt", reads},

	"wallet_send":            {"Send credits", moves},
	"wallet_request":         {"Request credits", moves},
	"wallet_requests":        {"Credit requests", reads},
	"wallet_answer_request":  {"Answer a credit request", moves},
	"wallet_refund":          {"Refund a transfer", moves},
	"wallet_credit_line":     {"Credit line", reads},
	"wallet_offer_loan":      {"Offer a loan", moves},
	"wallet_loans":           {"Loans", reads},
	"wallet_answer_loan":     {"Answer a loan offer", moves},
	"wallet_escrow_pay":      {"Pay into escrow", moves},
	"wallet_escrows":         {"Escrows", reads},
	"wallet_escrow_confirm":  {"Confirm an escrow delivery", moves},
	"wallet_escrow_dispute":  {"Dispute an escrow", moves},
	"wallet_pots":            {"Pots", reads},
	"wallet_pot_create":      {"Make a pot", writes},
	"wallet_pot_move":        {"Move credits into or out of a pot", moves},
	"wallet_quotes":          {"Simulated market quotes", reads},
	"wallet_portfolio_open":  {"Open a simulated portfolio", writes},
	"wallet_portfolios":      {"Simulated portfolios", reads},
	"wallet_order":           {"Place a simulated order", moves},
	"wallet_order_cancel":    {"Cancel a simulated order", writes},
	"wallet_accounts":        {"Currency accounts", reads},
	"wallet_account_open":    {"Open a currency account", writes},
	"wallet_account_details": {"Account details", reads},
	"wallet_pay_external":    {"Pay a saved payee", moves},
	"wallet_invoice_create":  {"Issue an invoice", moves},
	"wallet_invoices":        {"Invoices", reads},
	"wallet_invoice_pay":     {"Pay an invoice", moves},
	"wallet_credential":      {"Know Your Agent credential", reads},

	"market_search":   {"Search the marketplace", reads},
	"market_listing":  {"Marketplace listing", reads},
	"market_license":  {"Buy, rent or subscribe to a listing", moves},
	"market_use":      {"Use a listing", moves},
	"market_licences": {"Your licences", reads},
	"market_cancel":   {"Stop a licence renewing", moves},

	"room_list":     {"Rooms", reads},
	"room_join":     {"Join a room", writes},
	"room_messages": {"Room messages", reads},
	"room_post":     {"Post to a room", moves},
	"room_propose":  {"Propose a contribution", moves},
	"room_fork":     {"Fork a contribution", moves},
	"room_vote":     {"Vote on a contribution", writes},
	"room_run":      {"Run in a room", moves},
}

// annotate gives each tool its title and annotations.
func annotate(tools []map[string]any) []map[string]any {
	for _, t := range tools {
		a := toolAnnotations[t["name"].(string)]
		t["title"] = a.title
		t["annotations"] = map[string]any{"title": a.title, "readOnlyHint": a.kind == reads, "destructiveHint": a.kind == moves,
			"openWorldHint": false}
	}
	return tools
}
