/**
 * The owner's side of Agent Wallets through the SDK (B28.437) — the README's quickstart.
 *
 * A local HTTP server answers the way Lens does: the owner's key creates an agent, asks to fund
 * it (B28.73: nothing moves until a person approves the request from its link, and then the
 * fund line's ref is the SDK's key) and issues its key; a model call made with that key is
 * charged to the agent's wallet; the statement lists both. The SDK talks to it over real HTTP.
 */

import http from "http";
import type { AddressInfo } from "net";
import { AgentWalletError, LensClient } from "../src";

const OWNER = "Bearer tlv_owner_key";
const PERSON = "Bearer person_session";
const AGENT_KEY = "tlv_agent_xyz";
const CALL_COST = 1_500;

describe("Agents", () => {
  let server: http.Server;
  let url: string;
  const seen: { method?: string; path?: string; auth?: string; idem?: string }[] = [];

  beforeEach(async () => {
    seen.length = 0;
    let balance = 0;
    const lines: Record<string, unknown>[] = [];
    const requests = new Map<string, Record<string, any>>();
    let rules: Record<string, unknown> = { max_per_request_ulxc: 0 };
    const answer = (res: http.ServerResponse, status: number, body: unknown) =>
      res.writeHead(status, { "Content-Type": "application/json" }).end(JSON.stringify(body));
    server = http.createServer((req, res) => {
      let raw = "";
      req.on("data", (chunk) => (raw += chunk));
      req.on("end", () => {
        const body = raw ? JSON.parse(raw) : {};
        const auth = req.headers.authorization;
        seen.push({ method: req.method, path: req.url, auth, idem: req.headers["idempotency-key"] as string | undefined });
        const base = "/v1/workspaces/acme/agents";
        if (req.url === "/v1/proxy/openai/v1/chat/completions" && auth === `Bearer ${AGENT_KEY}`) {
          balance -= CALL_COST; // Lens charges the key's agent before the provider is called
          lines.unshift({ entry_id: "e2", kind: "spend", amount_ulxc: -CALL_COST, counterparty: "spend", balance_after_ulxc: balance, at: "" });
          return answer(res, 200, { id: "c1", object: "chat.completion", choices: [{ index: 0, message: { role: "assistant", content: "hi" }, finish_reason: "stop" }] });
        }
        const approve = req.url?.match(/^\/approve\/(afr_\d+)$/);
        if (req.method === "POST" && approve && auth === PERSON) { // the link, used by a person signed in
          const r = [...requests.values()].find((x) => x.id === approve[1])!;
          if (r.status === "pending") {
            balance += r.amount_ulxc;
            lines.unshift({ entry_id: "e1", kind: "fund", amount_ulxc: r.amount_ulxc, counterparty: "workspace", ref: r.idempotency_key, balance_after_ulxc: balance, at: "" });
            Object.assign(r, { status: "approved", balance_ulxc: balance });
          }
          return answer(res, 200, r);
        }
        if (auth !== OWNER) return answer(res, 403, { error: "only the workspace's owner or an admin may manage its agents" });
        if (req.method === "POST" && req.url === base) return answer(res, 201, { id: "agt_1", name: body.name, balance_ulxc: 0, keys: [] });
        if (req.method === "POST" && req.url === `${base}/agt_1/fund-requests`) {
          const key = req.headers["idempotency-key"] as string;
          if (!key) return answer(res, 400, { error: "an Idempotency-Key is required" });
          if (requests.has(key)) return answer(res, 200, requests.get(key));
          const id = `afr_${requests.size + 1}`;
          const r = { id, agent_id: "agt_1", amount_ulxc: body.amount_ulxc, idempotency_key: key, status: "pending", approval_url: `${url}/approve/${id}`, balance_ulxc: null };
          requests.set(key, r);
          return answer(res, 201, r);
        }
        const read = req.url?.match(/^\/v1\/workspaces\/acme\/agents\/fund-requests\/(afr_\d+)$/);
        if (req.method === "GET" && read) return answer(res, 200, [...requests.values()].find((x) => x.id === read[1]));
        if (req.method === "GET" && req.url === `${base}/agt_1/rules`) return answer(res, 200, rules);
        if (req.method === "POST" && req.url === `${base}/agt_1/rules/proposals`) {
          rules = { ...rules, ...body.rules };
          return answer(res, 200, { status: "applied", changes: Object.keys(body.rules), rules });
        }
        if (req.method === "POST" && req.url === `${base}/agt_1/keys`) return answer(res, 201, { agent_id: "agt_1", key: AGENT_KEY, id: "k1", prefix: "tlv_agen", warning: "" });
        if (req.method === "GET" && req.url === `${base}/agt_1/statement?limit=10`) return answer(res, 200, { agent_id: "agt_1", lines });
        if (req.method === "POST" && req.url === `${base}/agt_nope/fund-requests`) return answer(res, 404, { error: "economy: no such agent" });
        return answer(res, 404, { error: "not found" });
      });
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    url = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  });

  afterEach(() => new Promise<void>((resolve) => server.close(() => resolve())));

  it("the owner creates an agent, funds it through the approval link, issues its key, the agent calls a model and the statement shows the spend", async () => {
    const owner = new LensClient({ lensUrl: url, apiKey: "tlv_owner_key", workspaceId: "acme" });

    const agent = await owner.agents.create("researcher");
    expect(agent.id).toBe("agt_1");
    const request = await owner.agents.fund(agent.id, 10_000_000, { idempotencyKey: "fund-1" });
    expect([request.status, request.idempotency_key, request.balance_ulxc]).toEqual(["pending", "fund-1", null]);
    expect((await owner.agents.statement(agent.id, { limit: 10 })).lines).toEqual([]); // nothing moves before the link is used
    await fetch(request.approval_url, { method: "POST", headers: { Authorization: PERSON } });
    expect((await owner.agents.fundRequest(request.id)).status).toBe("approved");
    const { key } = await owner.agents.issueKey(agent.id);

    const ai = new LensClient({ lensUrl: url, apiKey: key, workspaceId: "acme" }).openai() as any;
    await ai.chat.completions.create({ model: "gpt-4o-mini", messages: [{ role: "user", content: "hello" }] });

    const { lines } = await owner.agents.statement(agent.id, { limit: 10 });
    expect(lines.map((l) => [l.kind, l.amount_ulxc, l.balance_after_ulxc])).toEqual([
      ["spend", -CALL_COST, 10_000_000 - CALL_COST],
      ["fund", 10_000_000, 10_000_000],
    ]);
    expect((await owner.agents.statement(agent.id, { limit: 10 })).lines[1].ref).toBe("fund-1");

    const unkeyed = await owner.agents.fund(agent.id, 1_000);
    expect(unkeyed.idempotency_key).toMatch(/^sdk-fund-[0-9a-f-]{36}$/);
    expect(seen[seen.length - 1].idem).toBe(unkeyed.idempotency_key);

    expect((await owner.agents.setRules(agent.id, { max_per_request_ulxc: 1_000_000 })).status).toBe("applied");
    expect((await owner.agents.rules(agent.id)).max_per_request_ulxc).toBe(1_000_000);

    await expect(owner.agents.fund("agt_nope", 1)).rejects.toThrow(AgentWalletError);
    await expect(owner.agents.fund("agt_nope", 1)).rejects.toThrow(/Lens answered 404: .*no such agent/);
  });
});
