# Wallet capabilities and their classes (B22.1)

Every wallet capability has a class, and the class decides whether it may use real money.

| Class | Real money | Capabilities |
|---|---|---|
| **GREEN** | Now | Spending on Talyvor, buying marketplace listings, moving money between one owner's own agents, rules, approvals, statements and pots, and Talyvor's credit line to companies |
| **AMBER** | Only after the lawyer confirms | Sending and requesting money between different owners, loans between companies, escrow, topping up by a payment from your own bank, credit lines and loans to companies, advances against marketplace earnings, companies lending to companies through the marketplace, and prepaid usage at today's prices |
| **RED** | Only once a licence or a licensed partner exists | Cashing credits out, any loan or credit involving a private user, interest or yield, investing and trading (simulated trading), cards, accounts in pounds, euros, dollars and USDC, account details others can pay into, receiving money from outside Talyvor, paying people and companies outside Talyvor, converting between currencies, stablecoin balances and transfers, paying and being paid over HTTP 402, accepting payments from agents as a business, trading shares, crypto and prediction markets through a broker partner, idle money in a money-market fund, cover for agent mistakes, and paying people for tasks |

An AMBER or RED capability takes **sandbox funds only** until the operator records a clearance for it. A GREEN
one takes any money.

The screens read `GET /v1/wallets/capabilities`. It lists every capability with its class and
`real_money`, which is true for GREEN, and for AMBER or RED only while a clearance is in force.

## What counts as sandbox funds

- **Credits.** Every credit lot records its funding on its `lxc_ledger` row, as `metadata.funding`:
  - `test`: bought in Stripe test mode. It stays sandbox funds for ever.
  - `live`: bought with real money.
  - `grant`: comped by the operator.
  - `synthetic`: LENS converted to LXC.

  `lxc_balances.test_funded_ulxc` holds the test-funded part of a workspace's balance. An uncleared AMBER
  or RED spend is paid out of it, and is refused when it does not cover the amount. Every other spend
  uses the credits that are not sandbox funds first. If a test purchase on a card is reversed or refunded,
  the credits come back as sandbox funds.
- **A Stripe bill.** A payment to another company's agent goes on the paying company's marketplace
  bill. That bill is real money when Lens's Stripe key is live (`sk_live_…`).

A refusal names the class, for example: *"Sending and requesting money between different owners is
class AMBER: it takes no real money until Talyvor records a clearance for it, and this would use real
money"*. A refused card purchase is recorded as declined with those words.

## Sandbox funds never reach a live payout

Each marketplace earning is `livemode` if the invoice that paid it was, and each Stripe payout is
`livemode` if the key that made it was. A live key pays a seller only their live earnings. Earnings taken
as credits are drawn from test earnings first, and the credits keep that funding. A test payout that
Stripe never accepted is never sent with a live key.

Going live with Stripe therefore needs no code change to stay safe.

## Clearing a capability (operator)

Run these inside the lens container:

```
docker compose exec lens /lens clearances
docker compose exec lens /lens clearances clear payments_out --licence EMI-900123 --partner "Partner Ltd" \
    --countries GB,IE --expires 2027-10-01 "partner agreement PA-7"
docker compose exec lens /lens clearances revoke payments_out "partner paused"
docker compose exec lens /lens clearances log
```

(`lens wallet-clearances` is the same command.)

- The first command lists every capability, its class, and its clearance: the countries, the expiry, the
  licence and the partner.
- A clearance names four things, and none is optional:
  - the licence it rests on (`--licence`);
  - the licensed partner the money moves through (`--partner`);
  - the countries live money may be used from, as ISO 3166-1 alpha-2 codes (`--countries GB,IE`);
  - when it ends (`--expires`, a day or an RFC 3339 time).

  It also needs a reference: the lawyer's opinion or the partner's agreement.
- Live money is used from the owner's country: the one their verification confirmed (the company's at L3,
  the person's at L2, from a real provider's check). An owner verified in a country the clearance does not
  list is refused as if there were no clearance, and the refusal names their country and the ones the
  clearance lists; so is an owner whose checks confirm no country. Sandbox funds are used from everywhere.
  Once the expiry passes, live money is refused again.
- A clearance recorded before these terms existed names no country, so it no longer lets live money
  through. Record it again with its terms.
- Every clear and every revoke is a row of `wallet_clearances`. That table is append-only, so it is the
  audit log, and each row records who, when and the reference.
- A revoke stops real money from the capability's next use.
- A GREEN capability cannot be cleared, because it has nothing to clear.

## Verification levels (B30.4)

Live money for a capability also needs the workspace's owner to be verified to the level that capability
needs. Each capability lists its level as `level_needed` in `GET /v1/wallets/capabilities`.

| Level | Meaning | Needed by |
|---|---|---|
| L0 | signed in | spending on Talyvor, buying and renting listings, moving money between your own agents |
| L1 | email and phone confirmed | nothing on its own; the identity check needs it first |
| L2 | identity checked | currency accounts, payments in and out, FX, trading, cards, paying other owners |
| L3 | company checked: its number, directors and people with significant control | credit, advances, lending, merchant acceptance |

- The owner reaches each level through `POST /v1/workspaces/{ws}/verification/contact`, `/identity` and
  `/company`, in that order. `GET /v1/workspaces/{ws}/verification` shows the level and every check.
- Each check goes to the verification provider. Lens keeps the level, the method (which provider checked),
  the status, the date and the provider's reference as evidence. It never keeps a document, a date of birth,
  an email or a phone number.
- The record is append-only (`workspace_verifications`). If a provider later withdraws a pass, a new row is
  added and the level drops.
- A pass by the Test provider shows on the record and counts for sandbox funds only. Live money is judged by
  `live_level`, which counts only the checks a real provider passed. With sandbox funds, every level can try
  every capability.
- Each level's live limits are set by Nicolai and nobody else. Until he sets them, no level takes live money.

```
lens verification-limits                                   each level, the capabilities that need it, and its limits
lens verification-limits set L2 GBP 1000.00 <reference>    the most one movement of live money may move at L2, in GBP
```

A level with no limit of its own in a currency uses the highest limit set at a lower level. A limit of 0 means
no live money in that currency at that level.
