package main

import (
	"context"
	"encoding/base32"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/moderatorkey"
	"github.com/talyvor/lens/internal/operatoraudit"
	"github.com/talyvor/lens/internal/partnerkey"
	"github.com/talyvor/lens/internal/stepup"
	"github.com/talyvor/lens/internal/tenant"
)

// B62.5 — the account partner's risk team reads every agent payment on its own key: the account and its level, the
// agent, the amount and currency, the rule and rules version, whether the owner approved it and with what key, its
// proof record and its state, newest first, filtered, paged, as a CSV and counted by day. An operator makes and
// revokes the key with the step-up code; every read is an audit row nobody can change; no name, email, phone number,
// address, payee or memo is in any row; and a workspace key, a moderator key, the operator read key and the admin key
// are all refused on the view.
func TestB625_ThePartnerReadsEveryAgentPaymentOnItsOwnKey(t *testing.T) {
	w := newB621World(t, "ws-b625")
	ctx := context.Background()

	// Three agent payments: €50.00 inside the rules, €150.00 the owner approved with the phone key, €120.00 waiting.
	if code := w.pay(t, "k-inside", w.same, 5_000); code != http.StatusOK {
		t.Fatalf("pay €50.00: %d %v", code, w.lastBody)
	}
	if code := w.pay(t, "k-approved", w.same, 15_000); code != http.StatusAccepted {
		t.Fatalf("pay €150.00: %d %v", code, w.lastBody)
	}
	w.approveSigned(t, w.approvalOf["k-approved"])
	if code := w.pay(t, "k-approved", w.same, 15_000); code != http.StatusOK {
		t.Fatalf("pay €150.00 after the owner's yes: %d %v", code, w.lastBody)
	}
	if code := w.pay(t, "k-waiting", w.same, 12_000); code != http.StatusAccepted {
		t.Fatalf("pay €120.00: %d %v", code, w.lastBody)
	}

	// The routes as main.go mounts them, on a real auth.Manager holding every other kind of key.
	partnerKeys, moderators := partnerkey.NewStore(w.pool), moderatorkey.NewStore(w.pool)
	ts := tenant.NewStore(w.pool)
	am := auth.NewManager("b625-admin-key", nil, auth.New(w.pool), ts).WithModeratorKeys(moderators).
		WithOperatorReadKey("b625-operator-read-key").WithPartnerKeys(partnerKeys)
	stepKey := []byte("0123456789abcdef0123")
	stepUp, err := stepup.New(base32.StdEncoding.EncodeToString(stepKey), stepup.NewPostgresClaims(w.pool))
	if err != nil {
		t.Fatal(err)
	}
	operatorAudit := operatoraudit.NewStore(w.pool)
	r := chi.NewRouter()
	r.Get("/v1/partner/agent-activity", requirePartnerRead(am, partnerKeys, newPartnerActivityHandler(w.b.store)))
	r.Get("/v1/partner/agent-activity/export", requirePartnerRead(am, partnerKeys, newPartnerActivityExportHandler(w.b.store)))
	r.Get("/v1/partner/agent-activity/summary", requirePartnerRead(am, partnerKeys, newPartnerActivitySummaryHandler(w.b.store)))
	r.Post("/v1/admin/partner-keys", requireAdminStepUp(am, stepUp, operatorAudit, "partner_key.create", requireAdmin(am, newPartnerKeyCreateHandler(partnerKeys))))
	r.Post("/v1/admin/partner-keys/{id}/revoke", requireAdminStepUp(am, stepUp, operatorAudit, "partner_key.revoke", requireAdmin(am, newPartnerKeyRevokeHandler(partnerKeys))))
	call := func(method, path, key, code string, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		req.Header.Set(moderatorOperatorHeader, "ops-b625")
		if code != "" {
			req.Header.Set(stepUpHeader, code)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	freshCode := func() string {
		t.Helper()
		if _, err := w.pool.Exec(ctx, `DELETE FROM operator_step_up_codes`); err != nil {
			t.Fatal(err)
		}
		return stepup.CodeAt(stepKey, time.Now())
	}

	// The operator makes the key: the admin key alone is not enough, the step-up code is, and it is an audit row.
	if rec := call(http.MethodPost, "/v1/admin/partner-keys", "b625-admin-key", "", `{"name":"Account partner, risk team"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("make a partner key without the step-up code: %d %s, want 403", rec.Code, rec.Body)
	}
	rec := call(http.MethodPost, "/v1/admin/partner-keys", "b625-admin-key", freshCode(), `{"name":"Account partner, risk team"}`)
	var made struct {
		Key        string         `json:"key"`
		PartnerKey partnerkey.Key `json:"partner_key"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &made) != nil || !strings.HasPrefix(made.Key, partnerkey.KeyPrefix) ||
		made.PartnerKey.CreatedBy != "ops-b625" {
		t.Fatalf("make a partner key: %d %s", rec.Code, rec.Body)
	}
	var made1 int
	_ = w.pool.QueryRow(ctx, `SELECT count(*) FROM operator_audit WHERE action = 'partner_key.create' AND actor = 'ops-b625'`).Scan(&made1)
	if made1 != 1 {
		t.Fatalf("operator_audit rows for making the key = %d, want 1", made1)
	}

	reads := 0
	view := func(query string) (int, map[string]json.RawMessage, []map[string]any) {
		t.Helper()
		reads++
		rec := call(http.MethodGet, "/v1/partner/agent-activity"+query, made.Key, "", "")
		var out map[string]json.RawMessage
		var rows []map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		_ = json.Unmarshal(out["payments"], &rows)
		return rec.Code, out, rows
	}

	// Every agent payment, newest first, in Sandbox.
	code, out, rows := view("")
	if code != http.StatusOK || string(out["mode"]) != `"sandbox"` || len(rows) != 3 {
		t.Fatalf("the partner's view = %d %v, want 3 payments in sandbox", code, out)
	}
	waiting, approved, inside := rows[0], rows[1], rows[2]
	if waiting["state"] != economy.PartnerStateWaitingForOwner || waiting["id"] != w.approvalOf["k-waiting"] || waiting["owner_approved"] != false ||
		waiting["proof_id"] != "" || waiting["amount_minor"] != 12_000.0 || waiting["currency"] != "EUR" || waiting["rule"] != economy.ProofRuleApprovalAbove {
		t.Fatalf("the waiting payment = %v", waiting)
	}
	var approvedProof, insideProof string
	_ = w.pool.QueryRow(ctx, `SELECT id FROM payment_proofs WHERE approval_id = $1`, w.approvalOf["k-approved"]).Scan(&approvedProof)
	_ = w.pool.QueryRow(ctx, `SELECT id FROM payment_proofs WHERE agent_id = $1 AND approval_id = ''`, w.payer).Scan(&insideProof)
	if approved["state"] != economy.PartnerStateSent || approved["owner_approved"] != true || approved["approval_key"] != "device" ||
		approved["proof_id"] != approvedProof || approvedProof == "" || approved["amount_minor"] != 15_000.0 || approved["rules_version"] == nil ||
		approved["rule"] != economy.ProofRuleApprovalAbove || approved["account_id"] != w.ws || approved["agent_id"] != w.payer {
		t.Fatalf("the approved payment = %v, want sent, approved by the owner's phone key, its proof %s", approved, approvedProof)
	}
	if inside["state"] != economy.PartnerStateSent || inside["owner_approved"] != false || inside["approval_key"] != "" ||
		inside["proof_id"] != insideProof || inside["rule"] != economy.ProofRuleWithinRules || inside["amount_minor"] != 5_000.0 {
		t.Fatalf("the payment inside the rules = %v, want sent within the rules, its proof %s", inside, insideProof)
	}
	for _, row := range rows {
		if !slices.Contains([]string{"L0", "L1", "L2", "L3"}, row["verification_level"].(string)) {
			t.Fatalf("verification_level = %v", row["verification_level"])
		}
	}

	// No personal field is in any row: the fields are exactly the list, and no name, payee, memo or email is anywhere.
	for _, row := range rows {
		keys := make([]string, 0, len(row))
		for k := range row {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		want := slices.Clone(economy.PartnerPaymentFields)
		sort.Strings(want)
		if !slices.Equal(keys, want) {
			t.Fatalf("a row's fields = %v, want exactly %v", keys, want)
		}
	}
	var names []string
	agentNames, _ := w.pool.Query(ctx, `SELECT name FROM agent_accounts WHERE workspace_id = ANY($1)`, []string{w.ws, w.other})
	for agentNames.Next() {
		var n string
		_ = agentNames.Scan(&n)
		names = append(names, n)
	}
	agentNames.Close()
	verified, _ := w.pool.Query(ctx, `SELECT verified_name FROM workspace_verifications WHERE workspace_id = $1 AND verified_name <> ''`, w.ws)
	for verified.Next() {
		var n string
		_ = verified.Scan(&n)
		names = append(names, n)
	}
	verified.Close()
	if len(names) < 3 {
		t.Fatalf("names to look for = %v, want the agents' and the verified owner's", names)
	}
	body := call(http.MethodGet, "/v1/partner/agent-activity", made.Key, "", "").Body.String() +
		call(http.MethodGet, "/v1/partner/agent-activity/export", made.Key, "", "").Body.String()
	reads += 2
	for _, personal := range append(names, "Grace", "Hopper", "DE89370400440532013000", "Invoice", "@", "owner-") {
		if personal != "" && strings.Contains(body, personal) {
			t.Fatalf("the partner's view holds %q:\n%s", personal, body)
		}
	}

	// Filters and pages.
	if _, _, sent := view("?state=sent"); len(sent) != 2 {
		t.Fatalf("state=sent: %d rows, want 2", len(sent))
	}
	if _, _, none := view("?account=" + w.other); len(none) != 0 {
		t.Fatalf("account=%s: %d rows, want 0", w.other, len(none))
	}
	if _, _, none := view("?from=" + time.Now().UTC().AddDate(0, 0, 1).Format(time.DateOnly)); len(none) != 0 {
		t.Fatalf("from tomorrow: %d rows, want 0", len(none))
	}
	if _, _, today := view("?from=" + time.Now().UTC().Format(time.DateOnly) + "&to=" + time.Now().UTC().Format(time.DateOnly)); len(today) != 3 {
		t.Fatalf("from today to today: %d rows, want 3", len(today))
	}
	if code, out, _ := view("?state=lost"); code != http.StatusBadRequest {
		t.Fatalf("state=lost: %d %v, want 400", code, out)
	}
	var paged []any
	cursor := ""
	for i := 0; i < 4; i++ {
		_, out, page := view("?limit=1&cursor=" + cursor)
		for _, p := range page {
			paged = append(paged, p["id"])
		}
		_ = json.Unmarshal(out["next_cursor"], &cursor)
		if cursor == "" {
			break
		}
	}
	if len(paged) != 3 || paged[0] != rows[0]["id"] || paged[1] != rows[1]["id"] || paged[2] != rows[2]["id"] {
		t.Fatalf("page by page = %v, want the three payments in order", paged)
	}

	// The CSV: the field list as its header, one line per payment.
	rec = call(http.MethodGet, "/v1/partner/agent-activity/export", made.Key, "", "")
	reads++
	lines, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if rec.Code != http.StatusOK || err != nil || len(lines) != 4 || !slices.Equal(lines[0], economy.PartnerPaymentFields) ||
		lines[2][11] != approvedProof || lines[2][12] != "sent" || lines[2][10] != "device" {
		t.Fatalf("the CSV = %d %v %v", rec.Code, err, lines)
	}

	// Counts by day.
	rec = call(http.MethodGet, "/v1/partner/agent-activity/summary", made.Key, "", "")
	reads++
	var summary struct {
		Mode string               `json:"mode"`
		Days []economy.PartnerDay `json:"days"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &summary) != nil || summary.Mode != "sandbox" || len(summary.Days) != 1 ||
		summary.Days[0].Count != 3 || summary.Days[0].ByState["sent"] != 2 || summary.Days[0].ByState["waiting_for_owner"] != 1 {
		t.Fatalf("the summary = %d %s", rec.Code, rec.Body)
	}

	// Each read is an audit row, and the rows cannot be changed.
	var uses int
	_ = w.pool.QueryRow(ctx, `SELECT count(*) FROM partner_key_uses WHERE key_id = $1`, made.PartnerKey.ID).Scan(&uses)
	if uses != reads {
		t.Fatalf("partner_key_uses = %d rows, want one per read: %d", uses, reads)
	}
	for _, sql := range []string{`UPDATE partner_key_uses SET path = '/' WHERE key_id = $1`, `DELETE FROM partner_key_uses WHERE key_id = $1`} {
		if _, err := w.pool.Exec(ctx, sql, made.PartnerKey.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s: %v, want refused as append-only", sql, err)
		}
	}

	// Every other kind of key is refused on the view, and a read refused is not shown.
	wsKey, _, err := ts.CreateAPIKey(ctx, w.ws, "a workspace key", []string{"proxy", "analytics", "admin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	modKey, _, err := moderators.Create(ctx, "a moderator", "ops-b625")
	if err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string]string{"a workspace key": wsKey, "a moderator key": modKey,
		"the operator read key": "b625-operator-read-key", "the admin key": "b625-admin-key"} {
		for _, path := range auth.PartnerRoutes {
			if rec := call(http.MethodGet, path, key, "", ""); rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), w.payer) {
				t.Errorf("%s reads %s: %d %s, want 403", name, path, rec.Code, rec.Body)
			}
		}
	}
	if rec := call(http.MethodGet, "/v1/partner/agent-activity", "", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d, want 401", rec.Code)
	}

	// The operator revokes it, with the step-up code: the key is refused on the next read.
	if rec := call(http.MethodPost, "/v1/admin/partner-keys/"+jsonInt(made.PartnerKey.ID)+"/revoke", "b625-admin-key", freshCode(), ""); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if rec := call(http.MethodGet, "/v1/partner/agent-activity", made.Key, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the revoked key reads the view: %d %s, want 401", rec.Code, rec.Body)
	}
	var revoked1 int
	_ = w.pool.QueryRow(ctx, `SELECT count(*) FROM operator_audit WHERE action = 'partner_key.revoke' AND actor = 'ops-b625'`).Scan(&revoked1)
	if revoked1 != 1 {
		t.Fatalf("operator_audit rows for revoking the key = %d, want 1", revoked1)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
