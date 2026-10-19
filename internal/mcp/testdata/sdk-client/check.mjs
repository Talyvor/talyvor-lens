// B40.1: the official TypeScript MCP SDK client against Lens's /mcp, run by TestOfficialTypeScriptSDKClient.
// Usage: node check.mjs <lens base URL> <agent key> <payee agent id>. Prints one JSON line; exits non-zero on a failure.
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { SSEClientTransport } from "@modelcontextprotocol/sdk/client/sse.js";

const [base, key, payee] = process.argv.slice(2);
const requestInit = { headers: { Authorization: `Bearer ${key}` } };
const fail = (msg) => {
  console.error(msg);
  process.exit(1);
};
const text = (res, tool) => {
  if (res.isError) fail(`${tool} refused: ${JSON.stringify(res.content)}`);
  return JSON.parse(res.content[0].text);
};

// Streamable HTTP, the transport Claude, ChatGPT and Cursor use.
const client = new Client({ name: "lens-ci", version: "1.0.0" });
const http = new StreamableHTTPClientTransport(new URL("/mcp", base), { requestInit });
await client.connect(http);
const { tools } = await client.listTools();
for (const t of tools) {
  const a = t.annotations;
  if (!a?.title || typeof a.readOnlyHint !== "boolean" || typeof a.destructiveHint !== "boolean") fail(`${t.name} has no annotations`);
  if (t.name.length > 64) fail(`${t.name} is over 64 characters`);
}
const byName = Object.fromEntries(tools.map((t) => [t.name, t]));
if (!byName.agent_balance?.annotations.readOnlyHint) fail("agent_balance is not readOnlyHint");
if (!byName.agent_pay?.annotations.destructiveHint) fail("agent_pay is not destructiveHint");
await client.ping();
const before = text(await client.callTool({ name: "agent_balance", arguments: {} }), "agent_balance");
const pay = text(await client.callTool({ name: "agent_pay", arguments: { to_agent_id: payee, amount_ulxc: 1500000, memo: "sdk check" } }), "agent_pay");
const out = {
  protocolVersion: http.protocolVersion,
  sessionId: http.sessionId ?? "",
  tools: tools.length,
  balanceBefore: before.agent.balance_ulxc,
  entryId: pay.entry_id,
};
await client.close();

// The 2024-11-05 HTTP+SSE transport, for clients that still use it.
const old = new Client({ name: "lens-ci-sse", version: "1.0.0" });
await old.connect(new SSEClientTransport(new URL("/mcp/sse", base), { requestInit }));
out.sseTools = (await old.listTools()).tools.length;
out.sseBalance = text(await old.callTool({ name: "agent_balance", arguments: {} }), "agent_balance").agent.balance_ulxc;
await old.close();
console.log(JSON.stringify(out));
