# The three top-up amounts: where they actually live, and what an arbitrary amount would cost

W4.10 asks for arbitrary top-up amounts and says to **measure first** — *"the three amounts may be an
allowlist in one place or hardcoded in several. ⚠ FIND EVERY ONE — the Stripe call, the BFF, the
screen, any validation, any test fixture. A partial change here fails at the money boundary."*

Measured 2026-08-28 at lens `cc1576a` and suite `b6faf92`. **No amount was changed.** The ceiling and
the minimum are decisions and are left open at the bottom.

## 1. The census: the item's fear is not what is true

There is **one** server-side allowlist and **one** deliberate mirror. The screen hardcodes nothing.

| site | what it is |
|---|---|
| `internal/billing/billing.go` → `allowedTopUps = []int64{1000, 5000, 10000}` | **the authority.** `CreateCheckout` refuses anything else with `ErrAmountNotAllowed`, and the webhook re-checks it |
| talyvor-suite `apps/bff/billing.go` → `allowedTopUpCents` | a **documented mirror**, unavoidable because the list is on no endpoint (below). Refuses off-list amounts before dialling Lens and serves the list at `GET /api/lxc/topup-options` |
| talyvor-suite `apps/web/src/areas/lens/TopUp.tsx` | **fetches** the list; its own header records why a price written into that file would be a button that fails the moment the two disagree |

Three instruments already keep the mirror honest, and they were run rather than read:
`TestAllowedTopUpsRestatedDeliberately` (makes an edit a two-place edit),
`apps/web/src/topUpMirrorRegister.test.ts` (keeps the deploy grep's amounts equal to the BFF's), and
`deploy/decision-expiry.sh`, which carries the cross-repo grep in its **UNCHECKABLE** half.

**That UNCHECKABLE premise is now settled.** Run in a lens checkout at `cc1576a`:

```
grep -c '^var allowedTopUps = \[\]int64{1000, 5000, 10000}$' internal/billing/billing.go   → 1   PASS
```

Lens still accepts exactly $10 / $50 / $100, so the BFF mirror is currently correct. The register
says plainly that its uncheckable entries are *not* passes; this one is now a pass, on this date, at
this SHA, and goes stale the moment either side moves.

## 2. The constraint the item does not mention, and it binds the whole feature

Both copies carry it: the list is **ADDITIVE-ONLY**.

> async payment methods (e.g. bank debits) can settle DAYS after session creation, and the webhook
> re-checks this list, so REMOVING a value would mark a legitimately-PAID purchase anomalous
> (charged, not credited).

So "arbitrary amounts" cannot be implemented by **replacing** the allowlist with a range check. A
session created under the old list can still settle after the swap, and the webhook would re-check it
against a rule that no longer names it. Whatever replaces the list has to keep admitting every amount
any in-flight session was created with. That is a migration question, not a validation question, and
it is the part most likely to be missed — it fails at the money boundary, silently, days later, on a
customer who has already been charged.

## 3. What an amount BUYS is already computable, with no new money mirror

The item asks the screen to show what the amount buys in credits. **It can, today, without
hardcoding a price**, and the neighbouring comment's reasoning does not extend to this:

- The peg is fixed — `internal/economy` `LXCUSDValue = 0.10`, and `billing.lxcForCents` recomputes
  LXC from cents server-side as the single price authority.
- **The peg is already served publicly**: `GET /v1/economy/conversion-rate` returns
  `"usd_per_lxc": economy.LXCUSDValue`, on the unauthenticated (rate-limited) public group.

⚠ The BFF comment says *"Lens itself exposes that list on no endpoint"* — **true of the ALLOWLIST,
not of the PEG.** The allowlist has no endpoint, so that mirror is genuinely unavoidable. The peg has
one, so a credit conversion needs **no** second mirror of a money constant: the BFF can read
`usd_per_lxc` and serve it alongside the amounts it already serves.

One caveat, and it is the shape the BFF already handles: that route is registered only when
`EconomyEnabled` (`econReg.get` simply does not register when off), so an economy-off deployment
404s it — exactly the probe pattern `billing_enabled` already uses.

At the current peg: **$10 → 100 LXC · $50 → 500 LXC · $100 → 1000 LXC.**

## 4. The minimum is NOT computable from anything committed

Nothing in any of these repositories records a Stripe fee. Measured: a sweep of lens for the fee
shape (`2.9%`, `0.30 per`, "stripe fee", "processing fee") returns **zero hits**, in code and docs.

The minimum sensible top-up is where the per-charge fee eats the gross margin on the credits sold:

```
net_to_talyvor(amount) = amount − stripe_fee(amount)
stripe_fee(amount)     = pct × amount + fixed        ← both from the ACCOUNT's pricing
worth_selling          ⟺ net_to_talyvor(amount) × gross_margin_per_dollar_of_credit > 0 … and > the
                          support/ops cost of the transaction
```

**Neither input is in the repo.** `pct` and `fixed` are the account's negotiated Stripe pricing —
readable in the Stripe dashboard, which this measurement has no access to. `gross_margin_per_dollar_of_credit`
is Talyvor's margin on inference resold through the peg, and nobody has written it down anywhere.

What *is* structural, and is true for any fixed-plus-percentage card fee: **the fixed part dominates
at small amounts.** Illustrating with Stripe's published US standard card rate at the time of writing
— *2.9% + $0.30, which must be verified against the account's actual pricing before anyone relies on
a row here*:

| charge | fee | fee as % of charge |
|---|---|---|
| $1 | $0.329 | **32.9%** |
| $5 | $0.445 | 8.9% |
| $10 | $0.590 | 5.9% |
| $50 | $1.750 | 3.5% |
| $100 | $3.200 | 3.2% |
| $500 | $14.80 | 3.0% |

The curve is the finding, not the rows: the existing $10 floor already sits at ~5.9% and anything
below about $5 is paying a double-digit percentage to the processor. Where exactly that stops making
sense is a margin call.

## 5. Left open, deliberately

- **The CEILING.** The item says the number is Nicolai's, and it is: an unbounded field on a payment
  form is a fraud surface and a fat-finger surface, and picking the bound is a risk decision.
- **The MINIMUM.** A price call, and one that cannot even be computed until someone supplies the two
  inputs in §4. Measure the account's real Stripe rate first; the table above is an example.
- **The migration in §2** is not a decision but it is the real work, and it must be designed before
  the allowlist stops being a list.

Nothing here changes an amount, a threshold or a price.
