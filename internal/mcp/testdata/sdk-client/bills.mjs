// B45.17: "Pay my bills" from any assistant — the official TypeScript MCP SDK client lists the bills due and prepares
// paying them all, run by TestOfficialTypeScriptSDKClient_PayMyBills. Usage: node bills.mjs <lens base URL> <key>
// <idempotency key>. Prints one JSON line; exits non-zero on a failure.
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const [base, key, idem] = process.argv.slice(2);
const fail = (msg) => {
  console.error(msg);
  process.exit(1);
};
const client = new Client({ name: "lens-ci-bills", version: "1.0.0" });
await client.connect(new StreamableHTTPClientTransport(new URL("/mcp", base), { requestInit: { headers: { Authorization: `Bearer ${key}` } } }));
const { tools } = await client.listTools();
const list = tools.find((t) => t.name === "list_bills_due");
if (!list?.annotations.readOnlyHint) fail("list_bills_due is missing or not readOnlyHint");
const call = async (name, args) => {
  const res = await client.callTool({ name, arguments: args });
  if (res.isError) fail(`${name} refused: ${JSON.stringify(res.content)}`);
  return JSON.parse(res.content[0].text);
};
const due = await call("list_bills_due", {});
const request = await call("prepare_bills_payment", { idempotency_key: idem, client: "mcp:lens-ci" });
await client.close();
console.log(JSON.stringify({ due, request }));
