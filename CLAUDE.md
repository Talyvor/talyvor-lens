## Working rules for this repo
- Recon first. Treat findings as hypotheses and check them against the live code.
- Red first: a test that fails for the right reason, then the fix, then the raw output of both.
- Money and authorisation code stops before merge for review, like workflow and security-header changes. Privacy, tenancy and migration changes carry their own proof (below) and merge the normal way unless they touch money or authorisation.
- Local green is not CI green. For money and tenancy, run the exact CI command against a database built from zero.
- Rounding: a charge or fee rounds up. A payout, mint or yield rounds down. Tax (VAT) rounds to the nearest minor unit, half up, and dollars shown beside LXC round to the nearest cent. Idempotency keys carry every identity they protect.
- Every query is scoped to its workspace. Prove the cross-tenant case is refused.
- Re-check migration numbers right before merging.
- Never forward a caller's credentials to a third party. Never log a secret.
- Record a merge only with the real squash SHA from GitHub.
