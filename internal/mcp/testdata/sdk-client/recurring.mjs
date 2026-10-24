// B45.13: the official TypeScript MCP SDK client prepares recurring payments from a list, run by
// TestRecurringPaymentsThroughSDKClient. Usage: node recurring.mjs <lens base URL> <key> <arguments JSON>.
// Prints the tool's answer as one JSON line; exits non-zero on a failure.
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const [base, key, args] = process.argv.slice(2);
const client = new Client({ name: "lens-ci-recurring", version: "1.0.0" });
await client.connect(new StreamableHTTPClientTransport(new URL("/mcp", base), { requestInit: { headers: { Authorization: `Bearer ${key}` } } }));
const res = await client.callTool({ name: "prepare_recurring_payments", arguments: JSON.parse(args) });
await client.close();
if (res.isError) {
  console.error(`prepare_recurring_payments refused: ${JSON.stringify(res.content)}`);
  process.exit(1);
}
console.log(res.content[0].text);
