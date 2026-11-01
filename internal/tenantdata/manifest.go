// Package tenantdata declares, in one place, every table that holds a customer's data and what
// must happen to it when that customer leaves.
//
// WHY THIS EXISTS WITHOUT A DELETION ENDPOINT. Deletion is done by hand today, and the policy says
// so. This is what makes a by-hand deletion possible at all: `workspace_id` has NO foreign key
// anywhere in the schema (`REFERENCES workspaces` appears zero times across 111 migrations), so
// there is no ON DELETE CASCADE to lean on. Removing a customer means an explicit DELETE against
// every table carrying the column — and the only way to know that list is to write it down and
// keep it honest.
//
// THE GUARD IS THE POINT, not the list. manifest_test.go reads the LIVE SCHEMA and fails when a
// tenant-keyed table is not classified here. Without it this file decays the moment someone adds a
// migration, and a by-hand deletion silently misses a table — which is worse than having no
// procedure, because it produces a confident claim that someone's data is gone.
//
// ⚠ W6.26 — AND THE GUARD'S POPULATION WAS THE THING THAT WAS WRONG. It asked the schema for
// tables with a column named exactly `workspace_id`. 58 of the 99 tables answer to that. But the
// tenant key is spelled SIX other ways in this schema — measured, not guessed:
//
//	workspace_id              58   owner_workspace_id      2
//	contributor_workspace_id   8   attester_workspace_id   1
//	requester_workspace_id     5   author_workspace_id     1
//	                               source_workspace        1
//
// ⚠ Those are TABLE counts, and the first draft of this comment had 9 and 6 rather than 8 and 5
// because it counted two VIEWS — distill_royalty_margin and pool_royalty_margin — as tables. The
// widened guard listed them as unclassified tenant tables too, which would have been a false
// finding of exactly the flattering kind: a view holds no rows and a DELETE against one fails.
// TestPopulationExcludesViews is that mistake, kept.
//
// Twelve tables carry one of the other six and NO bare workspace_id, so the guard could not see
// them, so nobody classified them, so a by-hand erasure misses them. A thirteenth — workspace_configs
// — is keyed by `id` like `workspaces` is, and the guard hand-exempted `workspaces` by name rather
// than admitting the category existed.
//
// ⚠ THE SHAPE OF THE MISS IS WORTH MORE THAN THE COUNT. tenant.Store owns exactly two tables and
// says so in its own comment: `workspace_configs` and `workspace_api_keys`. One of them was
// classified and one was not, and the only difference between them is how the column is spelled.
// A population boundary drawn on a NAME will always cut somewhere the data does not.
//
// The population is now: relkind r/p (not views — see the control in manifest_test.go), not a
// partition, and either a column matching %workspace% or membership of MappingTables below.
package tenantdata

// MappingTables are the tables whose PRIMARY KEY *is* the workspace id, so no column matching
// %workspace% exists to discover them by. The schema cannot tell us that `id` means a workspace
// here and a row id elsewhere, so this is declared — but it is declared, checked (every name must
// exist and must be classified), and no longer a silent exemption inside a test.
//
// ⚠ `workspaces` was already exempt, by name, in the staleness check. `workspace_configs` was not,
// and that is the whole finding: the exemption named one member of a category instead of naming
// the category.
var MappingTables = []string{"workspaces", "workspace_configs"}

// Disposition is what happens to a table's rows when a customer leaves.
type Disposition int

const (
	// Delete — rows are removed. This is the default for customer data and covers the bulk of
	// what is actually sensitive: cached answers, embeddings, prompts, keys, sessions, config.
	Delete Disposition = iota

	// Retain — rows stay, with a legal reason recorded below. These are audit-guarded: migration
	// 0055's audit_block_mutation raises on UPDATE **and** DELETE, so they can be neither removed
	// nor rewritten. That is deliberate and this manifest does not propose changing it.
	Retain

	// NotTenantScoped — the table carries a workspace_id column but holds no customer content:
	// operational bookkeeping that references a workspace without describing anybody. Classified
	// explicitly so the guard cannot be satisfied by silence.
	NotTenantScoped
)

