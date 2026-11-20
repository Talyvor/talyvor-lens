// Package partners is every outside service money moves through, behind one partner-neutral interface each.
//
// B30.3. Lens never talks to a bank, broker, card network, insurer or verification service directly: it asks the
// interface here, and the Registry decides which implementation answers. Each interface has a Test
// implementation that moves no real money and answers deterministically, as Stripe's test cards do:
//
//   - an amount whose minor units end in 13 fails;
//   - an amount whose minor units end in 14 succeeds, then comes back (a payment returned, a fill busted, a claim
//     clawed back): the call reports it done, and every later status reports it returned;
//   - an amount whose minor units end in 15 stays pending, however often it is asked after;
//   - any other amount succeeds.
//
// What has no amount (opening an account, a verification check, an agent token) answers by the name it is given:
// a name containing TESTFAIL fails, TESTRETURN succeeds and is later withdrawn, TESTPENDING stays pending. A name
// containing TESTSANCTION screens as a hit, and fails a verification check.
//
// The Registry hands out the Test implementation unless the capability the money moves for has a live clearance
// AND a real adapter is configured — and there is no real adapter yet. Follows economy.CashOutPartner (B22.9).
//
// A Test implementation keeps what it was asked in memory, so a status or statement read from the same one sees
// it. Every call that moves money takes the caller's ID and is idempotent on it: asked again, it does nothing new
// and answers as it did the first time.
package partners

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Money is an amount in a currency's minor units — pence, cents, or millionths of a USDC (economy.MoneyCurrencies).
// Never a float.
type Money struct {
	Minor    int64  `json:"amount_minor"`
	Currency string `json:"currency"`
}

func (m Money) String() string { return fmt.Sprintf("%d %s minor units", m.Minor, m.Currency) }

// Status is where an operation with a partner stands.
type Status string

// The statuses every partner reports.
const (
	StatusPending   Status = "pending"   // under way; not done yet
	StatusCompleted Status = "completed" // done: the money moved, the order filled, the check passed
	StatusFailed    Status = "failed"    // refused or declined: nothing moved
	StatusReturned  Status = "returned"  // done, and then undone by the partner: the money came back
)

// Result is a partner's answer about one operation: its reference with the partner, where it stands, and why.
type Result struct {
	Ref    string `json:"partner_ref"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Link is where a person completes the operation themselves, when the partner needs them to: a verification's
	// one-time link (B30.112). Lens never stores it.
	Link string `json:"link,omitempty"`
	// PartnerStatus is the partner's own word for where it stands (Persona's inquiry status), kept for support only:
	// it never reaches a person (B65.1).
	PartnerStatus string `json:"-"`
}

var (
	// ErrInvalid: a request a partner cannot act on — no ID, no positive amount, an unknown currency.
	ErrInvalid = errors.New("partners: invalid request")
	// ErrNotFound: a reference the partner does not know.
	ErrNotFound = errors.New("partners: the partner has no record of this reference")
	// ErrIDReused: an ID already used for a different request.
	ErrIDReused = errors.New("partners: this id was already used for a different request")
)

// currencies is how many decimal places each currency's minor unit is: the same as economy.MoneyCurrencies.
var currencies = map[string]int{"GBP": 2, "EUR": 2, "USD": 2, "USDC": 6}

// checkMoney refuses an amount that is not positive or not in a currency Talyvor holds.
func checkMoney(m Money) error {
	if _, ok := currencies[m.Currency]; !ok {
		return fmt.Errorf("%w: a currency is GBP, EUR, USD or USDC, not %q", ErrInvalid, m.Currency)
	}
	if m.Minor <= 0 {
		return fmt.Errorf("%w: an amount is positive, not %d", ErrInvalid, m.Minor)
	}
	return nil
}

func checkID(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: every request carries the caller's id", ErrInvalid)
	}
	return nil
}

// testOutcome is what a Test partner does with an amount: the status the call reports, the status every later
// read reports, and why.
func testOutcome(minor int64) (now, later Status, detail string) {
	switch minor % 100 {
	case 13:
		return StatusFailed, StatusFailed, "test mode: amounts ending in 13 fail"
	case 14:
		return StatusCompleted, StatusReturned, "test mode: amounts ending in 14 come back after they are sent"
	case 15:
		return StatusPending, StatusPending, "test mode: amounts ending in 15 stay pending"
	}
	return StatusCompleted, StatusCompleted, ""
}

// testNameOutcome is testOutcome for what has no amount, by the name it is given.
func testNameOutcome(name string) (now, later Status, detail string) {
	n := strings.ToUpper(name)
	switch {
	case strings.Contains(n, "TESTSANCTION"):
		return StatusFailed, StatusFailed, "test mode: names containing TESTSANCTION are on a sanctions list"
	case strings.Contains(n, "TESTFAIL"):
		return StatusFailed, StatusFailed, "test mode: names containing TESTFAIL fail"
	case strings.Contains(n, "TESTRETURN"):
		return StatusCompleted, StatusReturned, "test mode: names containing TESTRETURN are withdrawn after they pass"
	case strings.Contains(n, "TESTPENDING"):
		return StatusPending, StatusPending, "test mode: names containing TESTPENDING stay pending"
	}
	return StatusCompleted, StatusCompleted, ""
}

// testRef is a Test partner's reference for the caller's id: the same id, the same reference, always.
func testRef(kind, id string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + id))
	return "test_" + kind + "_" + hex.EncodeToString(sum[:8])
}

// testDigits is n decimal digits derived from seed, for a Test partner's account numbers.
func testDigits(seed string, n int) string {
	sum := sha256.Sum256([]byte(seed))
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		b.WriteByte('0' + sum[i%len(sum)]%10)
	}
	return b.String()
}

// testOp is one operation a Test partner was asked to do, as it answers for it.
type testOp struct {
	request any // what was asked, to tell a retry from a different request with the same id
	result  Result
	later   Status
	at      time.Time
}

// status is what a read after the call reports.
func (o *testOp) status() Result {
	r := o.result
	r.Status = o.later
	return r
}
