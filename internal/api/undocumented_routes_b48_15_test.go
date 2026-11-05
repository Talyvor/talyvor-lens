package api

// B48.15: POST …/agents/{id}/recovery-signer/challenge, PUT …/recovery-signer, POST …/recover/challenge and …/recover,
// POST …/rotate-signer, POST …/agents/lockdown and POST …/sweep — recovery, rotation, lockdown and sweep for a crypto
// agent's smart account — undocumented beside B48.3's spend-permission routes, until the wallet screens settle them.
func init() { undocumentedRoutes["B48.15"] = 7 }