// String names the disposition. Without it the guards' failure messages print the raw iota —
// "classified 0" — which reads as a missing value rather than as Delete. Control T5 produced
// exactly that message and it was the message that was wrong, not the guard.
func (d Disposition) String() string {
	switch d {
	case Delete:
		return "Delete"
	case Retain:
		return "Retain"
	case NotTenantScoped:
		return "NotTenantScoped"
	}
	return "Disposition(unknown)"
}

// Entry is one table's classification and the reason for it.
type Entry struct {
	Disposition Disposition
	// Why is required for Retain and NotTenantScoped: a classification without a reason is an
	// assertion, and the next person cannot tell a decision from an oversight.
	Why string
}

// Manifest maps table name → disposition. Partitioned tables appear ONCE, under the parent:
// a DELETE against the parent reaches every partition, and listing the eight children of each
// would be noise that rots the moment the partition count changes.
var Manifest = map[string]Entry{
	// ─── RETAIN: audit-guarded (migration 0055). Cannot be deleted OR updated. ────────────────
	//
	// These are the financial and provenance record. UK GDPR erasure is not absolute — it yields
	// to legal obligation and to the establishment or defence of legal claims, and accounting
	// records are the standard example. Deleting audit_block_mutation to satisfy an erasure
	// request would trade a working integrity control for the appearance of compliance, and would
	// leave us unable to prove what we charged anyone.
	//
	// ⚠ Note these cannot be ANONYMISED either: the trigger blocks UPDATE, so the workspace_id
	// column cannot be rewritten. Unlinkability comes from deleting the MAPPING instead — the
	// workspace row, its keys and its sessions — after which these rows reference a derived id
	// (u + base32(sha256(issuer‖sub))) that resolves to nobody. That is pseudonymisation, not
	// anonymisation, and the distinction is a lawyer's to draw.
	"lens_token_ledger":   {Retain, "audit-guarded (0055): LENS mint/spend record; financial-record retention"},
	"lxc_ledger":          {Retain, "audit-guarded (0055): LXC money ledger; financial-record retention"},
	"request_attribution": {Retain, "audit-guarded (0055): provenance of what was served and to whom"},
	"povi_receipts":       {Retain, "audit-guarded (0055): signed compute receipts; tamper-evidence record"},
	// B21.3 (0138): who asked for a deletion and what was deleted — the record that it was done. No
	// question or answer is held in either.
	"deletion_requests":         {Retain, "B21.3: the record that erasure was requested and completed"},
	"stored_answer_deletions":   {Retain, "B21.3: the audit log of each deletion of stored answers"},
	"answer_removals":           {Retain, "B23.1: who marked a served answer wrong, and what was removed; holds no question or answer"},
	"agent_postings":            {Retain, "B19.1: audit-guarded (0140, append-only): the agents' double-entry LXC ledger; financial-record retention"},
	"agent_card_authorizations": {Retain, "B19.12: audit-guarded (0156, append-only): every agent card authorisation, approved or declined, and the LXC it debited; financial-record retention"},
	"agent_payee_payments":      {Retain, "B28.304: audit-guarded (0191, append-only): every payment an agent's rules let through and whom it paid, which its per-payee daily caps count; financial-record retention"},
	"agent_card_settlements":    {Retain, "B19.25: audit-guarded (0157, append-only): each capture, release and refund that settled an agent card authorisation, and the LXC it moved; financial-record retention"},
	"agent_transfers":           {Retain, "B22.3: keys `from_workspace_id`+`to_workspace_id`; audit-guarded (0159, append-only): each transfer of credits between two agents and its postings; financial-record retention"},
	"agent_tool_calls":          {Retain, "B19.9: audit-guarded (0145, append-only): every call an agent made to its wallet tools; the record of what it asked and was refused"},
	"market_earnings":           {Retain, "B20.2: audit-guarded (0149, append-only): a seller's share of each cleared use — money owed to them, paid out by B20.5"},
	"market_receipts":           {Retain, "B32.40: audit-guarded (0225, append-only): Talyvor's VAT receipt for each paid marketplace bill — a tax document kept as issued"},
	"market_self_bills":         {Retain, "B32.43: audit-guarded (0228, append-only): the self-billed invoice for each weekly payout, from the seller to Talyvor — a tax document kept as issued"},
	"market_refunds":            {Retain, "B20.4: audit-guarded (0152, append-only but for its Stripe credit): each refunded use of a taken-down listing — money given back to its buyer and taken from its seller"},
	"market_licences":           {Retain, "B32.19: audit-guarded (0202, only status, ends_at and auto_renew change): keys `buyer_workspace_id`: each licence a workspace bought, rented or subscribed to — what its billed purchase on market_uses paid for"},
	"test_crossing_reversals":   {Retain, "B26.15: keys `from_workspace_id`+`to_workspace_id`; audit-guarded (0177, append-only): each crossing between a test workspace and a real one that the operator reversed, naming the original row; financial-record retention"},
	"market_payouts":            {Retain, "B20.5: audit-guarded (0153, append-only but for its Stripe transfer): each payout of a seller's earnings, in money through Stripe or in credits"},
	"money_entries":             {Retain, "B30.2: audit-guarded (0221, append-only): every movement of money in pounds, euros, dollars and USDC, by idempotency key; its money_postings are append-only too; financial-record retention"},
	"compliance_cases":          {Retain, "B30.6: each sanctions match screening found on the workspace's payees and outside payments — the name screened, what it matched, the amount — and the operator's decision on it; B30.7: the workspace's transaction-monitoring cases; compliance record retention"},
	"compliance_alerts":         {Retain, "B30.7: each pattern transaction monitoring found in the workspace's payments — the rule, the payments, why — on its compliance case; compliance record retention"},
	"partner_calls":             {Retain, "B37.5: audit-guarded (0238, append-only): each call made to a money partner for the workspace — service, method, the caller's id, outcome and duration, never a reference, name or account number; the audit of what was asked of a partner"},
	"compliance_freezes":        {Retain, "B30.8: the operator's freeze on the workspace's money capabilities, while it stands — the case it is on, why, who and when; a deletion request does not lift a freeze, and the audit trail keeps its history"},
	"workspace_verifications":   {Retain, "B30.4: audit-guarded (0230, append-only): each verification check — level, method, status, date and the provider's evidence reference, never a document; customer due-diligence record retention"},
	"money_accounts":            {Retain, "B30.2: each account in pounds, euros, dollars or USDC; the append-only money_postings reference it by foreign key, so an account with money moved is closed, never deleted; financial-record retention"},
	"money_invoices":            {Retain, "B30.20: each invoice an agent's account issued — the customer, the lines and VAT, the pay link's token; its money_invoice_payments name the money_entries that paid it, and its money_invoice_reminders what was sent; financial-record retention"},
	"money_mandates":            {Retain, "B30.19: keys `workspace_id`+`payee_workspace_id`: each mandate an agent's account granted a payee — the limits, the expiry, the revocation, and an outside business's key only as its SHA-256; its money_mandate_pulls name the money_entries each pull posted; financial-record retention"},
	"permission_mandates":       {Retain, "B40.2: each permission mandate the person signed — what an assistant or an agent may prepare and pay, as the signed JSON with its passkey assertion, and its revocation; evidence of what the person allowed"},
	"payment_requests":          {Retain, "B40.3: each payment request an assistant or an agent prepared — the frozen payload and its hash, the client, the Idempotency-Key, and its payment_request_items: each payee, amount, reference and how it was judged against the mandates; evidence of what was asked for"},
	"payment_request_approvals": {Retain, "B40.5: each approval the person signed for a payment request — the statement shown (payload hash, payees, amounts, totals, expiry) and its passkey or device signature; evidence of what was authorised"},
	"audit_chain":               {Retain, "B40.5: the append-only, hash-chained record of each payment request — the request, each decision, the statement shown, the signature, and each payment provider call and its answer; evidence of every step"},
	"money_payment_quotes":      {Delete, "B30.22: each quote for an international payment — the account, the payee, the amount, the fee, the rate and what would arrive — kept so a payment can be made at it; a payment keeps the figures it was made at"},
	"money_payments":            {Retain, "B30.17: each payment out to a payee outside Talyvor — amount, reference, the payee's name as paid, and where it stood with the partner; it explains money_entries; financial-record retention"},
	"money_payout_batches":      {Retain, "B30.21: each bulk payout uploaded — every row as validated, with why it was refused or the money_payments row it became and where that stood when sent; who approved it and when; financial-record retention"},
	"money_card_batches":        {Retain, "B46.10: each batch of single-use cards an agent asked for — purpose, merchants' caps, who approved it and when; it explains money_entries; financial-record retention"},
	"money_cards":               {Retain, "B46.10: each single-use card — the merchant it was locked to, its cap, the issuer's reference and what it spent; it explains money_entries; financial-record retention"},
	"money_card_authorizations": {Retain, "B46.10: each charge an issuer asked about on a card and Lens's answer, append-only; financial-record retention"},
	"money_payout_details":      {Delete, "B46.11: where the workspace is paid — an IBAN or a USDC address and its holder — and the partner's verification reference; nothing to keep once the workspace is gone"},
	"money_earnings_payouts":    {Retain, "B46.11: keys `workspace_id`+`recipient_workspace_id`: each payout of earnings — a sale, a Room's split or a royalty — its source, agreement, amount and where it stood with the partner; it explains money_entries; financial-record retention"},

	// B30.9: its key is longer than the rest, so it stands apart rather than realign them.
	"capability_terms_acceptances": {Retain, "B30.9: audit-guarded (0235, append-only): each acceptance of a capability's terms — the version, the person, when, and an HMAC of the address it came from, never the address; evidence of what the customer agreed to"},

	// token_events is audit-guarded TOO, but migration 0055's trigger carries a sanctioned
	// exception: DELETE is permitted while the retention-bypass session flag is set — the flag the
	// retention sweeper already uses. So it is deletable through a path that exists and is already
	// trusted, and it matters most, because under a `full` logging policy this is the table that
	// holds prompt text.
	//
	// ⚠ The flag is NOT named here on purpose. internal/audit's integrity guard asserts it appears
	// in exactly one non-test file — internal/audit/retention.go — because every extra reference is
	// another place that can delete audit rows. That guard caught this comment, which is the guard
	// working; see retention.go for the flag itself and the by-hand procedure for how to set it.
	"token_events": {Delete, "audit-guarded; deletable only with the retention-bypass flag set (see internal/audit/retention.go)"},

	// ─── NOT TENANT-SCOPED: carries workspace_id, holds no customer content ───────────────────
	"mint_idempotency": {NotTenantScoped, "idempotency keys; operational replay-protection, describes nobody"},

	// ─── W6.26: THE THIRTEEN THE OLD POPULATION COULD NOT SEE ────────────────────────────────
	//
	// ⚠ THE RULE THAT DECIDES THESE IS THE MANIFEST'S OWN, NOT A NEW ONE. Retain here is reserved
	// for rows migration 0055's audit_block_mutation makes UNDELETABLE — that is the whole
	// argument for the four Retain entries above. NONE of the thirteen below carries that trigger
	// (measured against pg_trigger on the migrated schema: only lens_token_ledger, lxc_ledger,
	// povi_receipts, request_attribution, token_events and reputation_events do). So the existing
	// rule classifies every one of them Delete, and the precedent is concrete rather than
	// analogical: `lens_shadow_mints`, `traffic_mint_holds` and `pattern_mine_credits` are mint
	// tables already classified Delete above.
	//
	// ⚠ NICOLAI, THE ONE THING TO CONFIRM. Six of these are *_mints — a financial-adjacent record.
	// This applies the manifest's stated rule to them rather than deciding anything new, and
	// DeleteOrder is a BY-HAND procedure, so adding a table tells a person to delete it rather
	// than deleting it. But if a mint record is a financial record that must survive erasure, the
	// fix is a trigger in a migration (which would then make Retain the honest classification
	// here), not a quiet edit to this map. Every Why below says which precedent settled it.
	"annotation_tasks": {Delete, "W6.26; key `source_workspace`. Holds response_a/response_b — model OUTPUT " +
		"text drawn from that workspace's traffic (internal/mining/annotation_mining.go). `annotations` " +
		"ON DELETE CASCADEs off it, so this entry is what reaches BOTH; before it, neither was reachable."},
	"benchmark_eval_items": {Delete, "W6.26; key `author_workspace_id`. Holds input/expected_output for a " +
		"contributed benchmark item and names the contributing workspace (internal/benchprobe/store.go)."},
	"confidential_compute_mints": {Delete, "W6.26; key `contributor_workspace_id`. Not audit-guarded; same " +
		"disposition as `lens_shadow_mints` above."},
	"distill_royalty_basis": {Delete, "W6.26; keys `owner_workspace_id`+`requester_workspace_id`. The royalty " +
		"basis names BOTH tenants of a cross-tenant serve — erasing one leaves the other's row naming them."},
	"distill_royalty_mints": {Delete, "W6.26; keys `contributor_workspace_id`+`requester_workspace_id`. Not " +
		"audit-guarded; same disposition as `lens_shadow_mints`."},
	"distill_serve_attribution": {Delete, "W6.26; keys `owner_workspace_id`+`requester_workspace_id`. A per-pair " +
		"serve counter — it describes who served whom, which is that customer's traffic."},
	"eval_contribution_mints": {Delete, "W6.26; key `contributor_workspace_id`. Not audit-guarded."},
	"eval_correctness_attestations": {Delete, "W6.26; key `attester_workspace_id`. Records which workspace " +
		"attested to which item (internal/benchprobe/store.go)."},
	"node_latency_mints": {Delete, "W6.26; key `contributor_workspace_id`. Not audit-guarded."},
	"pool_royalty_mints": {Delete, "W6.26; keys `contributor_workspace_id`+`requester_workspace_id`, and it also " +
		"carries prompt_sha256/answer_sha256 — a hash of the customer's prompt is still derived from it."},
	"pooled_shadow_observations": {Delete, "W4.9; key `workspace_id`. The cross-tenant pooling SHADOW LOG " +
		"(migration 0125). It stores no prompt TEXT, only SHA-256 fingerprints — but `pool_royalty_mints` " +
		"directly above settles that: a hash of the customer's prompt is still DERIVED FROM IT, and the row " +
		"also records that this workspace made a request at this time. Not audit-guarded (no trigger), " +
		"purely observational, and nothing downstream aggregates it into money — so there is no retention " +
		"claim pulling the other way. Delete."},
	"routing_prediction_mints": {Delete, "W6.26; key `contributor_workspace_id`. Not audit-guarded."},
	"royalty_detector_findings": {Delete, "W6.26; keys `contributor_workspace_id`+`requester_workspace_id` plus " +
		"`identity_key` — an abuse-detection finding ABOUT a named workspace."},
	// ⚠ THE ONE THAT MAKES THE PSEUDONYMISATION ARGUMENT ABOVE ACTUALLY TRUE. The Retain block
	// says unlinkability comes from deleting the MAPPING — "the workspace row, its keys and its
	// sessions" — after which the retained ledger rows reference an id that resolves to nobody.
	// There are TWO mapping tables. workspace_configs is keyed by the same id and holds the same
	// `name`, written from the admin route that calls tenant.Store.UpsertConfig. Deleting
	// `workspaces` while leaving this behind does not achieve what that paragraph claims: the
	// customer's NAME survives, still keyed by the id the retained rows point at.
	"workspace_configs": {Delete, "W6.26; keyed by `id` like `workspaces`. Holds name, spending_cap, " +
		"monthly_budget, allowed_models, retention_days. Sibling of `workspace_api_keys` — tenant.Store " +
		"owns exactly these two and only the other one was classified."},

	// ─── DELETE: everything else. The bulk of what is actually sensitive. ─────────────────────
	"agent_accounts":               {Delete, ""},
	"agent_rules":                  {Delete, ""},
	"agent_rules_versions":         {Delete, "B28.307: every version of an agent's rules and the credential that changed them"},
	"agent_rule_boosts":            {Delete, "B28.308: an agent's limits raised until a time, and the credential that raised them"},
	"kya_credentials":              {Delete, "B30.5: each Know Your Agent credential issued to the workspace's agents, the key that signed it and whether it is revoked; one deleted before it expires leaves only its id on the revocation list (kya_revocation_tombstones)"},
	"agent_card_keys":              {Delete, "B45.23: the public half of each key the workspace's agents' Agent Cards were signed with, and whether it is revoked; deleting it makes every card signed with it fail to verify"},
	"agent_cards":                  {Delete, "B19.12: an agent's test-mode card — Stripe's card and cardholder ids, last 4, expiry"},
	"agent_approvals":              {Delete, ""},
	"agent_approvers":              {Delete, "B45.28: which agent approves which agent's requests, up to what, and the owner's signed AP2 open mandate that said so"},
	"agent_lxc_subbudgets":         {Delete, ""},
	"agent_workspace_pauses":       {Delete, ""},
	"market_listings":              {Delete, "B20.1: the listings a workspace published; their versions cascade"},
	"market_uses":                  {Delete, "B20.2: keys `seller_workspace_id`+`buyer_workspace_id`: each use of a listing, what it was billed and when it cleared"},
	"agent_money_requests":         {Delete, "B22.3: keys `from_workspace_id`+`to_workspace_id`: one agent asking another for credits, and the answer; the transfer an accepted one made is in agent_transfers"},
	"company_credit_lines":         {Delete, "B22.4: a company's credit line: its limit and whether the operator paused it"},
	"credit_line_draws":            {Delete, "B22.4: what the line lent an agent and the invoice it went on; the credits themselves are lxc_ledger rows of type credit_line_draw"},
	"credit_line_invoices":         {Delete, "B22.4: each month's Stripe invoice of a line's draws, and when it was paid"},
	"agent_cash_outs":              {Delete, "B22.9: an owner's requests to turn an agent's credits into money, and what the partner said; the credits' movements are in agent_postings and lxc_ledger"},
	"uploaded_documents":           {Delete, "B18.13: documents a workspace uploaded for its chats to reference by id; the file itself is in content"},
	"agent_portfolios":             {Delete, "B22.8: an agent's simulated portfolios; their orders cascade with them. Simulated cash only: no credits or money"},
	"agent_pots":                   {Delete, "B22.7: an agent's pots — a goal, a budget or a reserve — and their locks; their money is in agent_postings"},
	"agent_account_balances":       {Delete, "B28.297: each account's running balance, kept from agent_postings by a trigger (0185); the postings are the record"},
	"agent_loans":                  {Delete, "B22.5: keys `lender_workspace_id`+`borrower_workspace_id`: a loan between two companies' agents, its terms and state; its money is in agent_transfers"},
	"agent_escrows":                {Delete, "B22.6: keys `payer_workspace_id`+`payee_workspace_id`: credits one agent paid into escrow for another, and their state; its money is in agent_postings and lxc_ledger"},
	"agent_debit_settlements":      {Delete, "B23.13: how each agent question's pre-serve estimate was settled to its delivered cost and what its limit wrote off; the credits' movements are in agent_postings and lxc_ledger"},
	"market_bills":                 {Delete, "B20.2: the Stripe subscription a buyer's marketplace uses are metered onto"},
	"market_invoices":              {Delete, "B28.140: keys `buyer_workspace_id`: each paid invoice of a buyer's marketplace bill — its billing period, number and Stripe's PDF"},
	"market_sellers":               {Delete, "B20.5: the seller's connected Stripe account and what Stripe last said of it"},
	"market_remix_grants":          {Delete, "B32.25: the remix licences a workspace accepted, one per listing version, each with the share it locked"},
	"rooms":                        {Delete, "B32.28: keys `owner_workspace_id`: the rooms a workspace opened; their terms, members and agents cascade"},
	"room_members":                 {Delete, "B32.28: the rooms a workspace joined, with its role and the terms version it accepted"},
	"room_member_agents":           {Delete, "B32.28: the workspace's agents it brought into a room"},
	"room_invites":                 {Delete, "B32.29: the invites naming the workspace to a private room; a room's own invites cascade with it"},
	"room_messages":                {Delete, "B32.30: keys `author_workspace_id`: the messages a workspace posted in rooms; a room's own messages and events cascade with it"},
	"room_contributions":           {Delete, "B32.31: keys `author_workspace_id`: the contributions a workspace proposed to rooms; a room's own contributions, and its listings', cascade with them"},
	"room_votes":                   {Delete, "B32.31: the workspace's votes on rooms' contributions; a contribution's votes cascade with it"},
	"room_prizes":                  {Delete, "B32.35: keys `poster_workspace_id`: the prizes a workspace posted in its rooms; a room's own prizes cascade with it"},
	"room_reports":                 {Delete, "B32.52: keys `reporter_workspace_id`: the reports a workspace made of rooms and their messages; a room's own reports cascade with it"},
	"market_ip_claims":             {Delete, "B32.47: keys `claimant_workspace_id`: the IP claims a workspace filed, with their evidence and decision; claims against its own listings cascade with them"},
	"market_collections":           {Delete, "B32.50: the collections of listings a workspace curated, public or not; their items cascade"},
	"market_reviews":               {Delete, "B32.49: keys `buyer_workspace_id`: the reviews a workspace wrote of listings it paid for; reviews of its own listings cascade with them"},
	"buyer_tax_profiles":           {Delete, "B32.38: the workspace's declared legal name, address, country and tax id as a buyer; the tax lines of its sales keep their own evidence"},
	"seller_tax_profiles":          {Delete, "B32.41: the workspace's tax details as a seller — names, address, TINs, date of birth and payout account, the sealed ones under the envelope key — and its reminders and payout hold"},
	"market_content_filters":       {Delete, "B41.185: the highest age rating and the content labels a workspace is shown of the marketplace"},
	"market_listing_blocks":        {Delete, "B41.185: the listings a workspace blocked; blocks of a deleted listing cascade with it"},
	"market_listing_reports":       {Delete, "B20.4: keys `reporter_workspace_id`: the reports a workspace made of listings; reports of its own listings cascade with them"},
	"workspace_passkeys":           {Delete, "B19.16: the owner's passkeys' public keys"},
	"workspace_device_keys":        {Delete, "B40.4: the owner's phones' device public keys and the attestations they were registered with"},
	"agent_smart_accounts":         {Delete, "B48.2: each crypto agent's smart account — its address, the owner's passkey public key, the address of the agent's key at the provider and the recovery address"},
	"money_payees":                 {Delete, "B30.16: the people and companies outside Talyvor the workspace saved to pay — names, account details, country — and what their bank said of each name"},
	"money_bills":                  {Delete, "B41.101: the invoices the workspace received, as read — supplier, amount, due date, invoice number, the bank details printed on them — and the saved payee each was matched to"},
	"workspace_provider_keys":      {Delete, "B27.26: the workspace's own provider keys (BYOK), sealed; deleting the row is the end of Talyvor's custody"},
	"webauthn_challenges":          {Delete, ""},
	"workspace_push_subscriptions": {Delete, "B19.16: the owner's devices' push endpoints and keys"},
	"annotator_stakes":             {Delete, ""},
	"api_keys":                     {Delete, ""},
	"batch_jobs":                   {Delete, ""},
	"billing_customers":            {Delete, ""},
	"budgets":                      {Delete, ""},
	"cache_nodes":                  {Delete, ""},
	"compression_measurements":     {Delete, "per-request prompt lengths for one workspace; a byte count is not content, but it is still that customer's traffic"},
	"embedding_nodes":              {Delete, ""},
	"eval_datasets":                {Delete, ""},
	"eval_runs":                    {Delete, ""},
	"eval_schedules":               {Delete, ""},
	"eval_test_cases":              {Delete, ""},
	"experiments":                  {Delete, ""},
	"guardrail_events":             {Delete, ""},
	"guardrail_policies":           {Delete, ""},
	"inference_nodes":              {Delete, ""},
	"k4_mechanical_verdicts":       {Delete, ""},
	"k4_output_verdicts":           {Delete, ""},
	"keel_findings":                {Delete, ""},
	"lens_shadow_mints":            {Delete, ""},
	"lens_token_balances":          {Delete, ""},
	"lxc_balances":                 {Delete, ""},
	"lxc_purchases":                {Delete, ""},
	// W4.6.1 step 1. Classified DELETE for the same reason `lxc_purchases` is: the
	// authoritative record of what was charged lives in STRIPE, which has its own
	// retention and its own erasure obligations. These two tables are Lens's local
	// projection of that — a workspace's subscription state and the events that moved
	// it — so deleting them on erasure removes the customer's data here without
	// destroying the financial record anyone would actually audit.
	// W4.6.1 step 2 — the Model 2 allowance. Same reasoning as the two rows below:
	// Stripe holds the authoritative billing record, this is Lens's local grant-and-
	// consumption projection of it, so erasure removes the customer's data here without
	// destroying anything an audit would reach for.
	"subscription_allowance":    {Delete, ""},
	"subscription_plan_changes": {Delete, "B18.14: each mid-period plan change and the allowance it moved"},
	// B13.2 — which renewal invoice a workspace's earnings were credited against. Lens's projection of a
	// line on a Stripe invoice (the authoritative record); the LENS it consumed is in lens_token_ledger.
	"subscription_bill_credits":     {Delete, ""},
	"subscription_events":           {Delete, ""},
	"subscriptions":                 {Delete, ""},
	"workspace_contracts":           {Delete, "B32.10: an Enterprise contract's plan and fees; the contract itself and its invoices are the operator's"},
	"workspace_rate_limits":         {Delete, "B39.9: the request limits an operator gave the workspace"},
	"lxc_reservations":              {Delete, ""},
	"output_attributions":           {Delete, ""},
	"pattern_mine_credits":          {Delete, ""},
	"povi_challenges":               {Delete, ""},
	"povi_stakes":                   {Delete, ""},
	"prompt_embeddings":             {Delete, "the cached ANSWERS — the most sensitive thing held"},
	"prompts":                       {Delete, ""},
	"provenance_bonds":              {Delete, ""},
	"quality_scores":                {Delete, ""},
	"routing_brain_autonomous":      {Delete, ""},
	"routing_brain_recommendations": {Delete, ""},
	"routing_decisions":             {Delete, ""},
	"routing_patterns":              {Delete, ""},
	"routing_predictions":           {Delete, ""},
	"served_request_measurements":   {Delete, ""},
	"session_keys":                  {Delete, ""},
	"sessions":                      {Delete, ""},
	"stake_positions":               {Delete, ""},
	"traffic_mint_holds":            {Delete, ""},
	"work_tier_observations":        {Delete, ""},
	"workspace_api_keys":            {Delete, ""},
	"workspace_card_fingerprints":   {Delete, ""},
	// W1.9.1. Each row names a workspace and two of its keys. The keys themselves are Delete, so
	// retaining pointers to deleted keys would preserve no audit anybody could read while still
	// naming the customer.
	"workspace_key_rotations": {Delete, ""},
	"tare_training_switches":  {Delete, "B27.36: who switched the workspace's Tare training opt-in, and when"},
	"tare_training_traces":    {Delete, "B27.36: the workspace's prose collected for training Tare's compressor"},
	"workspace_owner_links":   {Delete, ""},
	"workspace_pattern_optin": {Delete, ""},
	"workspaces":              {Delete, "the mapping itself — deleting this is what makes the retained ledger rows unlinkable"},
}

// DeleteOrder returns the tables to delete, parents only, in an order safe to run top to bottom.
// Ordering is deliberate rather than alphabetical: balances and reservations reference the same
// workspace as the rows that produced them, and the workspace row goes LAST so an interrupted run
// leaves the mapping in place and the procedure can be restarted.
func DeleteOrder() []string {
	out := make([]string, 0, len(Manifest))
	for t, e := range Manifest {
		if e.Disposition == Delete && t != "workspaces" {
			out = append(out, t)
		}
	}
	sortStrings(out)
	return append(out, "workspaces")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
