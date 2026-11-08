// Package earnings answers one workspace-scoped question: what has this workspace EARNED, and how
// much of that is real yet? (W4.6.1 step 7 — the surface behind "your plan is $20, your answers
// earned $6 of it back".)
//
// ⚠ THE LEDGER IS THE SOURCE, NOT THE SIX CLAIM TABLES. Six tables carry a
// contributor_workspace_id — pool_royalty_mints, distill_royalty_mints, eval_contribution_mints,
// routing_prediction_mints, node_latency_mints, confidential_compute_mints — and every one of them
// records what a mint INTENDED. What a workspace was PAID is `lens_token_ledger`, and the two can
// disagree: config.go's ReputationBondedMintingEnabled comment records a measured case where the
// bond reduces the held CREDIT while the claim row keeps the unreduced base. A customer-facing
// figure read off the claim tables would state the larger of two numbers. One query on the ledger,
// scoped by workspace_id, cannot drift from what moved.
//
// ⚠ AND THE TYPE VOCABULARY IS FRAGMENTED ACROSS THREE PACKAGES IN TWO REPRESENTATIONS, WHICH IS
// WHY THIS FILE IS A MAP AND NOT A LIST. Measured 2026-08-26: `internal/mining` declares 35
// exported Type* constants, `internal/povi` declares one more (and `internal/mining/mint_gate.go`
// already carries a cycle-free duplicate literal of it, saying so in a comment), and
// `internal/economy` writes SEVEN ledger types as BARE STRING LITERALS with no constant at all
// (marketplace_buy, marketplace_fee, marketplace_unsold_refund, marketplace_listing,
// marketplace_refund, stake, unstake). No static census over constants can see any of them.
//
// ⚠ AND THE COUNT WAS FIVE UNTIL A DERIVED WALK CORRECTED IT, WHICH IS THE WHOLE ARGUMENT OF THIS
// FILE MADE AT MY OWN EXPENSE. The first version of this map carried five, taken from a grep for
// the type argument on the same line as the ledger call. `marketplace_listing` and
// `marketplace_refund` pass their type on the FOLLOWING line, so the grep could not see them — a
// 29% undercount in a census I had just written a paragraph about being careful with. The AST walk
// in TestE2 found both. A hand-built population is wrong in the direction nobody checks. So this package guards itself twice:
//
//   - STATICALLY, over the two packages that DO use constants (classification_census_test.go), so a
//     new mint type added the normal way must be classified or the build goes red; and
//   - AT RUNTIME, via the Unclassified bucket below, which reports any type actually present in a
//     workspace's ledger that this map does not know. That is the half that covers the literals.
//
// A silent "0" is the failure mode this package exists to avoid, so nothing is dropped anywhere: a
// type this file has never heard of is REPORTED, not skipped.
package earnings

// Class is what a ledger row means for an earnings figure.
type Class string

const (
	// Settled — minted, finalised, in circulation. This is money the workspace has.
	Settled Class = "settled"
	// Held — minted but not yet settled. Revocable by adjudication, so it is NOT earned yet and is
	// reported on its own line rather than folded into a total.
	Held Class = "held"
	// Revoked — a held mint that was clawed back. Reported so a drop in earnings has a name.
	Revoked Class = "revoked"
	// NotEarnings — a real ledger row that is not contribution income (spends, transfers, burns,
	// stake movements, marketplace transfers). Classified explicitly so "not counted" is a decision
	// with a reason attached rather than an omission.
	NotEarnings Class = "not_earnings"
	// Unclassified — a type present in the ledger that this file does not know. Never silently
	// dropped; surfaced so the gap is visible the first time it costs someone a number.
	Unclassified Class = "unclassified"
)

// Kind separates WHAT a settled earning came from. "Your answers earned $6 back" is a claim about
// CONTRIBUTION, and staking yield is not an answer anybody wrote — folding it in would make the
// sentence false while the total stayed right.
type Kind string

const (
	// Contribution — earned by contributing something others reused: a cached answer, a distilled
	// document, an evaluation, a routing prediction, compute.
	Contribution Kind = "contribution"
	// Capital — earned by holding or locking LENS rather than by contributing. Counted in the total,
	// excluded from the contribution subtotal, and always broken out by type.
	Capital Kind = "capital"
	// NotIncome — the kind of a row that is not income at all.
	NotIncome Kind = "not_income"
)

// Rule is one ledger type's classification and the reason for it. The reason is not decoration: a
// future reader deciding whether a new type belongs in "your answers earned" needs the argument,
// not the verdict.
type Rule struct {
	Class  Class
	Kind   Kind
	Reason string
}

