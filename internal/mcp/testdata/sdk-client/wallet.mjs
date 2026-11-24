// B48.16: an assistant uses an agent's own wallet — the official TypeScript MCP SDK client lists the tools, then makes
// the calls it is given, in order, run by TestOfficialTypeScriptSDKClient_AgentWallet. Usage: node wallet.mjs <lens
// base URL> <key> '<[{"name": …, "arguments": {…}}, …]>'. Prints one JSON line: each tool's name and annotations, and
// each call's answer with whether it was refused. Exits non-zero when the client itself fails.
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const [base, key, calls] = process.argv.slice(2);
const client = new Client({ name: "lens-ci-wallet", version: "1.0.0" });
await client.connect(new StreamableHTTPClientTransport(new URL("/mcp", base), { requestInit: { headers: { Authorization: `Bearer ${key}` } } }));
const { tools } = await client.listTools();
const results = [];
for (const { name, arguments: args } of JSON.parse(calls)) {
  const res = await client.callTool({ name, arguments: args });
  const text = res.content[0].text;
  let value = text;
  try {
    value = JSON.parse(text);
  } catch {}
  results.push({ name, isError: !!res.isError, value });
}
await client.close();
console.log(JSON.stringify({ tools: tools.map(({ name, annotations }) => ({ name, annotations })), results }));
