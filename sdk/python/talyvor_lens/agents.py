"""Agents — the workspace owner's side of Agent Wallets at Talyvor Lens.

Called with the owner's key (a workspace key with the keys or admin scope):
create an agent, ask to fund its wallet from the workspace's LXC, set its
spending rules, issue the agent a key of its own, and read its page and
statement. The agent then calls through Lens with that key, and every model
call is charged to its wallet, within its spending rules, before the provider
is called.

Funding moves nothing by itself (B28.73): ``fund`` answers an approval link,
and the agent is funded once a person signed in to the workspace approves it
there — one posting, its ref the request's idempotency key. A key never
approves its own request. Creating an agent needs its owner signed in: a
workspace key cannot create one.

They are Lens's REST routes under ``/v1/workspaces/{ws}/agents``, in the
client's workspace. A non-2xx answer raises :class:`AgentWalletError`
carrying Lens's status and message.
"""

from __future__ import annotations

import uuid
from typing import Any
from urllib.parse import quote

import httpx

from .agent_wallet import AgentWalletError
from .types import AgentAccount, AgentFundRequest, AgentKey, AgentRules, AgentStatement, FiledRules


class Agents:
    """Reach it as ``LensClient(...).agents``. Amounts are in µLXC (1 LXC = 1,000,000 µLXC).

    Example:
        >>> agents = LensClient(lens_url=url, api_key="tlv_owner_key", workspace_id="acme").agents
        >>> agent = agents.create("researcher")
        >>> request = agents.fund(agent["id"], 10_000_000)
        >>> # ...a person opens request["approval_url"] and approves it; then:
        >>> key = agents.issue_key(agent["id"])["key"]
        >>> # ...the agent calls through Lens with ``key``, then:
        >>> agents.statement(agent["id"])["lines"]
    """

    def __init__(self, lens_url: str, workspace_id: str, headers: dict[str, str], timeout: float = 30.0) -> None:
        self._base = f"{lens_url}/v1/workspaces/{quote(workspace_id, safe='')}/agents"
        self._headers = dict(headers)
        self._timeout = timeout

    def list(self) -> list[AgentAccount]:
        """Every agent in the workspace, with its balance."""
        return self._send("GET", "")["agents"]

    def create(self, name: str) -> AgentAccount:
        """Create an agent. It starts with an empty wallet and no key."""
        return self._send("POST", "", {"name": name})

    def get(self, agent_id: str) -> dict[str, Any]:
        """One agent's page: balance, safe to spend, this month, approvals waiting, latest statement lines."""
        return self._send("GET", f"/{quote(agent_id, safe='')}/summary")

    def fund(self, agent_id: str, amount_ulxc: int, idempotency_key: str = "") -> AgentFundRequest:
        """Ask to move µLXC from the workspace to the agent's wallet.

        Nothing moves yet: answers the request with its ``approval_url``, and the
        agent is funded once a person approves it there. The request is filed
        under ``idempotency_key`` (one is made when none is given): the same key
        again is the same request and, once approved, one posting whose ref is
        that key.
        """
        key = idempotency_key or f"sdk-fund-{uuid.uuid4()}"
        return self._send("POST", f"/{quote(agent_id, safe='')}/fund-requests", {"amount_ulxc": amount_ulxc}, key)

    def fund_request(self, request_id: str) -> AgentFundRequest:
        """A request to fund an agent, as it stands: pending, approved (with the agent's balance) or declined."""
        return self._send("GET", f"/fund-requests/{quote(request_id, safe='')}")

    def rules(self, agent_id: str) -> AgentRules:
        """The agent's spending rules in force."""
        return self._send("GET", f"/{quote(agent_id, safe='')}/rules")

    def set_rules(self, agent_id: str, rules: AgentRules) -> FiledRules:
        """Set the agent's spending rules.

        Rules that only tighten what is in force apply at once (``status`` "applied");
        rules that loosen any of it wait for the owner's passkey (``status`` "pending").
        """
        return self._send("POST", f"/{quote(agent_id, safe='')}/rules/proposals", {"rules": rules})

    def withdraw(self, agent_id: str, amount_ulxc: int, idempotency_key: str = "") -> dict[str, Any]:
        """Take µLXC back from the agent's wallet to the workspace."""
        return self._send("POST", f"/{quote(agent_id, safe='')}/withdraw", {"amount_ulxc": amount_ulxc}, idempotency_key)

    def issue_key(self, agent_id: str, name: str = "") -> AgentKey:
        """Issue the agent a proxy key of its own.

        ``key`` is shown once: a client built with it calls models on the
        agent's wallet and reaches ``.wallet``.
        """
        return self._send("POST", f"/{quote(agent_id, safe='')}/keys", {"name": name} if name else {})

    def statement(self, agent_id: str, limit: int | None = None) -> AgentStatement:
        """The agent's account, newest first: funding, model spend, payments (at most 1000 lines)."""
        q = f"?limit={limit}" if limit else ""
        return self._send("GET", f"/{quote(agent_id, safe='')}/statement{q}")

    def _send(self, method: str, path: str, body: dict[str, Any] | None = None, idempotency_key: str = "") -> Any:
        headers = dict(self._headers)
        if idempotency_key:
            headers["Idempotency-Key"] = idempotency_key
        what = f"{method} {self._base}{path}"
        try:
            resp = httpx.request(method, f"{self._base}{path}", json=body, headers=headers, timeout=self._timeout)
        except httpx.HTTPError as exc:
            raise AgentWalletError(f"{what}: {exc}") from exc
        if not 200 <= resp.status_code < 300:
            raise AgentWalletError(f"{what}: Lens answered {resp.status_code}: {resp.text.strip()}")
        try:
            return resp.json()
        except ValueError as exc:
            raise AgentWalletError(f"{what}: unreadable answer: {resp.text!r}") from exc
