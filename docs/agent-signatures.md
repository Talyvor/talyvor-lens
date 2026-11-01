# Signing requests to Talyvor as an outside agent (B45.24)

Talyvor reads the HTTP message signature (RFC 9421) an outside agent sends, and puts every request at one of three
trust levels. An approval card shows the level in these words:

| Level        | Words on the card                                 | When                                                                 |
|--------------|---------------------------------------------------|----------------------------------------------------------------------|
| `verified`   | Verified agent of *Acme Inc.*                     | the signature holds AND its domain belongs to a business Talyvor has checked |
| `signed`     | Signed by *agent.acme.com* (business not checked) | the signature holds; it proves only that the domain published the key |
| `unverified` | Unverified agent                                  | no signature                                                          |

A signature that is present and does not hold is refused with 401 and the reason.

## Your keys

Publish your Ed25519 public keys as a JWKS at `https://<your domain>/.well-known/http-message-signatures-directory`
(Web Bot Auth). Sign with `alg="ed25519"`, `keyid` = the key's RFC 7638 JWK thumbprint, and `tag="web-bot-auth"`, and
send `Signature-Agent: "https://<your domain>"`. Talyvor caches your directory for 5 minutes.

## Two profiles

**Web Bot Auth** — on the A2A endpoints (`/.well-known/agent-card.json`, `/v1/a2a/...`): cover `@authority` (or
`@target-uri`) and `signature-agent`; `created` and `expires` set, at most 24 hours apart.

**Money** — on money endpoints, Talyvor's own profile:

- cover `@method`, `@target-uri` (the full URL, `https://lens.talyvor.com/...` with its query), `content-digest` and
  `signature-agent`, and send `Content-Digest` (RFC 9530, `sha-256` or `sha-512`) of the exact body;
- `expires` at most 5 minutes after `created`, and at most 5 minutes from now;
- a `nonce`: Talyvor accepts each nonce once per key.

## Check your signature

    GET  https://lens.talyvor.com/v1/a2a/signature-check          under Web Bot Auth
    POST https://lens.talyvor.com/v1/a2a/signature-check/money    under the money profile

Each answers `{"level", "label", "domain", "business", "key_id", "profile"}`, or 401 with why. Every A2A response
also carries `Talyvor-Agent-Trust: <level>`.

To become `verified`, ask Talyvor to check the business behind your signing domain.

## Ask to be paid

    POST https://lens.talyvor.com/v1/a2a/agents/<agent id>/payment-requests    under the money profile, or unsigned
    Idempotency-Key: <your key>
    {"items": [{"payee_id": "<the payee id the owner gave you>", "currency": "GBP", "amount_minor": 12500, "reference": "INV-1"}]}

The request lands in the owner's approvals with your trust level, and nothing is paid until the owner approves it. An
unsigned request is shown as "Unverified agent". Bank details are refused: you are paid to the payee the owner saved.
The answer is `{"id", "status", "agent_trust"}`.
