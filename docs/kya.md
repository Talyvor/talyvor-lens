# Know Your Agent (B30.5) — checking a Talyvor agent's credential

Every agent on Talyvor can show a **credential**: a signed statement of who the agent is, who answers for it and
how far they are verified, what it may do with live money, and a summary of its spending limits. Any platform can
check one with Talyvor's published keys — no account, no API key.

## Where an agent gets it

- An agent, with its own key, calls the MCP tool **`wallet_credential`**.
- Its owner reads it at `GET /v1/workspaces/{workspace}/agents/{agent}/credential`.

Both return the same thing:

```json
{
  "id": "kya_6f1c…",
  "credential": "eyJhbGciOiJFZERTQSIsImtpZCI6Ik…",
  "claims": { … },
  "issued_at": "2026-10-08T12:00:00Z",
  "expires_at": "2026-10-09T12:00:00Z",
  "jwks_url": "/.well-known/talyvor-kya/jwks.json",
  "verify_url": "/v1/kya/verify"
}
```

The agent hands the `credential` string to the platform that asks for it. A frozen or archived agent has no
credential: the call is refused (`409` on the owner's route) and says why.

## The format

The credential is a JWT (RFC 7519) signed with **Ed25519** (`alg` `EdDSA`, RFC 8037).

Header:

| field | value |
|---|---|
| `alg` | `EdDSA` |
| `typ` | `kya+jwt` |
| `kid` | the signing key's RFC 7638 thumbprint; find it in the JWKS |

Claims:

| claim | meaning |
|---|---|
| `iss` | always `talyvor` |
| `sub` | the agent's id |
| `jti` | the credential's id — the revocation list names it |
| `iat`, `nbf`, `exp` | issued, valid from, expires (seconds since the epoch). A credential lasts `LENS_KYA_CREDENTIAL_TTL`, 24 hours by default |
| `agent.id`, `agent.name` | the agent |
| `owner.workspace_id` | the workspace that owns the agent and answers for it |
| `owner.name` | the name the owner's verification confirmed — a person's or a company's; absent before any |
| `owner.level` | the verification level the owner's checks reach: `L0` signed in, `L1` email and phone confirmed, `L2` identity checked, `L3` company checked (number, directors, people with significant control) |
| `owner.live_level` | the level counting only checks by a real verification provider. Talyvor's test provider's passes count toward `level` and never toward `live_level` |
| `capabilities` | every wallet capability, each with `money`: `live` when this agent may use it with real money now, `test` when it takes sandbox funds only |
| `limits` | a summary of the agent's spending rules in force now, temporary raises included, in µLXC (1 LXC = 1,000,000 µLXC). An absent limit is not set |

`limits` may hold:

| field | meaning |
|---|---|
| `max_per_request_ulxc` | the most one request or payment may cost |
| `hourly_limit_ulxc`, `daily_limit_ulxc`, `weekly_limit_ulxc`, `monthly_limit_ulxc` | the most it may spend in each period |
| `approval_above_ulxc` | a person approves anything above this |
| `max_commitment_ulxc` | the most one marketplace commitment may cost |
| `requests_per_minute` | the most model requests a minute |
| `active_hours` | the hours it may spend, e.g. `09:00-17:00 Europe/London` |
| `models_listed` | `true` when it may use only the models or providers its owner listed |
| `payees_listed` | `true` when it may pay only the payees its owner listed |

An example payload:

```json
{
  "iss": "talyvor",
  "sub": "agt_3b9d…",
  "jti": "kya_6f1c…",
  "iat": 1791460800, "nbf": 1791460800, "exp": 1791547200,
  "agent": { "id": "agt_3b9d…", "name": "Research bot" },
  "owner": { "workspace_id": "ws_81a2…", "name": "Acme Ltd", "level": "L3", "live_level": "L3" },
  "capabilities": [
    { "capability": "spend_on_talyvor", "money": "live" },
    { "capability": "fx", "money": "test" }
  ],
  "limits": { "max_per_request_ulxc": 1000000, "daily_limit_ulxc": 5000000, "approval_above_ulxc": 2000000 }
}
```

## Checking one

### Offline, with the published keys

1. Fetch `https://<talyvor api>/.well-known/talyvor-kya/jwks.json`. Each key is an OKP JWK on `Ed25519`
   (`kty`, `crv`, `x`, `kid`, `alg` `EdDSA`, `use` `sig`). It lists the key signing now and every key a credential
   not yet expired was signed with, so cache it and fetch it again when a `kid` is not in your copy.
2. Find the key whose `kid` is the credential header's `kid`, and verify the signature with it. Accept only
   `alg` `EdDSA`.
3. Check `iss` is `talyvor` and that now is between `nbf` and `exp`.
4. Fetch `https://<talyvor api>/.well-known/talyvor-kya/revoked.json` and refuse the credential when its `jti` is
   on it:

   ```json
   { "issuer": "talyvor", "generated_at": "2026-10-08T12:05:00Z",
     "revoked": [ { "jti": "kya_6f1c…", "reason": "frozen", "revoked_at": "…", "expires_at": "…" } ] }
   ```

   The list holds each revoked credential until it would have expired. Do not cache it for long.

In Go, with `github.com/golang-jwt/jwt/v5`:

```go
claims := jwt.MapClaims{}
_, err := jwt.ParseWithClaims(credential, claims, func(t *jwt.Token) (any, error) {
	for _, k := range jwks.Keys {
		if k.Kid == t.Header["kid"] && k.Kty == "OKP" && k.Crv == "Ed25519" {
			x, err := base64.RawURLEncoding.DecodeString(k.X)
			return ed25519.PublicKey(x), err
		}
	}
	return nil, errors.New("unknown kid")
}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer("talyvor"), jwt.WithExpirationRequired())
```

### Online, by asking Talyvor

`POST https://<talyvor api>/v1/kya/verify` with `{"credential": "<the credential>"}` answers `200`:

```json
{ "valid": true, "claims": { … } }
{ "valid": false, "reason": "revoked: rules changed", "claims": { … } }
```

Besides the signature, times and revocation list, it checks the credential still says what is true of the agent
now — one that no longer does is revoked as it answers. It is the most current answer; the offline check can lag
by as long as you cache the revocation list.

## When a credential is revoked

| reason | when |
|---|---|
| `frozen` | the agent was frozen — on its own, by its unusual spend, or with every agent of its owner |
| `archived` | the agent was archived |
| `rules changed` | its owner changed its spending rules, or raised or ended a temporary raise of a limit |
| `superseded` | anything else it states changed — its name, its owner's verification, a capability's money — and the agent was given a new credential |
| `deleted` | its owner's workspace was deleted |

Freezing, archiving and changing rules revoke the credential as they happen. After a rule change the agent's next
credential states the new limits. A frozen agent is given a new credential once its owner resumes it.
