"""Type definitions and header constants for the Talyvor Lens SDK.

The HEADER_* constants are the single source of truth for the wire-level
header names. Keep them in sync with the Go server-side handlers in
internal/proxy and internal/workspace — if the server adds a new
X-Talyvor-* header, mirror it here so SDK users can set it via kwargs.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Optional, TypedDict

HEADER_AUTHORIZATION = "Authorization"
HEADER_WORKSPACE = "X-Talyvor-Workspace"
HEADER_TEAM = "X-Talyvor-Team"
HEADER_FEATURE = "X-Talyvor-Feature"
HEADER_SESSION = "X-Talyvor-Session"
HEADER_AGENT = "X-Talyvor-Agent"
HEADER_BRANCH = "X-Talyvor-Branch"
HEADER_PR = "X-Talyvor-PR"
HEADER_COMMIT = "X-Talyvor-Commit"
HEADER_REPOSITORY = "X-Talyvor-Repository"


@dataclass(frozen=True)
class AttributionContext:
    """Aggregates the request-level attribution fields a caller can set.

    Frozen so two LensClients constructed from the same context can't
    accidentally share mutable state.
    """

    workspace_id: str = "default"
    team: str = ""
    feature: str = ""
    session_id: str = ""
    agent_name: str = ""
    branch: str = ""
    pr_number: str = ""
    commit: str = ""
    repository: str = ""

    def is_empty(self) -> bool:
        """True when no attribution fields beyond the default workspace are set."""
        return (
            self.workspace_id in ("", "default")
            and not any(
                (
                    self.team,
                    self.feature,
                    self.session_id,
                    self.agent_name,
                    self.branch,
                    self.pr_number,
                    self.commit,
                    self.repository,
                )
            )
        )


# The agent wallet (B19.9/B19.18): the JSON Lens's agent tools answer with. Amounts are µLXC.


class AgentAccount(TypedDict, total=False):
    id: str
    name: str
    balance_ulxc: int
    spent_ulxc: int
    keys: list[str]
    created_at: str
    paused_at: str
    paused_reason: str
    owner_user_id: str
    verified: bool


class AgentRules(TypedDict, total=False):
    max_per_request_ulxc: int
    hourly_limit_ulxc: int
    daily_limit_ulxc: int
    weekly_limit_ulxc: int
    monthly_limit_ulxc: int
    model_daily_limits_ulxc: dict[str, int]
    requests_per_minute: int
    approval_above_ulxc: int
    allowed_models: list[str]
    allowed_providers: list[str]
    allowed_payees: list[str]
    blocked_payees: list[str]
    payee_daily_limits_ulxc: dict[str, int]
    payee_monthly_limits_ulxc: dict[str, int]
    payee_allowances: dict[str, dict[str, object]]
    max_commitment_ulxc: int
    allowed_licences: list[str]
    may_subscribe: bool
    active_from: str
    active_until: str
    timezone: str
    pause_on_unusual_spend: bool


class AgentBalance(TypedDict):
    agent: AgentAccount
    rules: AgentRules
    workspace_paused: bool


class AgentApproval(TypedDict, total=False):
    id: str
    agent_id: str
    amount_ulxc: int
    model: str
    reason: str
    status: str  # pending | approved | denied | used
    created_at: str
    decided_at: str


class AgentPayment(TypedDict, total=False):
    entry_id: str
    from_agent_id: str
    to_agent_id: str
    amount_ulxc: int
    from_balance_ulxc: int
    to_balance_ulxc: int
    memo: str


class ReceiptPosting(TypedDict):
    posting_id: int
    account: str  # workspace | spend | agent:<id>
    amount_ulxc: int


class AgentReceipt(TypedDict, total=False):
    entry_id: str
    kind: str
    ref: str
    at: str
    postings: list[ReceiptPosting]


# The owner's side (B28.437): what Lens's /v1/workspaces/{ws}/agents routes answer with.


class AgentKey(TypedDict):
    agent_id: str
    key: str  # the raw key — shown once
    id: str
    prefix: str
    warning: str


class AgentStatementLine(TypedDict, total=False):
    entry_id: str
    kind: str  # fund | withdraw | spend | hold | settle | release | pay
    amount_ulxc: int
    counterparty: str  # workspace | spend | agent:<id>
    ref: str
    balance_after_ulxc: int
    at: str


class AgentStatement(TypedDict):
    agent_id: str
    lines: list[AgentStatementLine]
