# Loans between companies (B22.5)

An agent of one company can lend credits to an agent of another. The borrower repays in equal instalments on
a schedule, with interest.

Both sides must be companies with verified owners. A company is recorded by the operator
(`lens credit-lines company <workspace> on`, see [credit-lines.md](credit-lines.md)). Credit involving a
private user is class RED, so a private user cannot lend or borrow.

A loan is class AMBER (`loans_between_companies`). Until the operator clears it, a loan can be made only of
test-funded credits. Offering live money without a clearance is refused, and the refusal names the class.

## Offering and answering

```
POST /v1/workspaces/{ws}/agents/{agent}/loans
  {"to": "@borrower", "principal_ulxc": 100000000, "interest_bps": 1000, "instalments": 3,
   "every": "day|week|month", "late_fee_ulxc": 2000000, "memo": "working capital"}
POST /v1/workspaces/{ws}/loans/{loan}/accept      the borrower's side: the principal is paid out
POST /v1/workspaces/{ws}/loans/{loan}/decline
POST /v1/workspaces/{ws}/loans/{loan}/withdraw    the lender's side, before it is answered
GET  /v1/workspaces/{ws}/loans                    what it lends and borrows, with every event
GET  /v1/workspaces/{ws}/loans/{loan}
```

`interest_bps` is the interest on the principal over the whole term: 1000 is 10%. Principal and interest are
split into equal instalments, and the last one takes what the division leaves.

Accepting pays the principal from the lender's agent to the borrower's, as a transfer (B22.3). The lender's
agent rules judge the payout.

Talyvor keeps a lending fee of the principal (`lending_fee_bps` in `GET /v1/public/fees`, 1% by default). The
offer shows it as `fee_bps` and `fee_ulxc` before the borrower answers. Accepting takes that fee from what the
borrower received, once. It is its own line on the borrower's statement ("Lending fee 1%"), and the payout event
names it as `fee_ulxc`. A declined or withdrawn offer costs nothing.

## Repayments

Each instalment is taken from the borrower's agent when it falls due, on the agent schedules' minute tick.
- **Paid.** The instalment moves to the lender's agent.
- **Missed** (the borrower's agent cannot cover it). The loan is `late`. The instalment is tried again one
  period later, with the late fee added.
- **Missed again.** The loan is `defaulted`, and nothing more is taken.

Both sides see the loan's status and every event: payout, instalment, missed, late, defaulted.

## Records

Each movement of a loan's money is an `agent_transfers` row naming the loan. A loan's payout or instalment
cannot be given back as a plain refund.

The events are in `agent_loan_events`, which is append-only. Both sides' statements carry the loan's terms,
the events in the period, and the transfers as lines.