// rules is the authoritative vocabulary. Its completeness over the CONSTANT-declared types is
// guarded by classification_census_test.go; its completeness over everything else is guarded at
// runtime by the Unclassified bucket.
var rules = map[string]Rule{
	// ── settled contribution income: the eleven counted-supply types, minus capital ────────────
	"cache_mine":                {Settled, Contribution, "a cached answer this workspace contributed was reused and the mint settled"},
	"compute_mine":              {Settled, Contribution, "compute this workspace supplied was used and the mint settled"},
	"embedding_mine":            {Settled, Contribution, "an embedding this workspace contributed was reused"},
	"annotation_mine":           {Settled, Contribution, "an annotation this workspace supplied was used"},
	"pattern_mine":              {Settled, Contribution, "a prompt pattern this workspace contributed was reused"},
	"pool_royalty":              {Settled, Contribution, "THE headline case: a cross-tenant pooled cache/distill hit on this workspace's answer, settled"},
	"eval_contribution":         {Settled, Contribution, "an evaluation this workspace contributed, settled"},
	"eval_routing_prediction":   {Settled, Contribution, "a routing prediction this workspace made proved right, settled"},
	"eval_latency_locality":     {Settled, Contribution, "latency/locality this workspace provided, settled"},
	"eval_confidential_compute": {Settled, Contribution, "confidential compute this workspace attested, settled"},

	// ── settled, but capital rather than contribution ──────────────────────────────────────────
	"stake_yield": {Settled, Capital, "yield on LOCKED LENS. In counted supply and credited to the workspace, so it is real income — but nobody wrote an answer for it, so it must not enter a 'your answers earned' subtotal. ⚠ W6.3.3 has this mint under an open economic and legal question; this package reports it and takes no view"},

	// ── held: minted, not settled, still revocable ─────────────────────────────────────────────
	"cache_mine_held":                {Held, Contribution, "held cache mint, not yet finalised"},
	"compute_mine_held":              {Held, Contribution, "held compute mint, not yet finalised"},
	"embedding_mine_held":            {Held, Contribution, "held embedding mint, not yet finalised"},
	"pattern_mine_held":              {Held, Contribution, "held pattern mint, not yet finalised"},
	"pool_royalty_held":              {Held, Contribution, "held pool royalty — an adjudicator can still revoke this, so it is not earned"},
	"eval_contribution_held":         {Held, Contribution, "held eval-contribution mint"},
	"eval_routing_prediction_held":   {Held, Contribution, "held routing-prediction mint"},
	"eval_latency_locality_held":     {Held, Contribution, "held latency/locality mint"},
	"eval_confidential_compute_held": {Held, Contribution, "held confidential-compute mint"},

	// ── revoked: clawed back after adjudication ────────────────────────────────────────────────
	"pool_royalty_revoked":              {Revoked, Contribution, "a held pool royalty was adjudicated and burned"},
	"eval_contribution_revoked":         {Revoked, Contribution, "a held eval-contribution mint was revoked"},
	"eval_routing_prediction_revoked":   {Revoked, Contribution, "a held routing-prediction mint was revoked"},
	"eval_latency_locality_revoked":     {Revoked, Contribution, "a held latency/locality mint was revoked"},
	"eval_confidential_compute_revoked": {Revoked, Contribution, "a held confidential-compute mint was revoked"},
	"traffic_mint_revoked":              {Revoked, Contribution, "a held traffic mint was revoked"},

	// ── not income ─────────────────────────────────────────────────────────────────────────────
	"spend":                    {NotEarnings, NotIncome, "the workspace spending its own LENS"},
	"transfer":                 {NotEarnings, NotIncome, "moves existing LENS; mints nothing. Counting it would let a workspace inflate its own earnings by transferring to itself"},
	"transfer_in":              {NotEarnings, NotIncome, "the credit half of a transfer — existing LENS, not earned"},
	"transfer_out":             {NotEarnings, NotIncome, "the debit half of a transfer"},
	"burn":                     {NotEarnings, NotIncome, "destroys LENS outright — a burn is the opposite of income"},
	"convert_to_lxc":           {NotEarnings, NotIncome, "LENS→LXC conversion debit; the value is not lost but it is not income"},
	"povi_stake_lock":          {NotEarnings, NotIncome, "PoVI stake locked — the workspace's own LENS, immobilised"},
	"povi_stake_release":       {NotEarnings, NotIncome, "PoVI stake returned — the same LENS coming back"},
	"povi_stake_slash":         {NotEarnings, NotIncome, "PoVI stake slashed — a penalty, not income"},
	"receipt_mine_provisional": {NotEarnings, NotIncome, "PoVI provisional receipt mint. Deliberately OUTSIDE counted supply per its own go-live treatment (internal/mining/cache_mining.go), so reporting it as earned would credit a workspace with LENS the supply figure says does not exist"},
	// ⚠ the five below have NO Go constant anywhere — internal/economy writes them as bare string
	// literals, so the static census cannot see them and they are here by hand. The Unclassified
	// bucket is what catches a SIXTH.
	"marketplace_buy":           {NotEarnings, NotIncome, "LENS bought on the marketplace — a transfer of existing LENS, and W6.3.2 records that this path takes no payment at all"},
	"marketplace_fee":           {NotEarnings, NotIncome, "Talyvor's marketplace fee; not the workspace's income"},
	"compute_fee":               {NotEarnings, NotIncome, "Talyvor's compute fee on a node earning (B56.20); not the workspace's income"},
	"compute_fee_held":          {NotEarnings, NotIncome, "Talyvor's compute fee while the node mint it came from is held"},
	"marketplace_unsold_refund": {NotEarnings, NotIncome, "escrow returned on an unsold listing — the seller's own LENS coming back"},
	"stake":                     {NotEarnings, NotIncome, "principal locked into a stake position"},
	"marketplace_listing":       {NotEarnings, NotIncome, "escrow debit when a seller lists LENS — the workspace's own LENS, immobilised, not income"},
	"marketplace_refund":        {NotEarnings, NotIncome, "escrow returned when a listing is cancelled — the seller's own LENS coming back"},
	"unstake":                   {NotEarnings, NotIncome, "principal returned. Deliberately uncounted in supply because it was already in supply when staked; the newly-created half is stake_yield"},
}

// Classify returns the rule for a ledger type. An unknown type is Unclassified rather than assumed
// harmless — the caller reports it.
func Classify(ledgerType string) Rule {
	if r, ok := rules[ledgerType]; ok {
		return r
	}
	return Rule{Class: Unclassified, Kind: NotIncome, Reason: "this ledger type is not in internal/earnings' vocabulary"}
}

// ClassifiedTypes returns every ledger type this package knows, for the census and for callers that
// want to state what they summed.
func ClassifiedTypes() []string {
	out := make([]string, 0, len(rules))
	for t := range rules {
		out = append(out, t)
	}
	return out
}
