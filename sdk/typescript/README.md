# Talyvor Lens TypeScript SDK

Give every AI agent its own Talyvor wallet — a budget, spending rules, approvals and a live
statement — and call models through Lens with the agent's own key, so Lens charges the agent's
wallet and enforces its rules before the provider is called. The same client is a drop-in
OpenAI wrapper that adds caching, routing, attribution and cost tracking without changing your
application code.

## Quick start: an agent with its own wallet

```typescript
import { LensClient } from "talyvor-lens";

const lensUrl = "https://lens.talyvor.com";
const owner = new LensClient({ lensUrl, apiKey: process.env.TALYVOR_OWNER_TOKEN!, workspaceId: "acme" });

// 1. Create an agent (signed in as its owner). 2. Ask to fund its wallet from the workspace (10 LXC = 10,000,000 µLXC).
const agent = await owner.agents.create("researcher");
const request = await owner.agents.fund(agent.id, 10_000_000, { idempotencyKey: "fund-researcher-oct" });
// Nothing moves yet: a person signed in to the workspace approves it at request.approval_url.

// 3. Issue the agent its own key (shown once).
const { key } = await owner.agents.issueKey(agent.id);

// 4. The agent calls models through Lens with that key: each call is charged to its wallet.
const ai = new LensClient({ lensUrl, apiKey: key, workspaceId: "acme" }).openai() as any;
await ai.chat.completions.create({ model: "gpt-4o-mini", messages: [{ role: "user", content: "Hello" }] });

// 5. Read its statement, newest first: the model call's three lines, then the funding.
const { lines } = await owner.agents.statement(agent.id);
// [{ kind: "platform_fee", amount_ulxc: -4, balance_after_ulxc: 9999930, ... },
//  { kind: "settle", amount_ulxc: 25891, balance_after_ulxc: 9999934, ... },
//  { kind: "hold", amount_ulxc: -25957, balance_after_ulxc: 9974043, ... },
//  { kind: "fund", amount_ulxc: 10000000, balance_after_ulxc: 10000000, ... }]
```

