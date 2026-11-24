# Cash-out: credits back into money, behind a licensed partner

B22.9. A workspace's owner can ask to turn an agent's credits back into money in their bank account.

1. **Held.** The credits leave the agent at once and are held: out of the agent (`agent:<id>` → `cashout:<id>`
   in `agent_postings`) and out of the workspace's balance (an `lxc_ledger` row of type `agent_cash_out`), so
   neither the agent nor the workspace can spend them meanwhile.
2. **Submitted.** The request is handed to the cash-out partner, which pays the money — the credits' USD value
   at the LXC peg, `amount_uusd` — to the destination.
3. **Paid**, the held credits are gone for good (`cashout:<id>` → `cashed_out`). **Failed**, they go back to the
   agent and to the workspace's balance, test-funded as they left, with the partner's reason in `detail`.

Lens hands requests to the partner, and records its answers, on the agent schedules' tick.

## The partner

Lens is built against one partner-neutral interface, `economy.CashOutPartner`: `Pay` a cash-out (idempotent on
its id) and ask its `Status`. **The only implementation is the test partner, which pays nothing.** It reports
every payment paid, except to a destination starting `test-fail`, which it reports failed so the way back can
be seen. The real implementation waits for a licensed partner; `destination` is a label for the account,
because the partner, not Talyvor, holds the bank details.

## Class

Cash-out is class **RED** (`cash_out`, see [wallet capabilities](wallet-capabilities.md)). Until the operator
records a clearance it takes test-funded credits only, and a request of live money is refused naming the class.
The owner must be verified, and a paused agent cannot be cashed out.

## Signed

B28.339. Once the workspace has a passkey or the phone's key, a cash-out is held only with a signature by one of
them over the challenge for exactly that cash-out: the agent, the amount and the destination. Without one the
request is refused (403) and nothing is posted. Each challenge is signed once.

Marketplace seller payouts through Stripe Connect (B20.5) are a different
thing and are unchanged.

## API

```
POST /v1/workspaces/{ws}/agents/{agent}/cash-outs/challenge  {"amount_ulxc": 40000000, "destination": "Business account ••1234"}
                                                    → {"challenge", "allow_credentials", "user_verification"}
POST /v1/workspaces/{ws}/agents/{agent}/cash-outs   {"amount_ulxc": 40000000, "destination": "Business account ••1234",
                                                     "assertion": {…the passkey's or the phone's signature…}}
GET  /v1/workspaces/{ws}/cash-outs                  every cash-out: held, submitted, paid or failed
```

Only the workspace's owner or an admin may cash out, never an agent's own key.
