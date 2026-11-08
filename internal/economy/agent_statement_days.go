package economy

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// agent_statement_days.go — B57.13: a statement read by model and day, and paged.
//
// Read by day, an account's questions to one model on one day are one line — the model, how many questions, what they
// cost net of what their settles and releases gave back — and their platform fees the line under it. Every other
// posting stays a line of its own. The accounts' opening, in, out and closing are the statement's, unchanged, and the
// lines sum to what its postings do. The CSV is never grouped: it keeps one row per posting, the audit file.

// OnThoseRequests is the memo of the fee line under a model's day.
const OnThoseRequests = "On those requests"

// ErrStatementCursor is a cursor no page of a statement gave.
var ErrStatementCursor = errors.New("economy: cursor is not one a statement page gave")

// questionKinds are the postings of a question to a model that its day's line sums; its fee is the line under it.
var questionKinds = map[string]bool{"spend": true, "hold": true, "settle": true, "release": true}

// ByDay is st with its questions to each model summed per account, model and day, in loc (named zone), newest first.
func (st Statement) ByDay(loc *time.Location, zone string) Statement {
	type key struct {
		account, model, day string
		fee                 bool
	}
	at := map[key]int{}
	refs := map[key]map[string]bool{}
	lines := []StatementLine{}
	for _, l := range st.Lines {
		fee := l.Kind == LXCTypePlatformFee
		if l.Model == "" || !fee && !questionKinds[l.Kind] {
			lines = append(lines, l)
			continue
		}
		k := key{l.Account, l.Model, l.At.In(loc).Format(time.DateOnly), fee}
		i, ok := at[k]
		if !ok {
			g := StatementLine{Account: l.Account, Kind: "spend", Counterparty: l.Counterparty, Model: l.Model, Day: k.day}
			if fee {
				g.Kind, g.Label, g.Memo = LXCTypePlatformFee, l.Label, OnThoseRequests
			}
			i, at[k], refs[k] = len(lines), len(lines), map[string]bool{}
			lines = append(lines, g)
		}
		g := &lines[i]
		g.PostingID, g.At, g.BalanceAfterULXC = l.PostingID, l.At, l.BalanceAfterULXC
		g.AmountULXC += l.AmountULXC
		g.Postings++
		if fee && g.Label != l.Label {
			g.Label = "Platform fee" // the rate changed during the day
		}
		// A question is its hold or its debit; a debit's later under-charge settle is a 'spend' of the same ref.
		if (l.Kind == "hold" || l.Kind == "spend") && (l.Ref == "" || !refs[k][l.Ref]) {
			refs[k][l.Ref] = true
			g.Requests++
		}
	}
	// Newest first; a fee line sorts as its model's line does, just under it.
	sortKey := make([]statementKey, len(lines))
	for i, l := range lines {
		sortKey[i] = statementKey{l.At.UnixMicro(), l.PostingID, 0}
		if l.Day != "" && l.Kind == LXCTypePlatformFee {
			if j, ok := at[key{l.Account, l.Model, l.Day, false}]; ok {
				sortKey[i] = statementKey{lines[j].At.UnixMicro(), lines[j].PostingID, 1}
			}
		}
	}
	order := make([]int, len(lines))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return sortKey[order[a]].before(sortKey[order[b]]) })
	st.Lines = make([]StatementLine, len(lines))
	for i, j := range order {
		st.Lines[i] = lines[j]
	}
	st.Group, st.TimeZone = "day", zone
	return st
}

// Page is st's lines newest first, after cursor ("" from the newest), at most limit of them (0 for every one), and the
// cursor of the page after. Its accounts are the whole period's on every page.
func (st Statement) Page(limit int, cursor string) (Statement, error) {
	lines := st.Lines
	if st.Group == "" { // a statement's postings are oldest first
		lines = make([]StatementLine, len(st.Lines))
		for i, l := range st.Lines {
			lines[len(lines)-1-i] = l
		}
	}
	keys := make([]statementKey, len(lines))
	for i, l := range lines {
		keys[i] = statementKey{l.At.UnixMicro(), l.PostingID, 0}
		if p := i - 1; p >= 0 && l.Day != "" && l.Kind == LXCTypePlatformFee && lines[p].Kind != LXCTypePlatformFee &&
			lines[p].Day == l.Day && lines[p].Model == l.Model && lines[p].Account == l.Account {
			keys[i] = statementKey{keys[p].at, keys[p].posting, 1}
		}
	}
	start := 0
	if cursor != "" {
		var c statementKey
		if n, err := fmt.Sscanf(cursor, "%d.%d.%d", &c.at, &c.posting, &c.sub); err != nil || n != 3 || c.String() != cursor {
			return st, ErrStatementCursor
		}
		for start < len(lines) && !c.before(keys[start]) {
			start++
		}
	}
	end := len(lines)
	if limit > 0 && start+limit < end {
		end = start + limit
		st.NextCursor = keys[end-1].String()
	}
	st.Lines = lines[start:end]
	return st, nil
}

// statementKey orders a statement's lines newest first: by time, then posting, then a fee line under its model's.
type statementKey struct {
	at, posting int64
	sub         int
}

func (k statementKey) before(o statementKey) bool {
	if k.at != o.at {
		return k.at > o.at
	}
	if k.posting != o.posting {
		return k.posting > o.posting
	}
	return k.sub < o.sub
}

func (k statementKey) String() string { return fmt.Sprintf("%d.%d.%d", k.at, k.posting, k.sub) }
