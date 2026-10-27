package compliance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/monitoring"
	"github.com/talyvor/lens/internal/screening"
)

// Export records that by exported case id, and answers the case file as a report draft in plain text, for a person
// to review, complete and file. Nothing is filed or sent.
func (s *Store) Export(ctx context.Context, id, by string) (string, error) {
	by = strings.TrimSpace(by)
	if by == "" {
		return "", fmt.Errorf("%w: an action names the operator who takes it", ErrInvalid)
	}
	if err := s.act(ctx, id, by, ActionExport, func(pgx.Tx, screening.Case) (string, error) {
		return "exported a report draft", nil
	}); err != nil {
		return "", err
	}
	f, err := s.File(ctx, id)
	if err != nil {
		return "", err
	}
	return Report(f, time.Now()), nil
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") }

// Report is f as a report draft, prepared at now.
func Report(f File, now time.Time) string {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	section := func(title string) { line("\n%s\n%s", title, strings.Repeat("-", len(title))) }
	c := f.Case

	line("DRAFT — COMPLIANCE CASE REPORT")
	line("Prepared %s from Talyvor's records, for a person to review, complete and file.", stamp(now))
	line("It has not been filed or sent anywhere.")

	section("Case")
	kind := "Sanctions screening"
	if c.Kind == monitoring.CaseKind {
		kind = "Transaction monitoring"
	}
	line("Case:      %s", c.ID)
	line("Kind:      %s", kind)
	line("Status:    %s", c.Status)
	line("Opened:    %s", stamp(c.OpenedAt))
	if c.DecidedAt != nil {
		line("Decided:   %s by %s — %s", stamp(*c.DecidedAt), c.DecidedBy, orNone(c.DecisionNote))
	}

	section("Subject")
	o := f.Owner
	line("Workspace: %s %s", o.WorkspaceID, paren(o.Name))
	if o.VerifiedName != "" {
		line("Verified:  %s%s%s", o.VerifiedName, prefixed(", ", o.Country), prefixed(", company number ", o.CompanyNumber))
	} else {
		line("Verified:  no completed verification on record")
	}
	if o.TestWorkspace {
		line("This is a test workspace: none of its money is real.")
	}

	section("Agents involved")
	if len(f.Agents) == 0 {
		line("None named by the alerts.")
	}
	for _, a := range f.Agents {
		line("- %s %s — %d alert%s", a.ID, paren(a.Name), a.Alerts, plural(a.Alerts))
	}

	section("Money capabilities")
	if f.Freeze == nil {
		line("Not frozen.")
	} else {
		line("Frozen since %s by %s, on case %s: %s", stamp(f.Freeze.FrozenAt), f.Freeze.FrozenBy, f.Freeze.CaseID, f.Freeze.Reason)
	}

	section("What was found")
	if c.Kind != monitoring.CaseKind {
		line("The %s's name %q was screened%s.", c.SubjectKind, c.Name, money(c))
		for _, m := range c.Matches {
			line("- matched %q on %s (entry %s), %d.%02d%% alike%s", m.Listed, m.List, m.Entry, m.ScoreBPS/100, m.ScoreBPS%100, weak(m.Weak))
		}
	}
	if len(f.Alerts) == 0 && c.Kind == monitoring.CaseKind {
		line("No alerts.")
	}
	for i := len(f.Alerts) - 1; i >= 0; i-- {
		a := f.Alerts[i]
		line("%d. %s  %s — %s", len(f.Alerts)-i, stamp(a.RaisedAt), a.Rule, a.Summary)
		line("   payments: %s (%s money)", strings.Join(a.Entries, ", "), a.Funding)
	}

	section("Notes")
	if len(f.Notes) == 0 {
		line("None.")
	}
	for _, n := range f.Notes {
		line("- %s, %s: %s", stamp(n.At), n.By, n.Text)
	}

	section("Timeline")
	for _, e := range f.Timeline {
		line("%s  %-8s %s%s", stamp(e.At), e.Kind, prefixed("", e.By+": "), e.Text)
	}

	section("To complete before filing")
	line("- The grounds for suspicion, in your own words.")
	line("- What you know of the customer and of the agents' purpose.")
	line("- The reference you file under, and the date you file.")
	return b.String()
}

func money(c screening.Case) string {
	if c.AmountMinor == nil {
		return ""
	}
	dir := "to"
	if c.Direction == "in" {
		dir = "from"
	}
	return fmt.Sprintf(" for a payment %s them of %s (%s money, %s)", dir, economy.FormatMinor(*c.AmountMinor, c.Currency), c.Funding,
		c.Capability)
}

func weak(w bool) string {
	if w {
		return " — on a low-quality alias"
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "no note"
	}
	return s
}

func paren(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}

func prefixed(prefix, s string) string {
	if s == "" || s == ": " {
		return ""
	}
	return prefix + s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