These are the lines a local Lens wrote for that call: the `hold` sets aside the most the call could
cost, the `settle` returns what it did not use, so the call cost the hold less the settle
(25,957 − 25,891 = 66 µLXC), and the platform fee is a line of its own (4 µLXC here, at the
workspace's plan rate).

Every agent has a person as its owner, so create it signed in — with your own token, or on the
Agent Wallets screen of the Talyvor app. Funding, `withdraw`, `issueKey`, `statement` and
`list` also take a workspace key with the `keys` scope. A refusal is an `AgentWalletError`
carrying Lens's status and reason (for example `409` when the workspace has too few LXC).

The agent's key also reaches its wallet directly — balance, payments, approvals: see
[The agent's side of its wallet](#the-agents-side-of-its-wallet).

## Installation

```bash
npm install talyvor-lens
# OpenAI is a peer dependency:
npm install openai
```

Requires Node 18+.

## Drop-in client (3 lines)

```typescript
import { LensClient } from "talyvor-lens";
const client = new LensClient({ lensUrl: "http://your-lens:8080", apiKey: "tlv_..." });
const ai = client.openai();
```

Then use `ai` exactly like the standard OpenAI client:

```typescript
const r = await (ai as any).chat.completions.create({
  model: "gpt-4o",
  messages: [{ role: "user", content: "Hello" }],
});
```

## With session tracking

```typescript
const turn = client.withSession("sess-abc123", "researcher");
const reply = await (turn.openai() as any).chat.completions.create({ ... });
```

Each call under the same `sessionId` is grouped into one conversation in the Lens dashboard.

## With Git branch attribution

```typescript
const pr = client.withBranch("feat/new-login", "142");
const result = await (pr.openai() as any).chat.completions.create({ ... });
```

Spend incurred on this PR shows up in `/v1/api/attribution/branch`.

## Standalone header injection

If you already have an HTTP client (fetch, axios, undici), use `injectLensHeaders`:

```typescript
import { injectLensHeaders } from "talyvor-lens";

const headers = injectLensHeaders(
  {},
  {
    apiKey: "tlv_...",
    workspaceId: "finance",
    sessionId: "sess-1",
    team: "ml-platform",
  },
);
await fetch("http://lens:8080/v1/proxy/openai/v1/chat/completions", {
  method: "POST",
  headers,
  body: JSON.stringify(body),
});
```

## The agent's side of its wallet

With a key attached to an agent (`POST /v1/workspaces/{ws}/agents/{agent}/keys`), the agent
uses its own account. Amounts are µLXC (1 LXC = 1,000,000 µLXC); every call is
logged by Lens, and the agent's spending rules judge every payment.

```ts
import { LensClient, PaymentRefused } from "talyvor-lens";

const wallet = new LensClient({ lensUrl: "https://lens.talyvor.com", apiKey: "tlv_ws_..." }).wallet;

(await wallet.balance()).agent.balance_ulxc; // 10000000
await wallet.requestApproval("agt_seller", 2_000_000, "October hosting");
// ...a person approves it on the Agent Wallets screen, then:
const payment = await wallet.pay("agt_seller", 2_000_000);
(await wallet.receipt(payment.entry_id)).postings; // every posting, summing to zero

try {
  await wallet.pay("agt_seller", 6_000_000);
} catch (err) {
  if (err instanceof PaymentRefused) console.log(err.message); // "...the agent's limit per request is 5 LXC"
}
```

A refusal by Lens is `PaymentRefused`; Lens not answering (network, a
rejected key) is its parent, `AgentWalletError`.

### Everything else the wallet does

The same key reaches every capability of the wallet (Lens's `wallet_*` MCP tools). Each is
judged by the agent's rules and by its capability's class: a refusal says which.

```ts
await wallet.send("@seller", 1_000_000, "hosting");                 // to any agent on Talyvor
await wallet.request("@buyer", 2_000_000);                           // it accepts or declines
await wallet.answerRequest("mreq_…", true);                          // pay a request made of the agent
await wallet.refund("xfer_…");                                       // give a received transfer back
await wallet.creditLine();                                           // the company's line
await wallet.offerLoan("@other-co", 30_000_000, 3, "week", { interestBps: 500 });
await wallet.answerLoan("loan_…", true);                             // take a loan offered to the agent
await wallet.escrowPay("@seller", 5_000_000, new Date("2026-10-06T12:00:00Z"), "logo");
await wallet.escrowConfirm("escrow_…");                              // or escrowDispute(id, reason)
const pot = await wallet.potCreate("rainy day", "reserve", { lockedUntil: "2026-12-01T00:00:00Z" });
await wallet.potMove(pot.id as string, 1_000_000, "in");
const pf = await wallet.portfolioOpen("fx", 1_000_000_000);          // simulated USD: no money moves
await wallet.order(pf.id as string, "EUR", "buy", "limit", 100_000_000, "1.15");
```

Each has a reader too: `requests()`, `loans()`, `escrows()`, `pots()`, `quotes()` and
`portfolios()`.

## Headers set by the SDK

| Header | Source | Purpose |
|--------|--------|---------|
| `Authorization: Bearer …` | `apiKey` | Lens API key authentication |
| `X-Talyvor-Workspace` | `workspaceId` (default: `"default"`) | Cache + policy scope |
| `X-Talyvor-Team` | `team` | Spend attribution bucket |
| `X-Talyvor-Feature` | `feature` | Spend attribution bucket |
| `X-Talyvor-Session` | `sessionId` | Multi-turn agent session ID |
| `X-Talyvor-Agent` | `agentName` | Agent identifier within a session |
| `X-Talyvor-Branch` | `branch` | Git branch for PR-level cost attribution |
| `X-Talyvor-PR` | `prNumber` | GitHub PR number |
| `X-Talyvor-Commit` | `commit` | Git commit SHA |
| `X-Talyvor-Repository` | `repository` | `owner/name` repo identifier |

Empty / undefined values are silently dropped — no blank headers ever leave the SDK.

## License

[MIT](LICENSE). This SDK is deliberately licensed more permissively than the Talyvor Lens server
it talks to, which is under the [Business Source License 1.1](../../LICENSE): the SDK is a thin
client you embed in your own application, so it should not put a licence review in the way of
an integration.
