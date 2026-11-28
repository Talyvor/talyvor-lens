/**
 * Agents — the workspace owner's side of Agent Wallets at Talyvor Lens.
 *
 * Called with the owner's key (a workspace key with the keys or admin scope):
 * create an agent, ask to fund its wallet from the workspace's LXC, set its
 * spending rules, issue the agent a key of its own, and read its page and
 * statement. The agent then calls through Lens with that key, and every model
 * call is charged to its wallet, within its spending rules, before the
 * provider is called.
 *
 * Funding moves nothing by itself (B28.73): ``fund`` answers an approval link,
 * and the agent is funded once a person signed in to the workspace approves it
 * there — one posting, its ref the request's idempotency key. A key never
 * approves its own request. Creating an agent needs its owner signed in: a
 * workspace key cannot create one.
 *
 * They are Lens's REST routes under ``/v1/workspaces/{ws}/agents``, in the
 * client's workspace. A non-2xx answer rejects with ``AgentWalletError``
 * carrying Lens's status and message.
 */

import { randomUUID } from "crypto";
import { AgentWalletError } from "./agentWallet";
import type { AgentAccount, AgentFundRequest, AgentKey, AgentRules, AgentStatement, FiledRules } from "./types";

/**
 * Reach it as ``new LensClient({...}).agents``. Amounts are in µLXC
 * (1 LXC = 1,000,000 µLXC).
 *
 * Usage:
 *   const agents = new LensClient({ lensUrl, apiKey: ownerKey, workspaceId: "acme" }).agents;
 *   const agent = await agents.create("researcher");
 *   const request = await agents.fund(agent.id, 10_000_000);
 *   // ...a person opens request.approval_url and approves it; then:
 *   const { key } = await agents.issueKey(agent.id);
 *   // ...the agent calls through Lens with `key`, then:
 *   const { lines } = await agents.statement(agent.id);
 */
export class Agents {
  private readonly base: string;
  private readonly headers: Record<string, string>;

  constructor(lensUrl: string, workspaceId: string, headers: Record<string, string>) {
    this.base = `${lensUrl}/v1/workspaces/${encodeURIComponent(workspaceId)}/agents`;
    this.headers = { ...headers, "Content-Type": "application/json" };
  }

  /** Every agent in the workspace, with its balance. */
  async list(): Promise<AgentAccount[]> {
    const book = await this.send<{ agents: AgentAccount[] }>("GET", "");
    return book.agents;
  }

  /** Create an agent. It starts with an empty wallet and no key. */
  create(name: string): Promise<AgentAccount> {
    return this.send("POST", "", { name });
  }

  /** One agent's page: balance, safe to spend, this month, approvals waiting, latest statement lines. */
  get(agentId: string): Promise<Record<string, unknown>> {
    return this.send("GET", `/${encodeURIComponent(agentId)}/summary`);
  }

  /**
   * Ask to move µLXC from the workspace to the agent's wallet. Nothing moves
   * yet: resolves to the request with its ``approval_url``, and the agent is
   * funded once a person approves it there. The request is filed under
   * ``idempotencyKey`` (one is made when none is given): the same key again is
   * the same request and, once approved, one posting whose ref is that key.
   */
  fund(agentId: string, amountUlxc: number, opts: { idempotencyKey?: string } = {}): Promise<AgentFundRequest> {
    const key = opts.idempotencyKey || `sdk-fund-${randomUUID()}`;
    return this.send("POST", `/${encodeURIComponent(agentId)}/fund-requests`, { amount_ulxc: amountUlxc }, key);
  }

  /** A request to fund an agent, as it stands: pending, approved (with the agent's balance) or declined. */
  fundRequest(requestId: string): Promise<AgentFundRequest> {
    return this.send("GET", `/fund-requests/${encodeURIComponent(requestId)}`);
  }

  /** The agent's spending rules in force. */
  rules(agentId: string): Promise<AgentRules> {
    return this.send("GET", `/${encodeURIComponent(agentId)}/rules`);
  }

  /**
   * Set the agent's spending rules. Rules that only tighten what is in force
   * apply at once (``status: "applied"``); rules that loosen any of it wait for
   * the owner's passkey (``status: "pending"``).
   */
  setRules(agentId: string, rules: Partial<AgentRules>): Promise<FiledRules> {
    return this.send("POST", `/${encodeURIComponent(agentId)}/rules/proposals`, { rules });
  }

  /** Take µLXC back from the agent's wallet to the workspace. */
  withdraw(agentId: string, amountUlxc: number, opts: { idempotencyKey?: string } = {}): Promise<{ agent_id: string; balance_ulxc: number }> {
    return this.send("POST", `/${encodeURIComponent(agentId)}/withdraw`, { amount_ulxc: amountUlxc }, opts.idempotencyKey);
  }

  /**
   * Issue the agent a proxy key of its own. ``key`` is shown once: a client
   * built with it calls models on the agent's wallet and reaches ``.wallet``.
   */
  issueKey(agentId: string, name = ""): Promise<AgentKey> {
    return this.send("POST", `/${encodeURIComponent(agentId)}/keys`, name ? { name } : {});
  }

  /** The agent's account, newest first: funding, model spend, payments (at most 1000 lines). */
  statement(agentId: string, opts: { limit?: number } = {}): Promise<AgentStatement> {
    const q = opts.limit ? `?limit=${opts.limit}` : "";
    return this.send("GET", `/${encodeURIComponent(agentId)}/statement${q}`);
  }

  private async send<T>(method: string, path: string, body?: unknown, idempotencyKey?: string): Promise<T> {
    const headers = { ...this.headers };
    if (idempotencyKey) headers["Idempotency-Key"] = idempotencyKey;
    const what = `${method} ${this.base}${path}`;
    let resp: Response;
    try {
      resp = await fetch(`${this.base}${path}`, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
      });
    } catch (err) {
      throw new AgentWalletError(`${what}: ${(err as Error).message}`);
    }
    const text = await resp.text();
    if (resp.status < 200 || resp.status >= 300) {
      throw new AgentWalletError(`${what}: Lens answered ${resp.status}: ${text.trim()}`);
    }
    try {
      return JSON.parse(text) as T;
    } catch {
      throw new AgentWalletError(`${what}: unreadable answer: ${text}`);
    }
  }
}
