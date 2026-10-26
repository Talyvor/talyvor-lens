/**
 * Type definitions and header constants for the Talyvor Lens SDK.
 *
 * Keep the HEADER_* exports in sync with the Go server's handlers in
 * internal/proxy and internal/workspace — if the server adds a new
 * X-Talyvor-* header, mirror it here so SDK users can set it.
 */

export const HEADER_AUTHORIZATION = "Authorization";
export const HEADER_WORKSPACE = "X-Talyvor-Workspace";
export const HEADER_TEAM = "X-Talyvor-Team";
export const HEADER_FEATURE = "X-Talyvor-Feature";
export const HEADER_SESSION = "X-Talyvor-Session";
export const HEADER_AGENT = "X-Talyvor-Agent";
export const HEADER_BRANCH = "X-Talyvor-Branch";
export const HEADER_PR = "X-Talyvor-PR";
export const HEADER_COMMIT = "X-Talyvor-Commit";
export const HEADER_REPOSITORY = "X-Talyvor-Repository";

/** Aggregate of the optional attribution context fields. */
export interface AttributionContext {
  workspaceId?: string;
  team?: string;
  feature?: string;
  sessionId?: string;
  agentName?: string;
  branch?: string;
  prNumber?: string;
  commit?: string;
  repository?: string;
}

// The agent wallet (B19.9/B19.18): the JSON Lens's agent tools answer with. Amounts are µLXC.

export interface AgentAccount {
  id: string;
  name: string;
  balance_ulxc: number;
  spent_ulxc: number;
  keys: string[];
  created_at: string;
  paused_at?: string;
  paused_reason?: string;
  owner_user_id: string;
  verified: boolean;
}

export interface AgentRules {
  max_per_request_ulxc: number;
  hourly_limit_ulxc: number;
  daily_limit_ulxc: number;
  weekly_limit_ulxc: number;
  monthly_limit_ulxc: number;
  model_daily_limits_ulxc: Record<string, number>;
  requests_per_minute: number;
  approval_above_ulxc: number;
  allowed_models: string[];
  allowed_providers: string[];
  allowed_payees: string[];
  blocked_payees: string[];
  payee_daily_limits_ulxc: Record<string, number>;
  payee_monthly_limits_ulxc: Record<string, number>;
  payee_allowances: Record<string, { amount_ulxc: number; per: "day" | "month" }>;
  max_commitment_ulxc: number;
  allowed_licences: string[];
  may_subscribe: boolean;
  active_from: string;
  active_until: string;
  timezone: string;
  pause_on_unusual_spend: boolean;
}

export interface AgentBalance {
  agent: AgentAccount;
  rules: AgentRules;
  workspace_paused: boolean;
}

export interface AgentApproval {
  id: string;
  agent_id: string;
  amount_ulxc: number;
  model: string;
  reason?: string;
  status: "pending" | "approved" | "denied" | "used";
  created_at: string;
  decided_at?: string;
}

export interface AgentPayment {
  entry_id: string;
  from_agent_id: string;
  to_agent_id: string;
  amount_ulxc: number;
  from_balance_ulxc: number;
  to_balance_ulxc: number;
  memo?: string;
}

export interface ReceiptPosting {
  posting_id: number;
  /** workspace | spend | agent:<id> */
  account: string;
  amount_ulxc: number;
}

export interface AgentReceipt {
  entry_id: string;
  kind: string;
  ref?: string;
  at: string;
  postings: ReceiptPosting[];
}

// The owner's side (B28.437): what Lens's /v1/workspaces/{ws}/agents routes answer with.

export interface AgentKey {
  agent_id: string;
  /** The raw key — shown once. */
  key: string;
  id: string;
  prefix: string;
  warning: string;
}

export interface AgentStatementLine {
  entry_id: string;
  /** fund | withdraw | spend | hold | settle | release | pay */
  kind: string;
  amount_ulxc: number;
  /** workspace | spend | agent:<id> */
  counterparty: string;
  ref?: string;
  balance_after_ulxc: number;
  at: string;
}

export interface AgentStatement {
  agent_id: string;
  lines: AgentStatementLine[];
}
