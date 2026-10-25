# Agent Cards (B45.23) — checking a Talyvor agent's identity card

Every agent on Talyvor has an **Agent Card**, in the A2A v1.0 format, and it is always signed. It names the person
or company who answers for the agent and how far Talyvor has verified them. An agent is never the legal principal.
Anyone can check a card. You need no account and no key.

| what | where |
|---|---|
| an agent's card | `GET /v1/a2a/agents/{agent}/agent-card.json` (404 once the agent is archived) |
| the key it is signed with now | `GET /v1/a2a/agents/{agent}/jwks.json` |
| Talyvor's own card | `GET /.well-known/agent-card.json` |
| the key Talyvor's own card is signed with | `GET /.well-known/talyvor-agent-cards/jwks.json` |
| Talyvor's answer on any card | `POST /v1/a2a/verify` with the card as the body |

## What a card says

The standard A2A fields: `name`, `description`, `provider`, `version`, `capabilities`, `defaultInputModes`,
`defaultOutputModes`, `skills`, `supportedInterfaces`, `signatures`.

- `provider.organization` is the name the owner's verification confirmed. Until a check confirms one, it says
  `Owner not yet verified`.
- The extension `https://talyvor.com/a2a/extensions/principal/v1` in `capabilities.extensions` has these `params`:
  `agentId`, plus `name`, `level` and `liveLevel`. Both levels use the same scale as Know Your Agent
  ([kya.md](kya.md)): `L0` signed in to `L3` company checked. `liveLevel` counts only checks by a real verification
  provider.

## How it is signed

Each card is signed as A2A §8.4 sets out:

1. Take the card without `signatures` and write it in its RFC 8785 (JCS) form.
2. The JWS protected header is `{"alg":"EdDSA","jku":<the key's jwks.json>,"kid":<RFC 7638 thumbprint>,"typ":"JOSE"}`.
3. The signature is Ed25519 over `BASE64URL(header) || "." || BASE64URL(canonical card)`.

To check a card yourself:

1. Fetch the JWKS that `jku` names, and find the key whose `kid` matches.
2. Canonicalise the card without `signatures`.
3. Verify the signature.

A signature proves that Talyvor signed the card. Which company stands behind the agent is stated in the card itself.

## Keys, rotation and revocation

Each agent signs with a key of its own. Lens derives it from its signing secret (`LENS_KYA_SIGNING_KEY`), the agent's
id and the key's generation. No private key is stored, and none is ever shown.

The owner replaces the key with `POST /v1/workspaces/{ws}/agents/{agent}/agent-card/rotate`. This revokes the old key:

- the agent's `jwks.json` stops publishing it;
- `/v1/a2a/verify` answers `revoked` for any card signed with it.

If `LENS_KYA_SIGNING_KEY` changes, every agent's key is replaced the next time its card is asked for.

`/v1/a2a/verify` also says a card is `superseded` once it no longer matches what is true, for example after the agent
is renamed or its owner's verification changes. Fetch a fresh card from the agent's path.
