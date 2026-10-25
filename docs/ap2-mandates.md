# Approvals as AP2 v0.2 mandates, and agents approving agents

B45.28. Code: `internal/ap2`, `internal/economy/agent_approvers.go`, `cmd/lens/agent_approvers_handler.go`.

## What a workspace owner can do

- **Let one agent approve another's requests.** The owner picks an approver agent for an agent and signs on the
  Talyvor app (a passkey or a device key). From then on the approver may approve that agent's requests up to its own
  limit: the lower of its `max_per_request_ulxc` and `approval_above_ulxc`. An approver with neither approves nothing.
  It approves within its daily limit too, counted over what it approved that day. Anything above those limits still
  goes to the owner.
- **Approve as before.** An approval the owner signs is kept as an AP2 v0.2 closed Payment Mandate.
- **Show what proves any approval.** `GET /v1/workspaces/{wsID}/agents/approvals/{id}/evidence`.

## The formats

Both mandates are SD-JWTs (RFC 9901), with the JWS signed ES256 and `typ` `dc+sd-jwt`. The disclosures follow it,
each ending in `~`; there is no key binding JWT. Talyvor signs, as the operator of the Trusted Surface (the Talyvor
app). The owner's own signature is carried inside as the `trusted_surface` disclosure: the device key's signature over
its challenge, or the passkey's WebAuthn assertion. Amounts are `{"currency": "LXC", "amount": <µLXC>}`: LXC, counted
in millionths.

| | vct | in the clear | disclosed |
|---|---|---|---|
| The owner approves one request | `mandate.payment.1` | `iss`, `iat`, `jti` | `transaction_id` (the approval's id), `payee`, `payment_amount`, `payment_instrument` (the agent's wallet), `trusted_surface` |
| The owner lets an agent approve | `mandate.payment.open.1` | `iss`, `iat`, `jti`, `cnf.jwk` (the approver's Agent Card key) | `constraints` (`payment.amount_range` up to the limit), `talyvor_delegation` (the approver and the agent it approves for), `trusted_surface` |

AP2 v0.2 does not cover one agent delegating to another, so an approver agent's approval is not a mandate. It is
Talyvor-signed evidence: a compact JWS, ES256, with `typ` `talyvor-agent-approval+jwt`. It carries `approval` (the id,
the amount and the payee), `requester` and `approver` (each an `agent_id` and a `name`; the approver also has the
`limit_ulxc` it approved within), and `mandate`. `mandate` is the open mandate the approver ran under: its `id`, `vct`
and `hash`, which is the base64url SHA-256 of the whole SD-JWT.

## Checking one

1. Fetch the key from `/.well-known/talyvor-ap2/jwks.json` (P-256, matched by `kid`).
2. Check the JWS: `alg` must be `ES256` and `typ` as above.
3. For an SD-JWT, check that the base64url SHA-256 of each disclosure is listed in `_sd`, then read each disclosure as
   `[salt, name, value]`.

`ap2.Verify` and `ap2.VerifyJWT` do exactly this.
