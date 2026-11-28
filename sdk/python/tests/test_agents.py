"""The owner's side of Agent Wallets through the SDK (B28.437) — the README's quickstart.

A local HTTP server answers the way Lens does: the owner's key creates an agent, asks to fund
it (B28.73: nothing moves until a person approves the request from its link, and then the
fund line's ref is the SDK's key) and issues its key; a model call made with that key is
charged to the agent's wallet; the statement lists both. The SDK talks to it over real HTTP.
"""

from __future__ import annotations

import json
import re
import threading
import urllib.request
from collections.abc import Iterator
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

import pytest

from talyvor_lens import AgentWalletError, LensClient

OWNER = "Bearer tlv_owner_key"
PERSON = "Bearer person_session"
AGENT_KEY = "tlv_agent_xyz"
CALL_COST = 1_500
BASE = "/v1/workspaces/acme/agents"


@pytest.fixture
def lens() -> Iterator[tuple[list[dict[str, Any]], str]]:
    seen: list[dict[str, Any]] = []
    state: dict[str, Any] = {"balance": 0, "lines": [], "requests": {}, "rules": {"max_per_request_ulxc": 0}}

    class Handler(BaseHTTPRequestHandler):
        def answer(self, status: int, body: Any) -> None:
            raw = json.dumps(body).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def route(self) -> None:
            n = int(self.headers.get("Content-Length") or 0)
            body = json.loads(self.rfile.read(n) or b"{}")
            auth = self.headers.get("Authorization")
            seen.append({"method": self.command, "path": self.path, "idem": self.headers.get("Idempotency-Key")})
            if self.path == "/v1/proxy/openai/v1/chat/completions" and auth == f"Bearer {AGENT_KEY}":
                state["balance"] -= CALL_COST  # Lens charges the key's agent before the provider is called
                state["lines"].insert(0, {"entry_id": "e2", "kind": "spend", "amount_ulxc": -CALL_COST,
                                          "counterparty": "spend", "balance_after_ulxc": state["balance"], "at": ""})
                return self.answer(200, {"id": "c1", "object": "chat.completion", "created": 0, "model": "gpt-4o-mini",
                                         "choices": [{"index": 0, "finish_reason": "stop",
                                                      "message": {"role": "assistant", "content": "hi"}}]})
            approve = re.fullmatch(r"/approve/(afr_\d+)", self.path)
            if self.command == "POST" and approve and auth == PERSON:  # the link, used by a person signed in
                r = next(x for x in state["requests"].values() if x["id"] == approve[1])
                if r["status"] == "pending":
                    state["balance"] += r["amount_ulxc"]
                    state["lines"].insert(0, {"entry_id": "e1", "kind": "fund", "amount_ulxc": r["amount_ulxc"], "ref": r["idempotency_key"],
                                              "counterparty": "workspace", "balance_after_ulxc": state["balance"], "at": ""})
                    r.update(status="approved", balance_ulxc=state["balance"])
                return self.answer(200, r)
            if auth != OWNER:
                return self.answer(403, {"error": "only the workspace's owner or an admin may manage its agents"})
            if self.command == "POST" and self.path == BASE:
                return self.answer(201, {"id": "agt_1", "name": body["name"], "balance_ulxc": 0, "keys": []})
            if self.command == "POST" and self.path == f"{BASE}/agt_1/fund-requests":
                key = self.headers.get("Idempotency-Key")
                if not key:
                    return self.answer(400, {"error": "an Idempotency-Key is required"})
                if key in state["requests"]:
                    return self.answer(200, state["requests"][key])
                rid = f"afr_{len(state['requests']) + 1}"
                state["requests"][key] = {"id": rid, "agent_id": "agt_1", "amount_ulxc": body["amount_ulxc"], "idempotency_key": key,
                                          "status": "pending", "approval_url": f"{state['url']}/approve/{rid}", "balance_ulxc": None}
                return self.answer(201, state["requests"][key])
            read = re.fullmatch(rf"{BASE}/fund-requests/(afr_\d+)", self.path)
            if self.command == "GET" and read:
                return self.answer(200, next(x for x in state["requests"].values() if x["id"] == read[1]))
            if self.command == "GET" and self.path == f"{BASE}/agt_1/rules":
                return self.answer(200, state["rules"])
            if self.command == "POST" and self.path == f"{BASE}/agt_1/rules/proposals":
                state["rules"].update(body["rules"])
                return self.answer(200, {"status": "applied", "changes": list(body["rules"]), "rules": state["rules"]})
            if self.command == "POST" and self.path == f"{BASE}/agt_1/keys":
                return self.answer(201, {"agent_id": "agt_1", "key": AGENT_KEY, "id": "k1", "prefix": "tlv_agen", "warning": ""})
            if self.command == "GET" and self.path == f"{BASE}/agt_1/statement?limit=10":
                return self.answer(200, {"agent_id": "agt_1", "lines": state["lines"]})
            if self.command == "POST" and self.path == f"{BASE}/agt_nope/fund-requests":
                return self.answer(404, {"error": "economy: no such agent"})
            return self.answer(404, {"error": "not found"})

        do_GET = do_POST = route

        def log_message(self, *args: Any) -> None:
            pass

    server = HTTPServer(("127.0.0.1", 0), Handler)
    state["url"] = f"http://127.0.0.1:{server.server_port}"
    threading.Thread(target=server.serve_forever, daemon=True).start()
    yield seen, f"http://127.0.0.1:{server.server_port}"
    server.shutdown()


def test_the_owner_creates_an_agent_funds_it_through_the_approval_link_issues_its_key_and_the_statement_shows_the_agents_spend(lens) -> None:
    seen, url = lens
    owner = LensClient(lens_url=url, api_key="tlv_owner_key", workspace_id="acme")

    agent = owner.agents.create("researcher")
    assert agent["id"] == "agt_1"
    request = owner.agents.fund(agent["id"], 10_000_000, idempotency_key="fund-1")
    assert (request["status"], request["idempotency_key"], request["balance_ulxc"]) == ("pending", "fund-1", None)
    assert owner.agents.statement(agent["id"], limit=10)["lines"] == []  # nothing moves before the link is used
    urllib.request.urlopen(urllib.request.Request(request["approval_url"], method="POST", headers={"Authorization": PERSON}))
    assert owner.agents.fund_request(request["id"])["status"] == "approved"
    key = owner.agents.issue_key(agent["id"])["key"]

    ai = LensClient(lens_url=url, api_key=key, workspace_id="acme").openai
    ai.chat.completions.create(model="gpt-4o-mini", messages=[{"role": "user", "content": "hello"}])

    lines = owner.agents.statement(agent["id"], limit=10)["lines"]
    assert [(l["kind"], l["amount_ulxc"], l["balance_after_ulxc"]) for l in lines] == [
        ("spend", -CALL_COST, 10_000_000 - CALL_COST),
        ("fund", 10_000_000, 10_000_000),
    ]
    assert lines[1]["ref"] == "fund-1"

    unkeyed = owner.agents.fund(agent["id"], 1_000)
    assert re.fullmatch(r"sdk-fund-[0-9a-f-]{36}", unkeyed["idempotency_key"])
    assert seen[-1]["idem"] == unkeyed["idempotency_key"]

    assert owner.agents.set_rules(agent["id"], {"max_per_request_ulxc": 1_000_000})["status"] == "applied"
    assert owner.agents.rules(agent["id"])["max_per_request_ulxc"] == 1_000_000

    with pytest.raises(AgentWalletError, match=r"Lens answered 404: .*no such agent"):
        owner.agents.fund("agt_nope", 1)
