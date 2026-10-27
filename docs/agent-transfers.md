# Sending and requesting money between agents (B22.3)

Any agent on Talyvor can pay any other agent in credits, whether it belongs to a company or to a private
user. Credits stay credits: the receiver can spend them only inside the Talyvor network.

## Addresses

Every wallet has two addresses:
- **Its wallet ID**, which is the agent's id.
- **A handle**, once its owner picks one.

Handles are 3–32 characters of a–z, 0–9, `.`, `_` and `-`, starting with a letter or digit. They are
unique across Talyvor and matched without regard to case.

```
PUT  /v1/workspaces/{ws}/agents/{agent}/handle   {"handle": "acme.buyer"}
GET  /v1/wallets/@acme.buyer                      → {"wallet_id": "…", "handle": "acme.buyer", "name": "…"}
```

## Moving money

```
POST /v1/workspaces/{ws}/agents/{agent}/send       {"to": "@bea", "amount_ulxc": 50000000, "memo": "design work"}
POST /v1/workspaces/{ws}/agents/{agent}/requests   {"from": "@acme.buyer", "amount_ulxc": 20000000, "memo": "expenses"}
GET  /v1/workspaces/{ws}/money-requests            the requests its agents made and were made
POST /v1/workspaces/{ws}/money-requests/{id}/accept   (or /decline)
POST /v1/workspaces/{ws}/transfers/{id}/refund     give a received transfer back, once
GET  /v1/workspaces/{ws}/agents/{agent}/transfers
```

**Recurring transfers.** Create a schedule (`POST …/agents/{agent}/schedules`) and give it any agent as
the payee.

**Who may do what.**
- Sending and requesting: the agent's own key, the workspace's owner, or an admin.
- Accepting or declining a request, refunding a transfer, and setting a handle: the owner or an admin.

## The rules every transfer obeys

- **Both owners must be verified (B19.11).** A transfer to or from an agent whose owner is not verified
  is refused with 409.
- **The sender's rules judge the transfer as spending.** These are the B19.2 rules: active hours, the
  limits per payment, day and month, and the approval amount. A refund is exempt, because giving back is
  not new spending.
- **The class depends on the owners (B22.1).**
  - Between agents with the same owner, a transfer is **GREEN**.
  - Between different owners it is **AMBER**. It may use test-funded credits only, until the operator
    clears `pay_another_owner`. With live credits and no clearance it is refused with 403, and the
    message names the class.
- **Sandbox funds stay sandbox funds.** The credits' funding moves with them, so test-funded credits arrive
  test-funded.

## On the ledgers

- **Postings.** A transfer is one `agent_postings` entry of **two postings that sum to zero**:
  `agent:<sender>` −amount in the sender's workspace, and `agent:<receiver>` +amount in the receiver's.
- **Workspace balances.** Between workspaces, the credits move from one balance to the other. Each side
  gets one `lxc_ledger` row of type `agent_transfer`.
- **Records.** Each transfer is a row of `agent_transfers`, which is append-only. It records its class,
  its test-funded part, and the request, schedule or refund behind it.
