# Compliance starting values — for Nicolai to review

Every threshold below is a setting Nicolai fills. Until he does, Lens uses the starting value listed here, which is
also written in the rule's own file. Set a value in `lens.env` (see `lens.env.example`), never under
docker-compose `environment:`.

## Transaction monitoring (B30.7)

Five rules judge every payment in or out of a workspace through a partner — when it is posted, and again nightly over
the last day's payments. A hit opens the workspace's monitoring case, or adds an alert to the one already open. A hit
never moves money and never stops it: a person reviews the case. The operator sees the cases at
`GET /v1/admin/screening` (kind `monitoring`, status `open`) and each alert at `GET /v1/admin/compliance/alerts`.

Sandbox funds and live money are judged apart. A rule's setting is a JSON object; a value it leaves out keeps the
starting value.

| Rule | Setting | Starting value | What it means | File |
|---|---|---|---|---|
| Several payments just under an agent's approval amount | `LENS_MONITOR_UNDER_APPROVAL` | `{"payments":3,"within_hours":24,"under_percent":10}` | 3 payments out by one agent within 24 hours, each at most its approval amount and no more than 10% under it. Pounds and euros are compared at the ECB reference rate of the payment's day. An agent without an approval amount is not judged by it. | `internal/monitoring/rule_under_approval.go` |
| Money in and straight out | `LENS_MONITOR_IN_AND_OUT` | `{"within_minutes":60,"out_percent":50}` | Half or more of a payment in leaves again, in payments out in the same currency, within 60 minutes of arriving. | `internal/monitoring/rule_in_and_out.go` |
| A first payment to a new payee well above the usual size | `LENS_MONITOR_NEW_PAYEE_LARGE` | `{"times_usual":3,"from_payments":3}` | The first payment to a payee is at least 3 times the median of the agent's earlier payments out in that currency (the company's, for its own money), once there are 3 of them. | `internal/monitoring/rule_new_payee_large.go` |
| A burst of round amounts | `LENS_MONITOR_ROUND_AMOUNTS` | `{"payments":3,"within_minutes":60,"round_to":100}` | 3 payments out within 60 minutes, each a whole multiple of 100 in its currency (£100, €100, $100 or 100 USDC). | `internal/monitoring/rule_round_amounts.go` |
| Many new payees in one day | `LENS_MONITOR_NEW_PAYEES` | `{"payees":5,"within_hours":24}` | 5 payees, none paid before, first paid within 24 hours. | `internal/monitoring/rule_new_payees.go` |
| How far back the rules look | `LENS_MONITOR_HISTORY_DAYS` | `90` | A payee paid in the last 90 days is not new, and an agent's usual size is its payments over the last 90 days. | `internal/monitoring/monitoring.go` |

## Sanctions screening (B30.6)

| Setting | Starting value | What it means |
|---|---|---|
| `LENS_SCREENING_FUZZY_THRESHOLD` | `0.9` | A name at least this similar (Jaro-Winkler, 0 to 1) to one on a sanctions list holds its payee or payment for an operator. |
