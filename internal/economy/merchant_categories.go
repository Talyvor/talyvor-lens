package economy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// merchant_categories.go — B61.43: THE MERCHANT CATEGORIES AN AGENT'S RULES NAME.
//
// An agent's rules may name the merchant categories it pays in only (AllowedMerchantCategories) and those it never
// pays in (BlockedMerchantCategories), as the rules screen's "Merchants and categories" row does ("Software, hosting,
// freight"). Each is one of MerchantCategories, by its id, or one merchant category code (ISO 18245, four digits). A
// card payment is judged by the category code its issuer reports, inside the payment's movement with the agent's
// other rules, before any money moves.

// MerchantCategory is a kind of merchant the rules name in words: the merchant category codes it covers, one code
// or a range "from-to" each.
type MerchantCategory struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Codes []string `json:"codes"`
}

// MerchantCategories are the categories an agent's rules name by id, in the order a screen lists them.
var MerchantCategories = []MerchantCategory{
	{ID: "software", Name: "Software", Codes: []string{"5734", "5817", "5818", "7372"}},
	{ID: "hosting", Name: "Hosting", Codes: []string{"4816", "7375", "7379"}},
	{ID: "freight", Name: "Freight", Codes: []string{"4214", "4215", "4225"}},
	{ID: "advertising", Name: "Advertising", Codes: []string{"7311", "7319"}},
	{ID: "travel", Name: "Travel", Codes: []string{"3000-3999", "4111", "4112", "4121", "4131", "4411", "4511", "4722", "7011", "7512"}},
	{ID: "telecoms", Name: "Telecoms", Codes: []string{"4812", "4814"}},
	{ID: "gambling", Name: "Gambling", Codes: GamblingMCCs},
}

// merchantCategoryCode is whether s is a merchant category code: four digits.
func merchantCategoryCode(s string) bool {
	return len(s) == 4 && strings.Trim(s, "0123456789") == ""
}

// merchantCategoryByID is the category id names.
func merchantCategoryByID(id string) (MerchantCategory, bool) {
	i := slices.IndexFunc(MerchantCategories, func(c MerchantCategory) bool { return c.ID == id })
	if i < 0 {
		return MerchantCategory{}, false
	}
	return MerchantCategories[i], true
}

// covers is whether c holds the merchant category code mcc.
func (c MerchantCategory) covers(mcc string) bool {
	n, err := strconv.Atoi(mcc)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(c.Codes, func(code string) bool {
		from, until, ranged := strings.Cut(code, "-")
		if !ranged {
			return code == mcc
		}
		lo, err1 := strconv.Atoi(from)
		hi, err2 := strconv.Atoi(until)
		return err1 == nil && err2 == nil && lo <= n && n <= hi
	})
}

// merchantCategoriesName is whether any of names — category ids or codes, as the rules hold them — holds mcc.
func merchantCategoriesName(names []string, mcc string) bool {
	return slices.ContainsFunc(names, func(name string) bool {
		if c, ok := merchantCategoryByID(name); ok {
			return c.covers(mcc)
		}
		return name == mcc
	})
}

// validMerchantCategories checks that each of names is a category id or a merchant category code.
func validMerchantCategories(names []string) error {
	for _, name := range names {
		if _, ok := merchantCategoryByID(name); !ok && !merchantCategoryCode(name) {
			return fmt.Errorf("%q is not a merchant category: name one of %s, or a four-digit merchant category code", name, merchantCategoryIDs())
		}
	}
	return nil
}

func merchantCategoryIDs() string {
	ids := make([]string, len(MerchantCategories))
	for i, c := range MerchantCategories {
		ids[i] = c.ID
	}
	return strings.Join(ids, ", ")
}

// judgeMerchantCategory judges a card payment at merchant, whose issuer reported the category code mcc ("" or
// "unknown" when it did not), by r's merchant categories. A payment whose category is unknown is refused once the rules
// name any category, so an issuer leaving it out cannot step around them.
func (r AgentRules) judgeMerchantCategory(merchant, mcc string) error {
	if len(r.AllowedMerchantCategories)+len(r.BlockedMerchantCategories) == 0 {
		return nil
	}
	if !merchantCategoryCode(mcc) {
		return ruleRefusal("the merchant category of %s is not known, so the payment is outside the agent's merchants and categories", merchant)
	}
	if merchantCategoriesName(r.BlockedMerchantCategories, mcc) ||
		len(r.AllowedMerchantCategories) > 0 && !merchantCategoriesName(r.AllowedMerchantCategories, mcc) {
		return ruleRefusal("%s (merchant category %s) is outside the agent's merchants and categories", merchant, mcc)
	}
	return nil
}

// merchantCategoryRefusal is why agentID's rules in force at now refuse a card payment at merchant in the category mcc,
// or "": the charge on a one-time card, which no other rule judges as it is made.
func merchantCategoryRefusal(ctx context.Context, tx pgx.Tx, agentID, merchant, mcc string, now time.Time) (string, error) {
	r, err := agentRulesInForce(ctx, tx, agentID, now)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("economy: agent rules: %w", err)
	}
	if err := r.judgeMerchantCategory(merchant, mcc); err != nil {
		return strings.TrimPrefix(err.Error(), ErrAgentRule.Error()+": "), nil
	}
	return "", nil
}
