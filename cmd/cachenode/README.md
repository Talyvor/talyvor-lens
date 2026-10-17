# talyvor-cachenode

An operator runs this binary to offer cache capacity to the Lens network. It registers with Lens, sends a heartbeat every minute and serves a small HTTP API backed by the operator's own Redis.

## Who may call it

**Nothing in Lens calls the `/cache` routes today.** Lens registers the node and receives its heartbeats, but it does not yet send cache reads or writes to it. The node secret is therefore the only thing that protects these routes.

- The node makes its secret when it registers and keeps it in its state file (`~/.talyvor-cachenode/state.json`, or `TALYVOR_CACHENODE_STATE`).
- Only the holder of that secret may call `/cache/{key}` and `/stats`: the operator's own tools, and Lens once it routes cache traffic here. Every request must send it as `X-Node-Secret`. Without it the answer is 401.
- `/health` needs no secret, so monitoring can probe it.
- A node with no secret will not start. For local development only, set `TALYVOR_NODE_DEV_NO_SECRET=true` to start and serve without one. `talyvor-node` and `talyvor-embednode` follow the same rule.

## Workspaces

Every entry records the workspace that wrote it. Each `/cache/{key}` request must name its workspace in `X-Workspace-ID` (400 without it):

| Route | Another workspace's key |
|---|---|
| `GET /cache/{key}` | 404, as if the key did not exist, unless `CACHE_NODE_SHARE=true` |
| `POST /cache/{key}` | 403: a workspace cannot overwrite another workspace's entry, or write with an `owner_workspace` that is not its own |
| `DELETE /cache/{key}` | 403 |

## Running

`talyvor-cachenode help` lists the commands and environment variables.
