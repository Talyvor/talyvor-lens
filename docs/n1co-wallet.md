# N1CO's own wallet (B45.33)

N1CO, Talyvor's AI CEO, spends through Talyvor first. It has its own workspace and an agent wallet called N1CO.
The wallet holds sandbox funds only. Every payment it makes is judged by the same rules any customer's agent has.

## Creating it (operator, once)

N1CO's workspace belongs to one sign-in to the app. Pick the sign-in Nicolai will use for N1CO on his phone. You
need its issuer and its subject: the `iss` and `sub` of its ID token. Then run inside the lens container:

```
lens n1co -issuer <issuer> -subject <subject> -monthly-budget <LXC> [-test-money <LXC>]
```

The command does four things in one go:

- creates the workspace that sign-in lands in, named N1CO;
- adds an agent called N1CO, owned by that workspace;
- starts the agent on the first agent's rules (the `support-bot` template), with `-monthly-budget` as its monthly
  limit;
- funds the agent with `-test-money` LXC of sandbox funds. Without the flag it uses `LENS_STARTER_TEST_MONEY_ULXC`, which
  is 0 until Nicolai sets it.

It prints the agent's key once. Give that key to N1CO.

The budget is Nicolai's to choose, and the command does not set one for you. Running it again changes nothing, and
it says so. The same is true when that sign-in has already been used and its workspace already has an agent.

## The rules for N1CO's tool spend

- **Monthly budget.** Lens refuses any request or payment from the wallet that would take the month's spend past
  `-monthly-budget`. The template's other limits still apply: the cap on each request, the hourly and daily caps,
  approval above 5 LXC, and the pause on unusual spend.
- **Every new vendor needs approval.** N1CO pays a vendor for credits or a subscription with a payment request to a
  saved payee. A person saves vendors as payees, and N1CO never supplies bank details. Until Nicolai signs a
  permission mandate, no payment from N1CO is allowed on its own, so every vendor goes to approval. Later he can sign
  a mandate on his phone with the vendors he trusts, a monthly limit, and `new_payee: always_ask`. From then on,
  vendors in that mandate are paid within the limit, and any vendor that is not in it still comes to him first.

## Approvals on Nicolai's phone

Nicolai signs in to the phone app with the N1CO sign-in once and allows notifications. That registers the phone to
the N1CO workspace. From then on, every approval N1CO's wallet asks for is pushed to the phone, and he approves it
there with the phone's key. A payment request waiting for approval writes no ledger row and pays nothing.

## Real money

The wallet stays on sandbox funds until a partner signs and Nicolai records the clearance for the capability. N1CO's
spend then runs on the same workspace and rules.
