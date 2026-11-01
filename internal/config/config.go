package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/talyvor/lens/internal/envelope"
	"strings"
)

// ── W4.6.1 step 4: session-key lifetimes ───────────────────────────────────────

const (
	// DefaultSessionKeyTTL is how long a minted session key lives when the operator sets no
	// ceiling and the caller asks for nothing. One hour: long enough that a chat is not
	// interrupted, short enough that a key leaked from a browser is stale before it is useful.
	//
	// ⚠ IT IS NOT THE ONLY BOUND, and it is the least important of the three. The binding one is
	// the caller's own remaining session life — a session key can never outlive the sign-in that
	// created it, whatever this says.
	DefaultSessionKeyTTL = time.Hour

	// MaxSessionKeyTTL is the hard ceiling in CODE. A configured value above it is REFUSED at
	// load rather than clamped, so a config file cannot disagree with the running process.
	//
	// 12 hours is chosen to be shorter than auth.DefaultTokenTTL (24h) — a credential derived
	// from a session must not have a longer maximum than the session itself, or the derived
	// credential is the wider one and the whole exercise is inverted.
	MaxSessionKeyTTL = 12 * time.Hour
)

type Config struct {
	ListenAddr      string
	RedisURL        string
	DatabaseURL     string
	NatsURL         string
	OpenAIAPIKey    string
	AnthropicAPIKey string
	// B40.10 — speech (internal/proxy/speech.go): Deepgram's key and base URL (default its EU endpoint).
	DeepgramAPIKey string
	DeepgramURL    string

	// ModelWatchEnabled runs the catalog-drift poller (internal/modelwatch): asks each provider what it
	// serves and alerts on anything the catalog cannot price. DETECTION ONLY — it never sets a price,
	// for the reasons in that package's doc comment (no provider publishes rates in any API).
	ModelWatchEnabled  bool
	ModelWatchInterval time.Duration
	// OperatorAlertWebhookURL/Secret is the sink that makes an operator alert reach a PERSON. Both must
	// be set or the sink is nil — and a nil sink is an ERROR at boot, not a silent no-op, because an
	// alert nobody receives is worse than no alert: it reads as covered.
	// Envs: LENS_OPERATOR_ALERT_WEBHOOK_URL, LENS_OPERATOR_ALERT_WEBHOOK_SECRET.
	OperatorAlertWebhookURL    string
	OperatorAlertWebhookSecret string
	GoogleAPIKey               string
	EmbeddingModel             string
	EmbeddingBaseURL           string // LENS_EMBEDDING_BASE_URL override; empty = OpenAI default. Process-env only — no request input can set it (mirrors VLLMBaseURL; offline trial harness).
	SemanticThreshold          float64
	MaxCacheTTL                time.Duration
	LogLevel                   string
	OllamaURL                  string

	// SemanticCacheRetention is the single sliding window for Postgres
	// semantic-cache rows (prompt_embeddings). Every cache hit bumps
	// updated_at = NOW(), so the window restarts on each use. It governs BOTH
	// halves of a row's life:
	//   - SERVE: semanticSelectSQL only returns rows with
	//     updated_at > NOW() − retention, so an entry stays servable for the
	//     full window and each hit resets it.
	//   - STORE: a background sweeper DELETEs any row past the window, bounding
	//     the otherwise-unbounded growth of the table and of its ivfflat
	//     similarity index from one-off prompts that are never reused.
	// Because serving and storage share one window, a cached answer can be
	// served until it is this old — a deliberate reuse-over-freshness tradeoff;
	// lower this single value to tighten both together.
	//
	// Default 0 (B21.2, decided 28 Sep 2026): a stored answer is never deleted
	// because of its age — it is served regardless of age and never swept, private
	// and pooled alike. What deletes one is quality eviction and deletion by the
	// workspace. Env: LENS_SEMANTIC_CACHE_RETENTION (Go duration; units h/m/s —
	// NOT "d"; "504h" is 21 days) lets a self-hosted operator turn the window on.
	SemanticCacheRetention time.Duration

	// SemanticCacheSweepInterval is how often the retention sweeper runs.
	// Default 1h. Must be > 0. Env: LENS_SEMANTIC_CACHE_SWEEP_INTERVAL (Go
	// duration). Only consulted when SemanticCacheRetention > 0.
	SemanticCacheSweepInterval time.Duration

	// U14 audit-trail integrity. All DEFAULT OFF.
	//
	// AuditRetention is the sliding window for the token_events retention sweeper
	// (token_events ONLY — there is no table knob; it can never touch a ledger).
	// Env LENS_AUDIT_RETENTION (Go duration). <= 0 / unset DISABLES the sweeper.
	AuditRetention time.Duration
	// AuditRetentionDatabaseURL (LENS_AUDIT_RETENTION_DATABASE_URL) is the lens_retention role's URL the
	// sweeper deletes with (B39.6); empty = the main pool, as before. The lens_app role cannot delete.
	AuditRetentionDatabaseURL string

	// AuditExportURL / AuditExportInterval drive the leader-elected off-box export
	// loop (reuses the audit webhook exporter). Empty URL = no loop (default off).
	// Env: LENS_AUDIT_EXPORT_URL, LENS_AUDIT_EXPORT_INTERVAL (Go duration, default 1h).
	AuditExportURL      string
	AuditExportInterval time.Duration

	// AuditRequireExportBeforePrune (U14 #187) gates the retention sweeper so it
	// deletes a token_events row ONLY once it is at/below the off-box export
	// watermark (audit_export_state.last_exported_at) — proof-of-export-before-
	// delete. When export is disabled, the sweep is SKIPPED entirely (it never
	// prunes un-exportable rows). Env LENS_AUDIT_REQUIRE_EXPORT_BEFORE_PRUNE;
	// default false = today's age-only pruning (no behaviour change).
	AuditRequireExportBeforePrune bool

	// WorkspaceReloadInterval (U7b) is how often each replica rebuilds its
	// in-memory workspace-config cache from Postgres, bounding cross-replica
	// staleness of logging policy + the cache-pooling privacy flag. Env:
	// LENS_WORKSPACE_RELOAD_INTERVAL (Go duration). Default 30s (tighter than the
	// budgets 60s — cache-pooling is a privacy decision; the reload is one cheap
	// SELECT). MUST be > 0 (a time.Ticker panics on a non-positive interval).
	WorkspaceReloadInterval time.Duration

	// WorkspaceMaxStaleness is the fail-closed bound for the workspace cache: once a
	// replica has gone this long without a SUCCESSFUL reload (a prolonged DB outage),
	// the consent/privacy accessors (cache_poolable, distill_poolable, distill_policy,
	// logging) return their conservative value instead of a possibly-revoked
	// permissive one. Env: LENS_WORKSPACE_MAX_STALENESS (Go duration). Default 3× the
	// reload interval (90s at the 30s default). MUST be > WorkspaceReloadInterval — a
	// bound <= the interval would trip during normal operation.
	WorkspaceMaxStaleness time.Duration

	// GuardrailsReloadInterval (#189) is how often each replica rebuilds its
	// in-memory custom guardrail-policy cache from Postgres (guardrail_policies),
	// bounding cross-replica staleness of a SECURITY control. Env:
	// LENS_GUARDRAILS_RELOAD_INTERVAL (Go duration). Default 30s. MUST be > 0.
	GuardrailsReloadInterval time.Duration

	// AWS Bedrock — all four fields are optional; an empty AccessKeyID
	// keeps HandleBedrock in 503 graceful-degradation mode. SessionToken
	// is only set when the deployment uses STS / assumed-role creds.
	AWSRegion          string
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	AWSSessionToken    string

	// ProviderSecretKeyring is the envelope-encryption keyring that wraps CUSTOMER provider
	// credentials (BYOK, W6.10). Supplied via LENS_PROVIDER_SECRET_KEK as one or more base64
	// 32-byte keys separated by commas, PRIMARY FIRST (`openssl rand -base64 32`).
	//
	// OPTIONAL, and the posture is copied from talyvor-track's TRACK_INTEGRATION_ENCRYPTION_KEY:
	// unset ⇒ nil ⇒ provider-secret custody is DISABLED and there is no plaintext fallback (Lens
	// still boots); set ⇒ every key validated to exactly 32 decoded bytes AT BOOT, so a wrong
	// length is fail-loud at startup rather than a broken-crypto surprise at the first customer
	// credential. It holds a *Keyring, never key material — see internal/envelope, and TestK5.
	//
	// ⚠ NOTHING MAY PERSIST A PROVIDER SECRET WHILE THIS IS NIL. W6.4/W6.8 build the store; this
	// is the switch they must gate on.
	ProviderSecretKeyring *envelope.Keyring

	// OpenAI-compatible providers: Mistral, Groq, vLLM. All optional.
	// Empty mistral/groq key → 503 on those routes. Empty VLLMBaseURL
	// → 503 on the vLLM route (vLLM has no default endpoint — the
	// operator chooses where to run the inference server).
	MistralAPIKey string
	GroqAPIKey    string
	VLLMBaseURL   string
	VLLMAPIKey    string

	// LocalEndpoints holds the LENS_LOCAL_ENDPOINTS env value,
	// in the format consumed by localrouter.ParseEndpointsConfig:
	//   provider:url:model1,model2;provider:url:model3
	// Empty string means "no multi-endpoint local routing" —
	// the legacy LENS_OLLAMA_URL single-endpoint path still works.
	LocalEndpoints string

	// JWT auth. JWTPrivateKey is a PEM-encoded EC P-256 private key used to
	// sign and verify tokens (ES256). Generate one with:
	//
	//   openssl ecparam -name prime256v1 -genkey -noout | \
	//     openssl pkcs8 -topk8 -nocrypt -out ec-private.pem
	//   export LENS_JWT_PRIVATE_KEY=$(cat ec-private.pem)
	//
	// Empty string is allowed — Lens generates an ephemeral key at startup for
	// single-instance dev; production deployments must persist the key so
	// tokens remain valid across restarts and across instances in an HA cluster.
	// Env: LENS_JWT_PRIVATE_KEY.
	JWTPrivateKey string
	TokenTTL      time.Duration

	// ProvisionSecret is the shared secret gating POST /v1/provision — the narrow
	// route that turns an authenticated identity into a tenant. It exists so the
	// session gateway (the suite BFF) never has to hold LENS_API_KEY: the global
	// admin key makes workspaceAuthorized return true for EVERY workspace and
	// unlocks ~30 admin routes, so a gateway compromise would escalate from one
	// tenant's data to control of every tenant. This credential can create a
	// workspace and mint a session token for it, and nothing else.
	//
	// EMPTY MEANS THE CAPABILITY DOES NOT EXIST: run() does not register the route
	// at all when this is unset, so an unconfigured deployment has no provisioning
	// surface rather than an unauthenticated one. Env: LENS_PROVISION_SECRET.
	ProvisionSecret string

	// SyntheticKey (LENS_SYNTHETIC_KEY) is the operator key for B17.1's synthetic-workspace routes,
	// POST /v1/synthetic/workspaces[/reset]. Unset (the default), they are not registered: 404.
	SyntheticKey string

	// B19.16 — agent approvals signed with a passkey, and a web push when one is filed.
	// WebAuthnRPID (LENS_WEBAUTHN_RP_ID, default app.talyvor.com) is the relying party the owners' passkeys
	// are created for; WebAuthnOrigins (LENS_WEBAUTHN_ORIGINS, comma-separated, default
	// https://app.talyvor.com) the pages a ceremony may come from. VAPIDPrivateKey (LENS_VAPID_PRIVATE_KEY,
	// base64url P-256 scalar; `lens vapid-key` makes one) signs the pushes; unset, no push is sent.
	// VAPIDSubject (LENS_VAPID_SUBJECT) is the contact the push services are given.
	WebAuthnRPID    string
	WebAuthnOrigins []string
	// ApprovalTTL (LENS_APPROVAL_TTL, Go duration, default 24h) is how long an approval lasts (B50.18): an agent's
	// request or a payment request waits that long for its tap, and once approved may be used that long.
	ApprovalTTL time.Duration
	// B40.4 — the phone app's device keys. DeviceAppleAppID (LENS_DEVICE_APPLE_APP_ID, "<team id>.<bundle id>",
	// default empty) is the iPhone app App Attest attestations must be for; empty, no iPhone key registers.
	// DeviceTestAttestation (LENS_DEVICE_TEST_ATTESTATION, default false) accepts device keys with a fake
	// attestation, platform "test" — test-only: never on a server real people use.
	DeviceAppleAppID      string
	DeviceTestAttestation bool
	VAPIDPrivateKey       string
	VAPIDSubject          string
	// B40.15 — native push to the phone app. APNs (iPhones): APNsKey (LENS_APNS_KEY, the .p8 key's PEM or its file's
	// path), APNsKeyID (LENS_APNS_KEY_ID), APNsTeamID (LENS_APNS_TEAM_ID), APNsTopic (LENS_APNS_TOPIC, the bundle id),
	// APNsSandbox (LENS_APNS_SANDBOX, development builds). FCM (Android): FCMCredentials (LENS_FCM_CREDENTIALS, a
	// service account's JSON key or its file's path). Unset, nothing is pushed to that platform and nothing fails.
	APNsKey        string
	APNsKeyID      string
	APNsTeamID     string
	APNsTopic      string
	APNsSandbox    bool
	FCMCredentials string

	// Global rate limits (Item 8). Zero = no global cap.
	//
	// ⚠ THIS USED TO ADD "the per-workspace tier in MultiTierLimiter still applies",
	// and there is no per-workspace tier: main.go builds MultiTierLimiter with
	// exactly one, named "global", then blank-assigns the limiter. Zero here does
	// not fall back to a per-workspace cap. The cap that still applies is the legacy
	// sliding-window limiter's, which is uniform across workspaces (W6.27).
	GlobalRPM       int
	GlobalTPM       int
	BurstMultiplier float64

	// Retry / circuit-breaker tuning (Item 9). Zero values fall
	// back to the library defaults in internal/retry/policy.go.
	RetryMaxAttempts  int
	RetryInitialDelay time.Duration
	RetryMaxDelay     time.Duration
	CBThreshold       int
	CBResetTimeout    time.Duration

	// CachePoolableEnabled is the GLOBAL switch for the Phase-2 Stage 2.0
	// shared-cache governance gate (exact cache). DEFAULT TRUE (it is in the default-on
	// loop in Load, and force-off'd to false whenever EconomyEnabled is false): cross-tenant
	// pooling is impossible unless this is on AND both the contributing and
	// requesting workspaces have cache_poolable=true. Inert by default — the
	// request path is byte-for-byte unchanged when this is off, which on a default
	// deployment means: when the economy is off or the env disables it.
	CachePoolableEnabled bool

	// DistillPoolableEnabled is the GLOBAL switch for cross-tenant DISTILL-cache
	// sharing (the document-artifact analogue of CachePoolableEnabled). DEFAULT
	// TRUE — it sits in the same default-on loop in Load as CachePoolableEnabled
	// above, and is force-off'd to false whenever EconomyEnabled is false. A
	// distill artifact is served only within its producing workspace unless this
	// is on AND both the owner and requester have distill_poolable=true, so the
	// per-workspace consent is what keeps a default deployment from sharing — the
	// GLOBAL switch is not. Only when this is off is the distill cache strictly
	// per-workspace by configuration. Env: LENS_DISTILL_POOLABLE_ENABLED.
	DistillPoolableEnabled bool

	// Pattern mining (Batch 2 Item 5). Deployment-level gate;
	// must AND with per-workspace opt-in for earnings to fire.
	PatternMiningEnabled bool

	// AnnotationMintingEnabled gates the annotation MINT (data-labelling reward).
	// DEFAULT FALSE and intentionally so: unlike the pattern held mint (held →
	// examined by the single-party clearer → settled after 72h) the annotation
	// reward is credited SPENDABLE-IMMEDIATELY (CreditTx), with no holdback and no
	// examiner, so a gaming pattern cannot be clawed back before it is spendable.
	// It is a protocol-issuance mint (labelling labour), bounded by the earn-verify
	// floor + the 1000-LENS/24h rate cap + the annotator stake + consensus, but it
	// must be an EXPLICIT opt-in, not armed by the bare economy master switch — so
	// the GA on-by-default set is only the two sound, funded mints (pattern held +
	// pool royalty). ANDed with EconomyEnabled at the mint gate in main.go; force-
	// off'd below. Env: LENS_ANNOTATION_MINTING_ENABLED.
	AnnotationMintingEnabled bool

	// POVIMintingEnabled gates PROVISIONAL receipt-based LENS minting
	// (Token Economy Phase 1, Part 1). DEFAULT FALSE and intentionally so:
	// minting from a signed receipt is UNSAFE on receipt-alone — a node can
	// sign a fabricated trace and this layer can't catch it. Real safety needs
	// stake + random challenge-and-slash (Parts 2/3). When false (default),
	// PoVI verifies + records receipts for audit but mints NOTHING. Only flip
	// LENS_POVI_MINTING_ENABLED in a test/throwaway context — never production
	// until Parts 2/3 land.
	POVIMintingEnabled bool

	// PoolRoyaltyMintingEnabled gates the Phase-2 Stage 2.1 Pool-B royalty
	// mint: a served cross-tenant pooled cache hit mints s × avoided_COGS to
	// the contributing tenant, exactly once per serving request. DEFAULT TRUE
	// — it is the first entry in the default-on loop in Load (the three live
	// traffic mints, closed-test), and force-off'd with EconomyEnabled. ⚠ SO ON
	// A DEFAULT DEPLOYMENT THIS MINT IS ARMED. Only when it is forced off do
	// pooled hits serve exactly as Stage 2.0 left them with nothing minting.
	// ⚠ AND EVERY OTHER SWITCH IN THE CHAIN IS ARMED TOO, so "it still needs a
	// pooled hit" is not a second safety: minting needs CachePoolableEnabled
	// (also in the default-on loop) AND both workspaces opted in, and migration
	// 0106 set workspaces.cache_poolable DEFAULT true for rows created from then
	// on. A fresh deployment serving two newly-registered workspaces mints.
	// Existing rows keep their stored value (0106 rewrote no row).
	// Env: LENS_POOL_ROYALTY_MINTING_ENABLED.
	PoolRoyaltyMintingEnabled bool

	// LXCShadowSpendEnabled gates the Phase-2 Stage 2.4/2.5 SHADOW LXC spend
	// path — the first live-serving-path change. When ON, an AI call records a
	// shadow LXC debit (cost_usd / LXCUSDValue) AFTER the response is served,
	// alongside (never replacing) the cost_usd write. OBSERVATIONAL ONLY: the
	// debit is non-gating, post-serve, and error-swallowed, so it cannot block,
	// delay, or alter any request. DEFAULT FALSE — with this off the shadow
	// path is fully inert (no SpendLXC call at all). Env:
	// LENS_LXC_SHADOW_SPEND_ENABLED.
	LXCShadowSpendEnabled bool

	// LXCGatingEnabled gates the Phase-2 Stage 2.4/2.5 LXC GATING step — the
	// pre-serve check that BLOCKS a request (402) when the workspace's LXC
	// balance can't cover the estimated cost. The first gate that can alter
	// whether a request succeeds. DEFAULT FALSE. COHERENCE: gating is inert
	// unless LXCShadowSpendEnabled is ALSO on — gating means "block then
	// debit," but the only serving-path debit is the shadow path, so gating
	// without shadow would block with no accounting. Two-flag staging:
	// shadow=observe, shadow+gating=enforce. Env: LENS_LXC_GATING_ENABLED.
	LXCGatingEnabled bool

	// AdminLXCGrantEnabled gates the ADMIN-ONLY comped-LXC grant endpoint
	// (POST /v1/admin/lxc/grant). It is the bootstrap escape hatch for a closed
	// trial: a fresh workspace has 0 LXC and (without Stripe) no funding path, so
	// it cannot transact. The grant credits LXC through economy.GrantLXC — the
	// same atomic ledger+balance move as a purchase, but recorded under the
	// distinct LXCTypeGrant ("admin_grant") type so an auditor can always tell a
	// comp from paid revenue. DEFAULT FALSE: off ⇒ the route is never registered
	// (chi 404, no oracle). Env: LENS_ADMIN_LXC_GRANT_ENABLED.
	AdminLXCGrantEnabled bool

	// PatternCaptureEnabled gates the Phase-3 routing-pattern CAPTURE WRITE —
	// the producer for the already-live routing Advisor. When ON, a served
	// model call persists an anonymized routing observation (post-serve, void)
	// for OPTED-IN workspaces only. CAPTURE-ONLY and structurally MINT-FREE:
	// it never reaches ledger.Credit (earning is a separate later stage).
	// DEFAULT FALSE. Env: LENS_PATTERN_CAPTURE_ENABLED. Separate from
	// PatternMiningEnabled (which gates the opt-in HTTP route, unchanged).
	PatternCaptureEnabled bool

	// PoolShadowLogEnabled gates the W4.9 cross-tenant pooling SHADOW LOG: a
	// post-serve, void, fingerprint-only observation of what WOULD have pooled,
	// so the pooled hit rate is measurable on real traffic while pooling itself
	// stays off. Persist-only; no cache client and no ledger handle, so it can
	// neither serve a pooled answer nor mint. DEFAULT FALSE.
	// Env: LENS_POOL_SHADOW_LOG_ENABLED.
	//
	// ⚠ NOT force-off'd with EconomyEnabled, and that is deliberate rather than an
	// omission — same posture as ShadowMintsEnabled. This flag measures whether a
	// feature would be worth turning on; force-off'ing it with the master switch
	// would mean the evidence needed to decide can only be gathered once the
	// decision has already been made.
	PoolShadowLogEnabled bool

	// PatternEarningEnabled gates the Phase-3 routing-pattern EARNING wire-up
	// (S4) — the FIRST pattern-earning stage that touches the serve path. When
	// ON, an opted-in, authenticated workspace's served request routes into
	// RecordPattern (credits LENS) instead of the mint-free capture write.
	// DEFAULT TRUE (the default-on loop in Load), force-off'd with EconomyEnabled — the
	// serve path is byte-identical to capture-only only when the flag is off. SEPARATE from PatternCaptureEnabled (capture and earn gate
	// independently). Env: LENS_PATTERN_EARNING_ENABLED.
	PatternEarningEnabled bool

	// PoolMintCapPerPair is the Pool-B mint cap (2.3b primitive #1): the max
	// royalty mints per (requester, contributor) pair per rolling window.
	// Phase-0 Item D: default 50 (was 0/off); 0 (env) disables. The cap bounds any gaming vector's
	// worst case to cap × s × avoided_COGS per pair per window — the
	// arithmetic behind the Option-C deterrence + bounded-exposure posture.
	// Env: LENS_POOL_MINT_CAP_PER_PAIR.
	PoolMintCapPerPair int

	// PoolMintCapPerEntry is the Pool-B per-ENTRY mint cap (2.3b follow-up):
	// max royalty mints per entry_id per rolling window, across ALL
	// contributors (semantic ownership churn lets one entry_id accrue mints
	// under different contributor identities over the window, so the per-pair
	// cap alone doesn't bound per-entry exposure). 0 (default) = disabled.
	// Shares PoolMintCapWindow. Env: LENS_POOL_MINT_CAP_PER_ENTRY.
	PoolMintCapPerEntry int

	// PoolMintCapWindow is the rolling window the per-pair cap counts over
	// (no period anchor, no state — matches the SpendLimitUSD rolling-window
	// precedent). Only consulted when the cap is enabled. Default 24h.
	// Env: LENS_POOL_MINT_CAP_WINDOW (Go duration, e.g. 48h).
	PoolMintCapWindow time.Duration

	// DistillMintCap* are the PR1 distill reuse-royalty mint caps — the same
	// deflationary primitive as PoolMintCap* but over distill_royalty_mints (a
	// SEPARATE budget; distill is its own table). PerPair caps mints per
	// (owner, requester); PerContent caps mints per content_hash across requesters.
	// Default 0/0 (off) + 24h. Env: LENS_DISTILL_MINT_CAP_PER_PAIR /
	// LENS_DISTILL_MINT_CAP_PER_CONTENT / LENS_DISTILL_MINT_CAP_WINDOW. NOT in the
	// economy force-off block — a cap only DENIES a mint, never enables one.
	DistillMintCapPerPair    int
	DistillMintCapPerContent int
	DistillMintCapWindow     time.Duration

	// PatternEarnCapPerWorkspace is the S2 routing-pattern EARN cap: max
	// pattern-mine credits per workspace per rolling window. DELIBERATELY
	// DEFAULTS to a REAL limit (50000), NOT 0 — pattern-earning is a
	// self-generated, gamed surface, so it must not be able to exist uncapped
	// behind a single flag (diverges from Pool-B's default-0 consciously). The
	// ceiling is anchored to the ATTACK case: with S1's corroboration floor an
	// attacker can't self-manufacture the premium, so pure-volume abuse earns
	// only base (0.001) → ~50 LENS/ws/24h max. 0 = explicit ops-disable, never
	// the default. Inert until S4 wires the earn path. Env:
	// LENS_PATTERN_EARN_CAP_PER_WORKSPACE.
	PatternEarnCapPerWorkspace int

	// PatternEarnCapWindow is the rolling window for the pattern earn cap
	// (matches Pool-B's window precedent). Default 24h. Env:
	// LENS_PATTERN_EARN_CAP_WINDOW (Go duration).
	PatternEarnCapWindow time.Duration

	// MintRateCapLENS24h is the U6 PR2 per-identity earning-rate cap: the max
	// LENS a single workspace may MINT (across ALL mint types: cache/compute/
	// embedding/annotation/pattern/PoVI/pool-royalty-held) in a rolling 24h
	// window, enforced at the ledger chokepoint. It is the universal steady-state
	// bound on Sybil wash yield — verification (the floor) raised the entry bar
	// but not the steady-state yield, so this caps it. DEFAULT 1000 (≈ $100/day
	// realizable at the $0.10 peg) — generous for a legitimate contributor,
	// bounding a washed identity; tighten at flip-on. 0 = explicit ops-disable
	// (never the default; default-ON-with-a-ceiling keeps inaction safe). A
	// safety restriction — NOT force-off'd by the economy kill. Env:
	// LENS_MINT_RATE_CAP_LENS_24H.
	MintRateCapLENS24h float64

	// PoolHoldbackWindow is the Stage-2.3a holdback: a pool-royalty mint
	// credits HELD balance, and only becomes spendable (and supply-counted)
	// when the finalize sweeper settles it after this window. Default 72h —
	// sized so the statistical gaming vectors (detectable in hours from the
	// durable claim rows) are comfortably inside it. TRIGGER-AGNOSTIC by
	// design: the ledger ops (CreditHeldTx/FinalizeHeldTx/RevokeHeldTx) don't
	// know what fires them — the timed sweeper is just the initial trigger,
	// and billing settlement can replace it later without ledger changes.
	// Env: LENS_POOL_HOLDBACK_WINDOW (Go duration).
	PoolHoldbackWindow time.Duration

	// Detector thresholds (Stage 2.3b) — tune the FLAGGED boolean of the
	// on-demand DetectorReader; they never affect the raw metrics it returns
	// and never trigger any action (detection only surfaces; revoke stays the
	// deliberate 2.3a operator decision). All have sensible defaults so the
	// detectors are usable out of the box.
	DetectVolumeMinMints      int     // LENS_DETECT_VOLUME_MIN_MINTS (default 50)
	DetectVolumeMaxRequesters int     // LENS_DETECT_VOLUME_MAX_REQUESTERS (default 2)
	DetectBilateralMinFrac    float64 // LENS_DETECT_BILATERAL_MIN_FRAC (default 0.9, [0,1])
	DetectBilateralMinMints   int     // LENS_DETECT_BILATERAL_MIN_MINTS (default 20)
	DetectSimilarityMinSample int     // LENS_DETECT_SIMILARITY_MIN_SAMPLE (default 30, >=1)
	DetectSimilarityMaxStddev float64 // LENS_DETECT_SIMILARITY_MAX_STDDEV (default 0.02, >=0)

	// DetectorSweep* gate the scheduled "smoke detector" — the leader-elected sweep that
	// runs the cache+distill detectors on a cadence and records flagged findings. It runs
	// iff EconomyEnabled AND DetectorSweepEnabled. DetectorSweepEnabled defaults TRUE so
	// detection accompanies minting automatically (the EconomyEnabled gate already makes
	// it free when idle — no mints, nothing to scan); the flag is only a manual off-switch.
	DetectorSweepEnabled  bool          // LENS_DETECTOR_SWEEP_ENABLED (default TRUE)
	DetectorSweepInterval time.Duration // LENS_DETECTOR_SWEEP_INTERVAL (default 1h)

	// LXCAgentAllocationEnabled gates the F4-capstone per-scoped-key LXC sub-budget path (step A). Defaults
	// TRUE (owner's call — the capstone ships armed): when on, an agent's LXC spend is bounded by its
	// sub-budget ceiling (default 50 LXC) and debited exactly-once; a zero-balance agent still spends nothing.
	// When false, the sub-budget path is bypassed (today's plain SpendLXC behavior). It is a SPEND bound, not
	// a mint — deliberately NOT in the economy force-off block (turning it off REMOVES a spend guard, so off
	// is the less-safe state; it is not a mint gate). Env: LENS_LXC_AGENT_ALLOCATION_ENABLED (default TRUE).
	LXCAgentAllocationEnabled bool // LENS_LXC_AGENT_ALLOCATION_ENABLED (default TRUE)

	// LXCReservationEnabled turns the agent charge into a HOLD → SETTLE/RELEASE reservation so the customer
	// is billed what was DELIVERED, not a pre-serve estimate (routing/cache/batch savings reach the bill).
	// DEFAULT TRUE — the correct billing IS the end state, not a flag someone remembers to flip. When on, the
	// pre-serve seam RESERVES a conservative hold and the post-serve seam SETTLES the delivered cost (or
	// RELEASES a cache hit, free). When off, the legacy pre-serve estimate debit (SpendLXCForAgent) runs — the
	// rollback path — and a served question is then settled to its delivered cost within the agent's limit,
	// the rest written off (B23.13); a cache serve to nothing, or a pooled one to its discounted price (B26.9).
	// Env: LENS_LXC_RESERVATION_ENABLED (default TRUE).
	LXCReservationEnabled bool
	// LXCReservationMaxOutputTokens BOUNDS the conservative hold when a request omits max_tokens. Held output
	// = explicit max_tokens if set, else THIS cap — deliberately NOT the catalog maximum: holding a model's
	// full 128k-token output against a request that produces 200 would block legitimate work on a modest
	// ceiling (a hold so conservative it blocks is its own bug). Env:
	// LENS_LXC_RESERVATION_MAX_OUTPUT_TOKENS (default 4096).
	LXCReservationMaxOutputTokens int
	DetectorSweepWindow           time.Duration // LENS_DETECTOR_SWEEP_WINDOW (default 24h)

	// AntiGaming* gate the Phase-2 AUTO clawback: the leader-elected sweep that detects
	// self-dealing RINGS (transitive identity closure over held cross-tenant royalty mints)
	// and REVOKES them before settlement, via the durable Adjudicate path. Unlike the
	// read-only smoke detector, this one ACTS — so it is DEFAULT-OFF (a detector that wrongly
	// revokes is worse than none; turning it on is a Phase-4 decision, after calibration). The
	// scheduler starts iff EconomyEnabled; RunOnce is a total no-op unless AntiGamingAutoRevokeEnabled.
	// FAIL-CLOSED: a detector error aborts the tick with no clawback (never a partial revoke).
	AntiGamingAutoRevokeEnabled bool          // LENS_ANTIGAMING_AUTO_REVOKE_ENABLED (default FALSE)
	AntiGamingScanWindow        time.Duration // LENS_ANTIGAMING_SCAN_WINDOW (default 168h — must exceed the 72h holdback so every held row is scanned)
	AntiGamingInterval          time.Duration // LENS_ANTIGAMING_INTERVAL (default 1m — frequent, to catch rings well before finalize)

	// PatternConcentrationVelocityMax — Phase-4a Item 3: the SINGLE-PARTY concentration
	// guard flags a workspace with MORE than this many held pattern mints in a 1h
	// velocity window (a farm spike); its mints are WITHHELD from settlement (held for
	// review — recoverable, not destroyed). PLACEHOLDER — calibrate at scale (like the
	// keel thresholds). Default 2000/h sits just below the earn cap's sustained max
	// rate (~2083/h) so only farm-level velocity is withheld. 0 disables flagging.
	PatternConcentrationVelocityMax int // LENS_PATTERN_CONCENTRATION_VELOCITY_MAX (default 2000)

	// SettlementFailClosedEnabled (Phase-3 Item 3) arms the settlement-side fail-closed layer for the
	// cross-tenant reuse tables (pool_royalty_mints / distill_royalty_mints). When ON: the finalize
	// sweeper settles ONLY status='cleared' rows, and the settlement clearer promotes ONLY examined-clean-
	// and-due held rows to 'cleared' — so a held mint the ring detector never examined (detector down /
	// lagging / the 72h window closed first) is NEVER cleared and NEVER settles (it holds). DEFAULT TRUE
	// and byte-identical when off (the sweeper settles 'held' as before, the clearer is a total no-op).
	// Scheduler starts iff EconomyEnabled. Env: LENS_SETTLEMENT_FAIL_CLOSED_ENABLED.
	SettlementFailClosedEnabled bool // LENS_SETTLEMENT_FAIL_CLOSED_ENABLED (default TRUE)

	// Keel* gate the U25 cross-tenant DRIFT-ATTRIBUTION sweep — DEFAULT-ON capability flag (mint-free,
	// descriptive, read-only; NOT force-off because it never touches money). Thresholds are PLACEHOLDERS —
	// calibrate at N3 turn-on; synthetic data proved only the mechanism, never these values.
	KeelEnabled        bool    // LENS_KEEL_ENABLED (default TRUE)
	KeelDeviationSigma float64 // LENS_KEEL_DEVIATION_SIGMA (default 3.0, placeholder)
	KeelWindowSeconds  int64   // LENS_KEEL_WINDOW_SECONDS (default 3600, placeholder)
	// ⚠ NOT ENV-CONFIGURABLE, AND THE COMMENT USED TO SAY OTHERWISE (W6.23). These two
	// read `// LENS_KEEL_INTERVAL` and `// LENS_KEEL_LOOKBACK` exactly like the
	// env-loaded fields around them, and NOTHING reads either name — Load assigns
	// both from constants below. An operator setting LENS_KEEL_INTERVAL=10m got
	// nothing, silently, from the file that is the authoritative place to look.
	KeelInterval time.Duration // CONSTANT 1h (sweep tick) — no env var
	KeelLookback time.Duration // CONSTANT 48h (corpus read-back) — no env var

	// Keel HARDENED (K3) money-grade detection — ADDITIVE, DEFAULT-ON. Leave-one-out + median/MAD +
	// money-grade floors + persistence + drop-direction, so findings can LATER gate money (H5). ALL
	// thresholds are PLACEHOLDERS — calibrate at N3 against real-scale distribution; NO MONEY MAY MOVE ON
	// AN UNCALIBRATED THRESHOLD.
	KeelHardenedEnabled    bool    // LENS_KEEL_HARDENED_ENABLED (default TRUE)
	KeelMoneyCohortFloor   int     // LENS_KEEL_MONEY_COHORT_FLOOR (PLACEHOLDER 10, >> the privacy floor of 3)
	KeelMinSamples         int     // LENS_KEEL_MIN_SAMPLES (PLACEHOLDER 30, per-workspace requests/window)
	KeelPersistenceWindows int     // LENS_KEEL_PERSISTENCE_WINDOWS (PLACEHOLDER 3, consecutive windows)
	KeelHardenedSigma      float64 // LENS_KEEL_HARDENED_SIGMA (PLACEHOLDER 3.5, MAD-scaled robust score)

	// PoolRoyaltyShare is s, the contributor's share of avoided_COGS
	// (Stage 2.1). Env: LENS_POOL_ROYALTY_SHARE. Default 0.5. Must be in
	// [0,1] so Talyvor's net (1−s) × avoided_COGS stays ≥ 0 (the
	// Burn-and-Mint invariant).
	PoolRoyaltyShare float64

	// PoolConsumerDiscount is r, the fraction of a CROSS-TENANT POOLED cache hit's list price handed
	// back to the CONSUMER. Env: LENS_POOL_CONSUMER_DISCOUNT. Default 0.30. Must be in [0,1).
	//
	// WHY THE DISCOUNT EXISTS: a pooled hit costs Talyvor nothing — there is no provider call — so the
	// entire charge is margin, split between the contributor (s) and us (1−s). Charging the consumer
	// the full avoided cost meant the cache saved them nothing, which is the first thing a buyer
	// notices and the last thing they forgive.
	//
	// WHY 0.30, argued rather than assumed. Both shares come out of the DISCOUNTED charge, so r is a
	// straight transfer from (contributor + Talyvor) to the consumer, split by s. Two constraints
	// pull against each other, and at s = 0.5 they cross here:
	//
	//   - the contributor must still want to contribute. They paid C upstream and earn s(1−r) of each
	//     pooled hit's list price, so they need 1/(s(1−r)) ≈ 2/(1−r) hits before their original call
	//     is paid for: 2.5 hits at r=0.2, 2.9 at r=0.3, 3.3 at r=0.4, 4.0 at r=0.5. 0.30 is the
	//     largest rate that keeps that under three — past it the ask on contributors grows faster
	//     than the consumer's benefit does.
	//   - the margin must still be worth running. Talyvor nets (1−s)(1−r) = 0.35 of list per pooled
	//     hit against zero COGS.
	//
	// ⚠ IT DOES NOT WEAKEN THE SELF-DEAL CONTROL that refuses s ≥ 1 below. A Sybil pair pays C
	// upstream and (1−r)·list as the consumer, earning s(1−r)·list back: net −C − (1−s)(1−r)·C, which
	// is negative for EVERY r < 1 — at r → 1 it is still a full −C, because the upstream call was
	// real money and the royalty is bounded by the consumer's settled charge. The discount moves the
	// pair's loss from 1.5C to 1.35C at the defaults; it cannot approach break-even.
	//
	// ⚠ AND THE VISIBILITY IS NOT OPTIONAL. A charge of 644 instead of 920 with nothing saying so is
	// indistinguishable from a cheaper answer — the rate does nothing a buyer can perceive unless the
	// response and the ledger row both carry what it WOULD have cost.
	PoolConsumerDiscount float64

	// POVIMinStake is the minimum LENS a node must lock as collateral to be
	// minting-eligible (Part 2). Env: LENS_POVI_MIN_STAKE. Default 100.
	POVIMinStake float64

	// POVIUnbondPeriod is how long staked collateral stays locked AND SLASHABLE
	// after a node begins unbonding — the anti-yank delay (a node can't cheat
	// then instantly withdraw before a challenge slashes it). Env:
	// LENS_POVI_UNBOND_PERIOD as a Go duration (units h/m/s — NOT "d"; use
	// "168h" for 7 days). Default 7*24h.
	POVIUnbondPeriod time.Duration

	// POVIChallengeRate is the fraction of verified receipts challenged per
	// scheduler round (Part 3). Env: LENS_POVI_CHALLENGE_RATE in [0,1].
	// Default 0.1. The economic deterrent: expected cost of cheating =
	// P(challenge) × slash_amount must exceed the gain. Higher rate = stronger
	// deterrent + more overhead; a low rate leaves some bad receipts
	// unchallenged (probabilistic, not absolute).
	POVIChallengeRate float64

	// POVISlashFraction is the fraction of a node's stake burned on a failed
	// challenge. Env: LENS_POVI_SLASH_FRACTION in (0,1]. Default 0.5.
	POVISlashFraction float64

	// POVIChallengeKey is the base64 ed25519 PRIVATE key Lens signs challenges
	// with (env: LENS_POVI_CHALLENGE_KEY). If empty, Lens generates an
	// ephemeral key at startup — fine for a single-instance dev run, but set a
	// stable key in production so a Lens restart doesn't invalidate the pubkey
	// nodes have pinned (which would make honest nodes fail challenges).
	POVIChallengeKey string

	// KYASigningKey is the base64 Ed25519 seed or private key every agent's Know Your Agent credential is signed with
	// (env: LENS_KYA_SIGNING_KEY, B30.5). Empty makes one per start: what it signed stays verifiable until it expires,
	// as the JWKS publishes every key an unexpired credential was signed with, but production sets one.
	KYASigningKey string

	// KYACredentialTTL is how long a Know Your Agent credential is valid (env: LENS_KYA_CREDENTIAL_TTL). Default 24h.
	KYACredentialTTL time.Duration

	// PublicURL is the address the world reaches Lens at (env: LENS_PUBLIC_URL, default https://lens.talyvor.com): an
	// agent's Agent Card names its own URL and its key's under it (B45.23).
	PublicURL string
	// PersonaAPIKey and PersonaTemplateID are Persona's API key and the inquiry template of its "KYC" solution (env:
	// LENS_PERSONA_API_KEY, LENS_PERSONA_TEMPLATE_ID, B30.112): with both set a person's identity check (L2) goes to
	// Persona, and otherwise to the Test provider.
	PersonaAPIKey     string
	PersonaTemplateID string
	// CompaniesHouseAPIKey is a Companies House REST API key (env: LENS_COMPANIES_HOUSE_API_KEY, B30.112): set, a UK
	// company's check (L3) goes to Companies House, and otherwise to the Test provider.
	CompaniesHouseAPIKey string
	// ScreeningFuzzyThreshold is how similar a name must be to one on a sanctions list for its payee or payment to be
	// held for an operator (B30.6), above 0 and at most 1. Env: LENS_SCREENING_FUZZY_THRESHOLD, which Nicolai sets;
	// until he does, 0.9.
	ScreeningFuzzyThreshold float64
	// ScreeningMaxAge is how long ago a sanctions list may have last downloaded before every payment in or out is held
	// for an operator (B37.4). Env: LENS_SCREENING_MAX_AGE_HOURS, whole hours, which Nicolai sets; until he does, 48.
	ScreeningMaxAge time.Duration
	// AccountPartnerWebhookSecret signs the account partner's notice of each payment in (B30.15): hex HMAC-SHA256 of
	// the body in X-Partner-Signature. A SECRET, never logged. Empty ⇒ POST /v1/money/payments-in/webhook is
	// unregistered (404) and no payment from outside posts. Env: LENS_ACCOUNT_PARTNER_WEBHOOK_SECRET.
	AccountPartnerWebhookSecret string
	// SuspenseReturnDays is how many business days money in that matched no account sits in suspense before it goes
	// back to its payer, unless the operator assigns it (B30.15). Env: LENS_SUSPENSE_RETURN_DAYS, which Nicolai sets;
	// until he does, 5.
	SuspenseReturnDays int

	// TrustfulComputeMintEnabled gates the LEGACY trust-based compute mint
	// (ComputeMiner.RecordServedRequest mints LENS per served request with no
	// receipt, on a caller-asserted token count, with no idempotency). Env:
	// LENS_TRUSTFUL_COMPUTE_MINT_ENABLED. DEFAULT FALSE (U6 Sybil floor): an
	// unprotected mint path must be opt-IN, not on-by-accident — receipt-minting
	// (LENS_POVI_MINTING_ENABLED, now safe with PoVI Parts 1+2+3) is the intended
	// successor. Set true only to deliberately re-enable the blind trust-mint.
	TrustfulComputeMintEnabled bool

	// GuardrailsEnabled gates the Upgrade 13 OUTPUT guardrails (CheckOutput:
	// output PII, JSON/length/regex validation) + the per-workspace config
	// API for them. OFF by default: when false the INPUT guardrails behave
	// exactly as today and no output guardrails run. Even when on, output
	// block-actions are explicit opt-in (default flag/observe). Opt in via
	// LENS_GUARDRAILS_ENABLED=true.
	GuardrailsEnabled bool

	// WorkTierEnabled gates the WorkTier descriptive work classifier — a
	// post-serve, NON-CONTENT classification of each served request (size / cost
	// / complexity / sensitivity), persisted for the future routing Advisor +
	// analytics. CAPABILITY flag, default false: off = no classification, no
	// rows, zero behavior change. DESCRIPTIVE and mint-free, so it is NOT a
	// safety restriction and is deliberately NOT in the economy force-off block
	// (the inverse of the economy gates: off = no feature = safe). Env:
	// LENS_WORKTIER_ENABLED.
	WorkTierEnabled bool

	// K4VerifierEnabled gates the K4 INTRINSIC output verifier — a POST-SERVE, off-path check of whether the
	// served output violates a machine-checkable constraint the REQUEST ITSELF declared (json_object /
	// json_schema / tool-call args), keyed by a server-derived gateway-bound output identity. CAPABILITY
	// flag, DEFAULT FALSE: off = no verification, no rows, zero behavior change. It moves NO money (mint-free,
	// import-guarded), never blocks/alters the response, and is intra-tenant only. Env: LENS_K4_VERIFIER_ENABLED.
	K4VerifierEnabled bool

	// RoutingDecisionCaptureEnabled gates the DESCRIPTIVE, MINT-FREE route-decision capture
	// (internal/routedecision) — records the routing Advisor's baseline vs served model + estimated cost, the
	// go/no-go substrate for a corpus-contribution mint. DEFAULT TRUE (descriptive, closed-test,
	// money-decoupled — the sink has no ledger handle). Env: LENS_ROUTING_DECISION_CAPTURE_ENABLED (falsey disables).
	RoutingDecisionCaptureEnabled bool

	// KeelRoyaltyHaircutEnabled gates KE-2: a REDUCE-ONLY reuse-royalty haircut for a workspace with a sustained
	// HARDENED idiosyncratic drift finding (never common-mode, never a slash — it can only LOWER a mint, never
	// below the floor, never increase anyone's earnings). DEFAULT-ON — closed-test: the reuse-royalty economy
	// has no external contributors, so a haircut takes nothing from anyone real, and running it is the ONLY way
	// to observe the guard fire against real routing behaviour before N3. It is REDUCE-ONLY, clamped to
	// [HaircutFloor, 1.0], can never burn/slash, can never increase a mint, and fails open on any doubt.
	// ⚠ THE THRESHOLD IS STILL AN UNCALIBRATED PLACEHOLDER — recalibrate at N3 BEFORE any external contributor
	// earns. Env LENS_KEEL_ROYALTY_HAIRCUT_ENABLED can still force it off.
	KeelRoyaltyHaircutEnabled bool

	// H5BondsEnabled gates H5.β PROVENANCE BONDS — THE MONEY PATH. A workspace stakes collateral on a
	// gateway-bound output_id; a self-reported mechanical FAILURE (the only slash-usable signal) BURNS a
	// fraction of the bond (supply reduced, paid to nobody), else time RELEASES it. DEFAULT FALSE: off = no
	// bond endpoints, no locks, no slashes, zero behavior change. Env: LENS_H5_BONDS_ENABLED.
	H5BondsEnabled bool

	// H5AttestEnabled gates the ATTESTED-SLASH PRODUCER (internal/attest) — Talyvor reproduces a bonded
	// output's build in its sandbox and records a talyvor_verified compile verdict (admin-only path). DEFAULT
	// FALSE. Off → the attest endpoint is not registered and no talyvor_verified row is ever written. Even ON,
	// it REFUSES every output whose committed response is not a buildable-tree hash (today: all of them), so
	// it is inert in production until a gateway commits buildable provenance. Env: LENS_H5_ATTEST_ENABLED.
	H5AttestEnabled bool

	// H5ArtifactEnabled gates the OPT-IN BUILDABLE-ARTIFACT COMMIT endpoint (POST /v1/outputs/{id}/artifact) —
	// a workspace commits, bound to an output it produced, the manifest hash of its buildable module (output
	// slot folded from the output's captured output_content_sha256 — the canonical served content a real tree
	// can carry; outputverify/content.go pins the bytes). DEFAULT FALSE. Off → the endpoint is not registered
	// and no artifact_sha256 is ever populated, so the attestor's opt-in binding is dormant (every output stays
	// on the legacy response_sha256 binding — fail-open, unchanged). Env: LENS_H5_ARTIFACT_ENABLED.
	H5ArtifactEnabled bool

	// NodeLatencyCaptureEnabled gates the DESCRIPTIVE proof-of-latency capture (P3 #6) — a post-serve,
	// off-path record of gateway-measured node serve latency into a per-(node,cohort) EWMA aggregate
	// (node_cohort_latency_stats), the substrate a LATER mint reads. CAPABILITY flag, default false: off =
	// no capture, no rows, zero behavior change. DESCRIPTIVE and mint-free (writes no ledger), so — like
	// WorkTier — it is deliberately NOT in the economy force-off block. Env: LENS_NODE_LATENCY_CAPTURE_ENABLED.
	NodeLatencyCaptureEnabled bool

	// NodeAttestationVerifyEnabled gates the Proof-of-Confidential-Compute VERIFY sweep (step b): the gateway
	// dials each node's /attestation, verifies the NVIDIA NRAS EAT (JWT sig + x5c chain to the pinned NVIDIA
	// root + claims), and records the verified hardware class to node_attestations. CAPABILITY flag, default
	// false: off = no sweep, no dials, no rows. Mint-free (records a class, credits nothing), so — like
	// NodeLatencyCapture — it is deliberately NOT in the economy force-off block. Also needs the pinned root
	// CA (LENS_NVIDIA_ROOT_CA_PEM) + the JWKS URL to be configured. Env: LENS_NODE_ATTESTATION_VERIFY_ENABLED.
	NodeAttestationVerifyEnabled bool

	// RoutingIntelligenceEnabled gates Upgrade 22 — feeding aggregated
	// pattern-mining intelligence back into model selection. OFF by default:
	// when false, routing behaves byte-for-byte as before. Even when on, it
	// only influences requests that explicitly opt into auto-routing
	// (model "auto" or X-Talyvor-Auto-Route) and only within the
	// workspace's allowed models. Opt in via
	// LENS_ROUTING_INTELLIGENCE_ENABLED=true.
	RoutingIntelligenceEnabled bool

	// RoutingTierCohortsEnabled refines RoutingIntelligence (Shape 3): when on, the Advisor
	// conditions its cohorts on the request's complexity tier (worktier.ComplexityBucketFor),
	// picking the best model PER work-type, with a tiered→non-tiered→default fallback. OFF by
	// default and in the kill-switch force-off block: when off, routing is byte-for-byte as
	// before (the complexity dimension is ignored). Only meaningful when RoutingIntelligence is
	// also on. Opt in via LENS_ROUTING_TIER_COHORTS_ENABLED=true.
	RoutingTierCohortsEnabled bool

	// RoutingBrainEnabled gates the H8.1 Routing Brain — the OFFLINE learner that reads
	// verified outcomes (H2 capability curves + Keel drift, keyed by the H1/H2 work-tier
	// difficulty) and writes a per-(workspace, cohort) best-model recommendation the serve
	// path reads as a cheap in-memory lookup. CAPABILITY flag, DEFAULT FALSE: off → the
	// offline job never runs and the serve path reads no recommendation (routing is
	// byte-for-byte unchanged). It is DESCRIPTIVE + mint-free (advises routing, never mints),
	// so — like WorkTier — it is deliberately NOT in the economy kill-switch force-off block
	// (off REMOVES a capability, it does not remove a money guard). When ON, ADVISORY is the
	// per-workspace default; AUTONOMOUS (the brain's pick is APPLIED within the hard floor) is
	// a further EXPLICIT per-workspace opt-in stored in routing_brain_autonomous. Env:
	// LENS_ROUTING_BRAIN_ENABLED.
	RoutingBrainEnabled bool

	// NodeAutoRouteEnabled (blocker 6) gates gateway auto-routing of normal inference traffic to a
	// registered inference node (which then auto-signs + submits its own receipt). DEFAULT FALSE.
	// It is a ROUTING flag, NOT a mint gate — minting stays gated downstream by stake +
	// earn_verified — so it is deliberately NOT in the kill-switch force-off block. Off → the serve
	// path is byte-identical to today. Env: LENS_NODE_AUTOROUTE_ENABLED.
	NodeAutoRouteEnabled bool

	// ReputationBondedMintingEnabled (P1 #9) gates the reputation BOND on PoVI-receipt + pool-royalty
	// mints: a contributor's reputation R modulates the mint amount via f(R)∈[0,1] (never amplifies)
	// and gates to 0 below the access floor. DEFAULT FALSE. It can only REDUCE or BLOCK a mint the U6
	// floor/stake/rate-cap already allowed — never enable one — so it is deliberately NOT in the
	// kill-switch force-off block (a mint-reducer, not a mint-enabler; when the economy is off all
	// bonded mints are already force-off'd). Off → the mint path is byte-identical to today (no
	// reputation read, no emitter). Env: LENS_REPUTATION_BONDED_MINTING_ENABLED.
	//
	// ⚠ THE REDUCER/ENABLER ARGUMENT ABOVE IS TRUE AND IT IS NOT THE ONE THAT DECIDES WHETHER
	// FLIPPING THIS IS SAFE. MEASURED (real minter + real finalize sweeper, real PG): on the
	// pool-royalty / distill track, reducing a mint does not pay a contributor less — it makes the
	// mint UNSETTLEABLE. The bond reduces the HELD credit inside CreditHeldTx, while the claim row
	// the sweeper settles (minted_amount) was already written with the unreduced base, so finalize
	// asks for more than was ever held and fails ErrInsufficientHeld every tick, forever. See
	// internal/poolroyalty/bonded_settlement_integration_test.go, which pins the measurement and
	// expires the day the two agree. THE REPAIR IS A MONEY DECISION (minted_amount also feeds the
	// gaming detectors and the margin views), so it is reported, not guessed — QUEUE.md W6.1.
	ReputationBondedMintingEnabled bool

	// ProofOfBenchmarkEnabled (P1 #10, PR-A) gates the proof-of-benchmark probe SCHEDULER: a verifier
	// draws an unpredictable eval item from a private pool, sends only the input to a node, and scores
	// the answer against held ground truth into a per-node quality signal. DEFAULT FALSE. MEASUREMENT
	// only — the score feeds NO routing and NO mint, so it is NOT in the kill-switch force-off block.
	// Off → no probes drawn, no scheduler. Env: LENS_PROOF_OF_BENCHMARK_ENABLED.
	ProofOfBenchmarkEnabled bool

	// ProofOfImprovementEnabled (Proof-of-Improvement rail, piece 1) is a CAPABILITY flag for selecting
	// a non-cost reward anchor (e.g. the held-benchmark anchor) in a FUTURE eval-contribution mint.
	// DEFAULT FALSE. It can never outrun the U6 floor/stake/rate-cap (those gate the amount downstream),
	// so it is NOT in the kill-switch force-off block. This PR wires NO reachable selection — the cost
	// anchor stands unconditionally — so the flag is byte-identical on or off today. Env:
	// LENS_PROOF_OF_IMPROVEMENT_ENABLED.
	ProofOfImprovementEnabled bool

	// EvalContributionMintingEnabled (Proof-of-Improvement instance 1) is the dedicated EARNING flag for
	// the proof-of-eval-contribution mint. DEFAULT FALSE. Unlike ProofOfImprovementEnabled (the
	// capability that authorizes selecting the held-benchmark anchor), this gates the mint FIRING — so
	// it is a state-creation gate and IS in the kill-switch force-off block (mirrors POVIMintingEnabled).
	// The mint fires only when BOTH this AND ProofOfImprovementEnabled are on AND a positive rate is set.
	// Env: LENS_EVAL_CONTRIBUTION_MINTING_ENABLED.
	EvalContributionMintingEnabled bool

	// RoutingPredictionEnabled (Proof-of-Improvement piece 3, PR-1) is a CAPABILITY flag gating SUBMISSION
	// of routing predictions (the attributable "cohort C → model M" unit). DEFAULT FALSE — the
	// routing_predictions table stays provably empty until the capability is deliberately enabled. This is
	// NOT an earning flag (the proof-of-routing-prediction mint + its force-off flag land in PR-4): PR-1 is
	// an inert data substrate with NO mint, so this is NOT in the kill-switch force-off block. Env:
	// LENS_ROUTING_PREDICTION_ENABLED.
	RoutingPredictionEnabled bool

	// RoutingPredictionScoringEnabled (Proof-of-Improvement piece 3, PR-3a) gates the routing-prediction
	// SCORER sweeper (run model M + the baseline on a cohort's held eval slice → skill-above-baseline
	// score). DEFAULT FALSE. It is a CAPABILITY/MEASUREMENT flag (it produces a score, mints nothing), so
	// it is NOT in the kill-switch force-off block — like LENS_PROOF_OF_BENCHMARK_ENABLED. INERT in PR-3a
	// regardless: the scorer is wired with NO real Inferer, so even flag-on runs no inference until PR-3b
	// supplies one. Env: LENS_ROUTING_PREDICTION_SCORING_ENABLED.
	RoutingPredictionScoringEnabled bool

	// RoutingPredictionMintingEnabled (Proof-of-Improvement instance 2, PR-4) gates the proof-of-routing-
	// prediction mint FIRING (pays a contributor whose prediction was proven skill-above-baseline). Like
	// EvalContributionMintingEnabled it is a state-creation gate (mints LENS) and IS in the kill-switch
	// force-off block. The mint fires only when BOTH this AND ProofOfImprovementEnabled are on AND a positive
	// rate is set. Env: LENS_ROUTING_PREDICTION_MINTING_ENABLED.
	RoutingPredictionMintingEnabled bool

	// EvalContributionRatePerPoint is the LENS-per-discrimination-point rate for the proof-of-eval-
	// contribution mint (amount = rate × clamp01(discrimination)). DEFAULT 0.05 (DefaultEvalContributionRatePerPoint,
	// NOT 0). The mint is nonetheless INERT by default because it also requires BOTH earning flags
	// (EvalContributionMintingEnabled AND ProofOfImprovementEnabled, default off) — a nonzero rate alone
	// mints nothing. A rate of 0 (env override) additionally makes the anchor nil (NewHeldBenchmarkAnchor
	// refuses a non-positive rate → RunOnce is a total no-op even with both flags on). Env:
	// LENS_EVAL_CONTRIBUTION_RATE_PER_POINT.
	EvalContributionRatePerPoint float64

	// RoutingPredictionRatePerPoint is the LENS-per-skill-margin-point rate for the proof-of-routing-
	// prediction mint (amount = rate × clamp01(skill_margin)). DEFAULT 0.02 (DefaultRoutingPredictionRatePerPoint,
	// NOT 0). INERT by default via the earning flags (RoutingPredictionMintingEnabled AND
	// ProofOfImprovementEnabled, default off), not via the rate; a rate of 0 (env override) additionally
	// nils the anchor (NewHeldBenchmarkAnchor refuses a non-positive rate). Env:
	// LENS_ROUTING_PREDICTION_RATE_PER_POINT.
	RoutingPredictionRatePerPoint float64

	// LatencyRatePerPoint is the LENS-per-latency-skill-point rate for the proof-of-latency-locality mint
	// (amount = rate × clamp01(latency_skill)). DEFAULT 0 ⇒ INERT: NewHeldBenchmarkAnchor refuses a
	// non-positive rate, so the minter's anchor is nil and RunOnce is a total no-op even with both flags on.
	// A deliberate later flip, set only when the economy goes live. Env: LENS_LATENCY_RATE_PER_POINT.
	LatencyRatePerPoint float64

	// GatewayPriceCeiling (F4-capstone step B) is the per-token price cap StrategyPriceAware clamps the
	// node-declared price to: effective_price = min(node_declared, ceiling). It bounds the ranking signal AND
	// (in step C) the agent's charge, so a node cannot set what an agent pays. DEFAULT 0.50 = 10× the 0.050
	// node default, so legitimate nodes are never clamped, only inflated declarations. A ROUTING bound, not a
	// mint gate — NOT in the economy force-off block. Env: LENS_GATEWAY_PRICE_CEILING.
	GatewayPriceCeiling float64

	// LatencyMintingEnabled (Proof-of-Improvement instance 3) gates the proof-of-latency-locality mint FIRING
	// (pays a NODE for cohort-relative, quality-gated fast service). Like the other two P-o-I mints it is a
	// state-creation gate (mints LENS) and IS in the kill-switch force-off block. Fires only when BOTH this
	// AND ProofOfImprovementEnabled are on AND a positive rate is set. Env: LENS_LATENCY_MINTING_ENABLED.
	LatencyMintingEnabled bool

	// ConfidentialRatePerPoint is the LENS-per-epoch FLAT rate for the proof-of-confidential-compute mint
	// (amount = rate × 1.0 per eligible (node,class,epoch)). DEFAULT 0 ⇒ INERT (the anchor refuses).
	// Env: LENS_CONFIDENTIAL_RATE_PER_POINT.
	ConfidentialRatePerPoint float64

	// ConfidentialMintingEnabled (Proof-of-Improvement instance 4) gates the proof-of-confidential-compute
	// mint FIRING (pays a NODE for VERIFIED confidential capacity). State-creation gate (mints LENS) and IS in
	// the kill-switch force-off block. Fires only when BOTH this AND ProofOfImprovementEnabled are on AND a
	// positive rate is set — and even then mints ZERO until a key_bound=true attestation exists (CC hardware).
	// Env: LENS_CONFIDENTIAL_MINTING_ENABLED.
	ConfidentialMintingEnabled bool

	// EconomyEnabled (U3) is the MASTER economy kill-switch. Env:
	// LENS_ECONOMY_ENABLED, default TRUE (explicit opt-out). When false, Load()
	// force-OFFs every economy state-creation gate below (regardless of its own
	// env value), and main.go does not register the economy route surface — the
	// deployment runs as pure fiat SaaS. NOT economy (untouched by this switch):
	// LENS_HA_ENABLED, LENS_GUARDRAILS_ENABLED.

	// BatchEnabled turns on the /v1/batch/* lane. DEFAULT FALSE, and deliberately so.
	//
	// ⚠ THE LANE BILLS NOTHING. /v1/batch/submit calls Anthropic directly and never enters
	// serve(), so Talyvor pays the provider and the workspace is charged nothing:
	// BatchRouter.SetSettleHook has no production caller, so the settle hook is nil in every
	// running Lens. It is worse than unbilled — the route takes its workspace from a CLIENT
	// HEADER (X-Talyvor-Workspace, defaulting to "default") without authorizing it, and
	// /v1/batch/jobs returns every workspace's jobs unfiltered, prompts and responses included.
	//
	// Off is therefore the honest position and a reversible one: the routes are not registered,
	// so the surface is absent rather than broken. Turning it on is a conscious act that should
	// wait for a settle hook, a session-derived workspace, and a filtered list.
	// Env: LENS_BATCH_ENABLED.
	BatchEnabled bool

	EconomyEnabled bool

	// ShadowMintsEnabled puts the six UNPROVEN mints (POVI, annotation, eval-contribution,
	// routing-prediction, latency-locality, confidential-compute) into SHADOW MODE: each
	// computes what it would pay, records it to lens_shadow_mints, and credits NOTHING.
	//
	// Default FALSE. Independent of EconomyEnabled and deliberately NOT in the force-off block:
	// shadow mode cannot pay anyone, so there is nothing for the master switch to protect against
	// — and force-off'ing it would make the one safe way to run these mints unavailable exactly
	// when the economy is off, which is when you most want to measure before paying.
	ShadowMintsEnabled bool

	// BillingEnabled (U18b) gates the FIAT Stripe billing surface (checkout +
	// webhook + admin purchases). Env: LENS_BILLING_ENABLED, default FALSE. It is
	// FIAT, NOT economy — independent of EconomyEnabled (a pure fiat-SaaS
	// deployment runs EconomyEnabled=false + BillingEnabled=true). When false,
	// main.go does not register the billing routes (unregistered ⇒ 404).
	BillingEnabled bool

	// EarnRequireLivePurchase tightens the verified-to-earn predicate
	// (internal/earnverify) to REAL-money purchases only. Env:
	// LENS_EARN_REQUIRE_LIVE_PURCHASE, default FALSE.
	//
	// False is what makes a TEST-key trial work: a test purchase is recorded with
	// livemode=false and still verifies, so a trial user becomes earn-verified by
	// buying instead of by a manual UPDATE. True closes that door — a test purchase
	// no longer verifies — with no code change and no migration, which is the whole
	// reason the mode is a recorded column rather than an assumption.
	//
	// ⚠ Turn this ON before open signup. With self-service signup and test keys,
	// anyone who can reach checkout can mint themselves earning rights for free.
	EarnRequireLivePurchase bool

	// DashboardEnabled gates the browser page at /dashboard. Env:
	// LENS_DASHBOARD_ENABLED, default FALSE — Lens is an API, and serving a UI
	// from the API host is opt-in. When false the route is not registered
	// (unregistered ⇒ chi 404), the same shape as BillingEnabled above.
	//
	// It does NOT gate `GET /`, which always serves the same static status page:
	// a visitor who types the API hostname must never be met with nothing, and
	// that page holds no credential, reads no database and shows no per-workspace
	// data, so there is nothing about it worth switching off.
	DashboardEnabled bool

	// StripeSecretKey / StripeWebhookSecret are read ONLY here (the
	// NoDirectEnvReads invariant extends to them). SECRETS — never logged. If
	// BillingEnabled is true and either is empty, Load() fails startup.
	StripeSecretKey     string
	StripeWebhookSecret string

	// B25.2 — test (synthetic) workspaces pay through Stripe TEST MODE, always, even once
	// LENS_STRIPE_SECRET_KEY is live: a test-mode key (sk_test_… or rk_test_…; a live key fails startup),
	// the signing secret of the test-mode webhook endpoint (POST /v1/billing/webhook/test), and the test-mode
	// plans (plan → test-mode Price id, like LENS_BILLING_SUBSCRIPTION_PLANS). SECRETS except the plans. With
	// the key or the secret unset a test workspace cannot buy or subscribe, and the refusal names what is
	// missing. Envs: LENS_STRIPE_TEST_SECRET_KEY, LENS_STRIPE_TEST_WEBHOOK_SECRET,
	// LENS_STRIPE_TEST_SUBSCRIPTION_PLANS.
	StripeTestSecretKey         string
	StripeTestWebhookSecret     string
	StripeTestSubscriptionPlans map[string]string
	// StripeTestMarketBillPriceID is the test-mode twin of MarketBillPriceID (B25.6): the metered Price, on a
	// test-mode meter of the same event name, a test workspace's paid marketplace uses are billed on. Unset,
	// a test workspace with the test-mode key cannot use a paid listing. Env:
	// LENS_STRIPE_TEST_MARKET_BILL_PRICE_ID, default EMPTY.
	StripeTestMarketBillPriceID string

	// StripeIssuingWebhookSecret signs the real-time authorisation endpoint Stripe calls for every agent
	// card purchase (B19.12) — Dashboard → Issuing → settings, a separate endpoint from the billing
	// webhook. A SECRET, never logged. Empty ⇒ POST /v1/agent-cards/authorizations is unregistered (404)
	// and no purchase can be approved. StripeIssuingCurrency is the currency agent cards are issued in:
	// the platform's own (gbp for a UK platform). Envs: LENS_STRIPE_ISSUING_WEBHOOK_SECRET,
	// LENS_STRIPE_ISSUING_CURRENCY.
	StripeIssuingWebhookSecret string
	StripeIssuingCurrency      string

	// MoneySandboxWebhookSecret signs the money layer sandbox's webhooks (B46.3), POST
	// /v1/money/webhooks/sandbox. A SECRET, never logged. Empty ⇒ that endpoint refuses every webhook.
	// Env: LENS_MONEY_SANDBOX_WEBHOOK_SECRET, default EMPTY.
	MoneySandboxWebhookSecret string

	// TrackWebhookURL / TrackWebhookSecret (SEC-7) configure the outbound Lens→Track
	// spend-alert emitter. TrackWebhookURL is Track's POST /v1/lens/webhook;
	// TrackWebhookSecret is the shared HMAC secret (must equal Track's
	// TRACK_LENS_WEBHOOK_SECRET) — a SECRET, never logged. If EITHER is empty the
	// emitter is DISABLED (a total no-op, no error) — this is the default posture.
	// Envs: LENS_TRACK_WEBHOOK_URL, LENS_TRACK_WEBHOOK_SECRET.
	TrackWebhookURL    string
	TrackWebhookSecret string

	// BillingSuccessURL / BillingCancelURL are the Stripe Checkout redirect URLs
	// (NOT secrets). Stripe requires both; defaulted, override per deployment.
	BillingSuccessURL string
	BillingCancelURL  string

	// BillingSubscriptionPriceID is the Stripe Price the Model 2 subscription bills
	// (W4.6.1 step 1). Env: LENS_BILLING_SUBSCRIPTION_PRICE_ID, default EMPTY.
	//
	// ⚠ EMPTY IS A REAL STATE, NOT A MISSING CONFIG, and it is the default on purpose:
	// billing (one-off LXC top-ups) and subscriptions are separate products, and a
	// deployment that sells top-ups must not start offering a subscription because it
	// set a Stripe key. With this empty the subscription routes are not registered and
	// CreateSubscriptionCheckout refuses with billing.ErrNoSubscriptionPrice — it never
	// calls Stripe with an empty price id.
	//
	// ⚠ NOT A SECRET, and not the FEE. `price_...` is a public identifier; the amount
	// lives in Stripe. W4.6.1: "F AND D ARE NICOLAI'S" — this is how the number stays
	// out of the codebase entirely rather than being written down in two places.
	BillingSubscriptionPriceID string

	// BillingSubscriptionPlans are the named plans offered at checkout (B13.1), plan → Stripe Price id.
	// Env: LENS_BILLING_SUBSCRIPTION_PLANS="plus=price_…,pro=price_…,max=price_…", default EMPTY. Like
	// the single price above, the amounts live in Stripe; each plan's included usage is computed from
	// its price (internal/billing/included_usage.go), not configured.
	BillingSubscriptionPlans map[string]string

	// MarketBillPriceID is the Stripe metered Price a buyer's paid marketplace uses are billed on, monthly
	// (B20.2): one µLXC per unit of the meter MarketMeterEvent names, at the LXC peg. Env:
	// LENS_MARKET_BILL_PRICE_ID, default EMPTY — then a paid listing can be used by no one but its seller.
	// MarketMeterEvent is that meter's event name. Env: LENS_MARKET_METER_EVENT.
	MarketBillPriceID string
	MarketMeterEvent  string

	// MarketTaxPriceID is the Stripe metered Price each billed use's tax is invoiced on, as its own line (B32.39): one
	// µLXC per unit of the meter MarketTaxMeterEvent names, at the LXC peg, like MarketBillPriceID. A buyer's
	// marketplace subscription gains an item of it the first time a use of theirs owes tax. Env:
	// LENS_MARKET_TAX_PRICE_ID, default EMPTY — then a use that owes tax waits, unbilled, for an operator.
	// MarketTaxMeterEvent is that meter's event name. Env: LENS_MARKET_TAX_METER_EVENT.
	// StripeTestMarketTaxPriceID is its test-mode twin. Env: LENS_STRIPE_TEST_MARKET_TAX_PRICE_ID.
	MarketTaxPriceID           string
	MarketTaxMeterEvent        string
	StripeTestMarketTaxPriceID string

	// TaxPartner is who works out the tax on Talyvor's own sales (B32.45): "test", the Test tax partner over the tax
	// rows, or "stripe", Stripe Tax in test mode with LENS_STRIPE_TEST_SECRET_KEY, else a test-mode
	// LENS_STRIPE_SECRET_KEY (startup fails without either). Env: LENS_TAX_PARTNER, default test.
	TaxPartner string

	// SupplierLegalName, SupplierAddress and SupplierVATNumber are what Talyvor's marketplace receipts print for the
	// supplier (B32.40). Env: LENS_SUPPLIER_LEGAL_NAME and LENS_SUPPLIER_ADDRESS, default the company and registered
	// office as the public register has them; LENS_SUPPLIER_VAT_NUMBER, default EMPTY — then every receipt reads "VAT
	// registration pending" and is a Preview.
	SupplierLegalName string
	SupplierAddress   string
	SupplierVATNumber string

	// SelfBillingVAT is whether a seller who agreed to self-billing has the VAT on their supply to Talyvor worked out
	// and paid with their payout (B32.43). Env: LENS_SELF_BILLING_VAT, default false until Talyvor's accountant confirms
	// the treatment — every self-billed invoice is then issued at zero VAT, "VAT on your supply: under review".
	SelfBillingVAT bool

	// MarketTrialMax is the most free trial uses one per_use offer may give each buyer (B32.21). Env:
	// LENS_MARKET_TRIAL_MAX, default 5 — a proposal for Nicolai; 0 lets no offer give any.
	MarketTrialMax int
	// LineageMaxShareBPS is the largest share of a remix's sales a royalty listing may ask (B32.24). Env:
	// LENS_LINEAGE_MAX_SHARE_BPS, default 3000 (30%) — a proposal for Nicolai.
	LineageMaxShareBPS int
	// LineageMaxDepth is how many generations a listing's lineage read walks up (B32.24). Env:
	// LENS_LINEAGE_MAX_DEPTH, default 5 — a proposal for Nicolai.
	LineageMaxDepth int
	// LineageTotalCapBPS is the most a listing's parents together receive of what it received from a sale (B32.26).
	// Env: LENS_LINEAGE_TOTAL_CAP_BPS, default 5000 (50%) — a proposal for Nicolai.
	LineageTotalCapBPS int
	// MarketSimilarityHold is how similar a new listing version may be to another seller's listing it does not declare
	// as a parent before it is held for review (B32.46), 0 to 1. Env: LENS_MARKET_SIMILARITY_HOLD, default 0.92 — a
	// proposal for Nicolai.
	MarketSimilarityHold float64
	// IPCounterDays is how many days after an IP claim is filed its seller may counter it (B32.47). Env:
	// LENS_IP_COUNTER_DAYS, default 10 — a proposal for Nicolai.
	IPCounterDays int
	// SellerTaxReminderDays is the days between the requests to a seller with earnings for incomplete tax details
	// (B32.41). Env: LENS_SELLER_TAX_REMINDER_DAYS, default 30 — a proposal for Nicolai.
	SellerTaxReminderDays int
	// MarketPayoutWeekday is the day of the week, UTC, sellers are paid on (B32.42). Env: LENS_MARKET_PAYOUT_WEEKDAY,
	// MON to SUN, default MON.
	MarketPayoutWeekday time.Weekday
	// RoomMessagesPerMinute is how many messages one member posts in one room in a minute (B32.30). Env:
	// LENS_ROOM_MESSAGES_PER_MINUTE, default 20 — a proposal for Nicolai.
	RoomMessagesPerMinute int
	// RoomContextMessages is how many of a room's latest messages its AI reads with a member's question (B32.33). Env:
	// LENS_ROOM_CONTEXT_MESSAGES, default 30 — a proposal for Nicolai.
	RoomContextMessages int
	// RoomReportsHide is how many workspaces' open reports take a public room off the list of rooms until the operator
	// reviews it (B32.52). Env: LENS_ROOM_REPORTS_HIDE, default 3 — a proposal for Nicolai.
	RoomReportsHide int

	// MarketPayoutReturnURL / MarketPayoutRefreshURL are where Stripe's seller onboarding returns to when a
	// seller finishes it, and when its link has expired (B20.5). Env: LENS_MARKET_PAYOUT_RETURN_URL,
	// LENS_MARKET_PAYOUT_REFRESH_URL; default the web app's selling page.
	MarketPayoutReturnURL  string
	MarketPayoutRefreshURL string

	// SubscriptionAllowanceULXC is D — the Model 2 allowance granted per billing
	// period, in µLXC (W4.6.1 step 2). Env: LENS_SUBSCRIPTION_ALLOWANCE_ULXC,
	// default 0.
	//
	// ⚠ ZERO IS A REAL STATE AND IT IS THE DEFAULT: "no allowance priced yet". No
	// grant row is written and the allowance covers nothing, so a deployment that
	// turns subscriptions on without deciding D sells a subscription that entitles
	// the buyer to nothing — which is visible immediately, rather than a default
	// somebody mistakes for a decision and ships.
	//
	// ⚠ F AND D ARE NICOLAI'S (W4.6.1). phi = F/D < 1 means the allowance is worth
	// MORE than the fee; the HARD CAP is what makes that safe, because the worst
	// case per subscriber is then EXACTLY D and can be priced rather than hoped for.
	// See docs/model2-allowance-economics.md for what the measured hit rate
	// requires of the pair — this value is not a recommendation.
	SubscriptionAllowanceULXC int64

	// StarterTestMoneyULXC is the test money a new workspace starts with, in µLXC, funded to its first agent
	// (B45.7). Env: LENS_STARTER_TEST_MONEY_ULXC, default 0 — Nicolai's to set: test credits still pay for
	// real model calls, so the amount is a cost per signup. At 0 a new workspace still gets its first agent,
	// key and rules, with no money.
	StarterTestMoneyULXC int64

	// SessionKeysEnabled gates W4.6.1 step 4 — SESSION-SCOPED KEYS, the credential a browser chat
	// holds instead of a workspace key. Env: LENS_SESSION_KEYS_ENABLED. DEFAULT FALSE.
	//
	// ⚠ OFF MEANS THE ROUTES ARE NEVER REGISTERED — a chi 404, not a route that exists and refuses.
	// This is a new AUTHZ surface (a credential that mints a credential), so a deployment that does
	// not opt in is byte-for-byte unchanged by the feature. Same posture as the H5 flags.
	SessionKeysEnabled bool

	// SessionKeyTTL is the CEILING on how long a minted session key may live. Env:
	// LENS_SESSION_KEY_TTL (Go duration, e.g. 45m). Default DefaultSessionKeyTTL.
	//
	// ⚠ IT IS A CEILING, NOT THE LIFETIME. The actual TTL is
	// min(what the caller asked for, THIS, the caller's OWN remaining session life) — the third
	// bound is the one that makes it a session key, and it is applied in
	// cmd/lens/session_key_handler.go where the caller's expiry is knowable.
	//
	// ⚠ A VALUE ABOVE MaxSessionKeyTTL IS REFUSED AT LOAD, not clamped. Clamping would let an
	// operator configure 30 days, read the config back as 30 days, and get 12 hours — a
	// disagreement between what the file says and what the process does is exactly the class of
	// defect this repo keeps finding. A MALFORMED value is refused for the same reason.
	SessionKeyTTL time.Duration

	// ROIIncludeEngineerBreakdown gates the per-engineer (author) cost
	// section of the executive ROI report (Upgrade 24). OFF by default:
	// attributing cost to named people is SENSITIVE and easily misread as a
	// productivity/surveillance metric. It is a cost ATTRIBUTION, not a
	// performance judgment. Operators opt in explicitly via
	// LENS_ROI_INCLUDE_ENGINEER_BREAKDOWN=true.
	ROIIncludeEngineerBreakdown bool

	// TLS / HTTPS (Let's Encrypt autocert).
	//
	// TLSDomain is the public hostname Lens provisions a certificate for.
	// When set, the server binds on :443 (TLS) and :80 (HTTP→HTTPS redirect).
	// When empty, Lens runs plain HTTP on ListenAddr — safe for local dev or
	// when TLS is terminated upstream (e.g. nginx, a load balancer, Cloudflare).
	// Env: LENS_TLS_DOMAIN.
	//
	// TLSCacheDir is where autocert stores the Let's Encrypt certificate and
	// private key across restarts. The directory must be writable by the Lens
	// process. Defaults to /var/cache/lens-tls.
	// Env: LENS_TLS_CACHE_DIR.
	TLSDomain   string
	TLSCacheDir string

	// CORSAllowedOrigins is a comma-separated list of origins permitted to make
	// cross-origin requests to the Lens API. Env: LENS_CORS_ALLOWED_ORIGINS.
	// Default empty — CORS is disabled and browsers enforce the same-origin
	// policy, which is correct for server-to-server API gateway deployments.
	//
	// Use "*" to allow any origin (fully-public API mode). Use a comma-separated
	// list to restrict to known frontend deployments, e.g.:
	//   LENS_CORS_ALLOWED_ORIGINS=https://app.example.com,https://admin.example.com
	CORSAllowedOrigins string

	// RedisTLS controls TLS for the Redis connection.
	// Env: LENS_REDIS_TLS. Default false.
	//
	// Two ways to enable Redis TLS:
	//   1. Use rediss:// in LENS_REDIS_URL — go-redis detects the scheme
	//      automatically and enables TLS without any extra configuration.
	//   2. Set LENS_REDIS_TLS=true — Lens forces TLS on the connection even
	//      when the URL scheme is redis://. Useful when the URL is supplied
	//      by a secrets manager that always emits redis://.
	//
	// Lens logs a startup warning when neither is set, because Redis carries
	// semantic cache data (which may include LLM prompt fragments),
	// rate-limit counters, HA heartbeats, and circuit-breaker state.
	RedisTLS bool

	// RedisTLSSkipVerify disables Redis server certificate verification.
	// Env: LENS_REDIS_TLS_SKIP_VERIFY. Default false.
	// Only appropriate for self-signed / internal-CA certificates in
	// controlled environments. Has no effect when Redis TLS is not active.
	RedisTLSSkipVerify bool

	// NatsTLS controls TLS for the NATS connection.
	// Env: LENS_NATS_TLS. Default false.
	//
	// Two ways to enable NATS TLS:
	//   1. Use tls:// or nats+tls:// in LENS_NATS_URL — the nats client
	//      detects the scheme automatically and enables TLS without any extra
	//      configuration.
	//   2. Set LENS_NATS_TLS=true — Lens forces TLS on the connection even
	//      when the URL scheme is nats://. Useful when the URL is supplied
	//      by a secrets manager that always emits nats://.
	//
	// Lens logs a startup warning when neither is set, because NATS carries
	// alert events, session turn events, learner signals, and status updates —
	// all of which may contain prompt content or workspace metadata.
	NatsTLS bool

	// NatsTLSSkipVerify disables NATS server certificate verification.
	// Env: LENS_NATS_TLS_SKIP_VERIFY. Default false.
	// Only appropriate for self-signed / internal-CA certificates in
	// controlled environments. Has no effect when NATS TLS is not active.
	NatsTLSSkipVerify bool

	// NodeTLSSkipVerify disables TLS certificate verification when Lens
	// contacts registered nodes (inference, embedding, PoVI challenge).
	// Env: LENS_NODE_TLS_SKIP_VERIFY. Default false.
	// Required when nodes present self-signed certificates — the
	// recommended default for LAN deployments (see NODE_TLS_CERT /
	// CACHE_NODE_TLS_CERT / EMBED_NODE_TLS_CERT). Has no effect when
	// nodes serve plain HTTP.
	NodeTLSSkipVerify bool

	// NodePrivateCIDRAllowlist (Phase-0 Item E) opts SPECIFIC private CIDRs back into
	// the node call-out SSRF guard, for an internal node network on a private subnet.
	// Comma-separated CIDRs (e.g. "10.42.0.0/16,192.168.10.0/24"). EMPTY (default) =
	// today's behavior EXACTLY — all private ranges blocked. Cloud metadata
	// (169.254.169.254) is NEVER allowlistable (safehttp refuses it, fail-loud at
	// boot). Env: LENS_NODE_PRIVATE_CIDR_ALLOWLIST.
	NodePrivateCIDRAllowlist string

	// DBSSLMode controls TLS for the Postgres connection.
	// Env: LENS_DB_SSL_MODE. Default "require".
	//
	// Valid values mirror the libpq sslmode parameter:
	//   disable     — no TLS; only appropriate for localhost / trusted private
	//                 networks.  Logs a startup warning when set.
	//   allow       — try plain TCP first, upgrade to TLS if the server
	//                 demands it.  Not recommended (MITM-capable).
	//   prefer      — try TLS first, fall back to plain TCP.  pgx default
	//                 when sslmode is omitted; not suitable for production.
	//   require     — always use TLS; skip server certificate verification.
	//                 Safe for most managed databases (RDS, Supabase, Neon,
	//                 etc.) that present TLS certs signed by a private CA.
	//   verify-ca   — require TLS and verify the server cert is signed by a
	//                 trusted CA (set sslrootcert in the DSN or via PGSSLROOTCERT).
	//   verify-full — require TLS, verify CA, and verify the server hostname.
	//                 Strongest option; requires a valid cert chain.
	DBSSLMode string

	// DBPgBouncer switches pgxpool to simple-query protocol (no prepared
	// statements), required when Lens connects through PgBouncer in
	// transaction pooling mode. Env: LENS_DB_PGBOUNCER. Default false.
	// The Helm chart sets this automatically when pgbouncer.enabled=true.
	DBPgBouncer bool

	// DBMaxConns caps the pgxpool at this many open server connections.
	// Env: LENS_DB_MAX_CONNS. Default 25 (direct Postgres); raise to ~100
	// when a PgBouncer in transaction mode sits in front because PgBouncer
	// multiplexes many cheap client connections onto a smaller pool of real
	// server connections.
	DBMaxConns int32

	// DBMinConns is the number of idle connections pgxpool keeps open so
	// the first burst of requests after a quiet period isn't bottlenecked
	// on connection establishment. Env: LENS_DB_MIN_CONNS. Default 2.
	DBMinConns int32

	// DBReplicaURL is an OPTIONAL read-replica DSN (U8/U9). When set, the
	// recon-confirmed analytics/display reads (forecast, ROI, dashboard
	// stats, admin read-only lists, rate/history) are routed to it; ALL
	// money/authz/transaction reads stay on the PRIMARY by construction —
	// the money/authz structs are never handed this handle. Empty (default)
	// → every read uses the primary pool, byte-identical to today. A
	// malformed URL or an unreachable replica at boot falls back to the
	// primary with a WARN: the replica is an optimization, never a boot
	// dependency. When LENS_DB_PGBOUNCER is set, the replica pool also uses
	// simple protocol. Env: LENS_DB_REPLICA_URL.
	DBReplicaURL string

	// ObsWriteMaxConcurrent caps how many post-serve observational writes
	// (attribution records + routing-pattern capture) may run at once.
	// Past the cap, writes are DROPPED (they are observational by contract)
	// rather than queued — unbounded, they convert overload into
	// pool-connection churn that can exhaust PgBouncer's max_client_conn
	// (#122; observed empirically 2026-06-11). Env:
	// LENS_OBS_WRITE_MAX_CONCURRENT. Default 32 (≈ a third of the
	// PgBouncer-mode pool). 0 disables the bound entirely.
	ObsWriteMaxConcurrent int

	// High Availability (Upgrade 7). HA is strictly opt-in via
	// LENS_HA_ENABLED; when false (the default) the process runs as a
	// single instance exactly as it did before HA existed. Enabling HA
	// requires Redis (LENS_REDIS_URL) — which Lens already requires — for
	// the instance registry, shared rate limiter, and breaker gossip.
	HAEnabled bool
	// Production is LENS_ENV=production (B50.3): Lens refuses to start without the signing keys it would otherwise
	// make afresh at each start, silently, so every credential it signed stops verifying after a restart.
	Production bool
	// HAHeartbeat is how often this instance refreshes its registry key.
	// HAInstanceTTL is the TTL on that key, so a crashed instance
	// disappears after a few missed heartbeats. HADrainTimeout bounds
	// graceful shutdown — in-flight requests get this long to finish.
	HAHeartbeat    time.Duration
	HAInstanceTTL  time.Duration
	HADrainTimeout time.Duration

	// DistillWorkerBin is the path to the compiled distill-worker binary that
	// distill.ProcessIsolator spawns to run document conversion in a killable,
	// memory-limited subprocess (the stage-3 resource-isolation envelope). Env:
	// LENS_DISTILL_WORKER_BIN. Defaults to the distill-worker binary sitting
	// beside the running lens executable, so the Docker image (which ships both
	// binaries in the same directory) works with no config. Nothing in the
	// serving path uses it yet — the request-path integration that constructs
	// the isolator with this path is a later PR.
	DistillWorkerBin string
}

// Proof-of-Improvement mint rate defaults (LENS per readout-point). Exported as the SINGLE SOURCE OF TRUTH
// so money-path tests bind to the SAME value production runs at (not a lookalike literal). Env overrides:
// LENS_EVAL_CONTRIBUTION_RATE_PER_POINT / LENS_ROUTING_PREDICTION_RATE_PER_POINT.
const (
	// DefaultEvalContributionRatePerPoint — LENS per discrimination-point for proof-of-eval-contribution.
	DefaultEvalContributionRatePerPoint = 0.05
	// DefaultRoutingPredictionRatePerPoint — LENS per skill-margin-point for proof-of-routing-prediction.
	DefaultRoutingPredictionRatePerPoint = 0.02
)

func Load() (*Config, error) {
	c := &Config{
		ListenAddr:        getEnv("LENS_LISTEN_ADDR", "0.0.0.0:8080"),
		RedisURL:          os.Getenv("LENS_REDIS_URL"),
		DatabaseURL:       os.Getenv("LENS_DATABASE_URL"),
		DBReplicaURL:      os.Getenv("LENS_DB_REPLICA_URL"),
		NatsURL:           os.Getenv("LENS_NATS_URL"),
		OpenAIAPIKey:      os.Getenv("LENS_OPENAI_API_KEY"),
		AnthropicAPIKey:   os.Getenv("LENS_ANTHROPIC_API_KEY"),
		GoogleAPIKey:      os.Getenv("LENS_GOOGLE_API_KEY"),
		DeepgramAPIKey:    os.Getenv("LENS_DEEPGRAM_API_KEY"),
		DeepgramURL:       getEnv("LENS_DEEPGRAM_URL", "https://api.eu.deepgram.com"),
		EmbeddingModel:    getEnv("LENS_EMBEDDING_MODEL", DefaultEmbeddingModel),
		EmbeddingBaseURL:  os.Getenv("LENS_EMBEDDING_BASE_URL"),
		SemanticThreshold: DefaultSemanticThreshold,
		MaxCacheTTL:       24 * time.Hour,
		LogLevel:          getEnv("LENS_LOG_LEVEL", "info"),
		OllamaURL:         getEnv("LENS_OLLAMA_URL", "http://localhost:11434"),

		AWSRegion:          getEnv("LENS_AWS_REGION", "us-east-1"),
		AWSAccessKeyID:     os.Getenv("LENS_AWS_ACCESS_KEY_ID"),
		AWSSecretAccessKey: os.Getenv("LENS_AWS_SECRET_ACCESS_KEY"),
		AWSSessionToken:    os.Getenv("LENS_AWS_SESSION_TOKEN"),

		MistralAPIKey: os.Getenv("LENS_MISTRAL_API_KEY"),
		GroqAPIKey:    os.Getenv("LENS_GROQ_API_KEY"),
		VLLMBaseURL:   os.Getenv("LENS_VLLM_BASE_URL"),
		VLLMAPIKey:    os.Getenv("LENS_VLLM_API_KEY"),

		CachePoolableEnabled:   parseBoolEnv("LENS_CACHE_POOLABLE_ENABLED"),
		DistillPoolableEnabled: parseBoolEnv("LENS_DISTILL_POOLABLE_ENABLED"),
		PatternMiningEnabled:   parseBoolEnv("LENS_PATTERN_MINING_ENABLED"),
		POVIMintingEnabled:     parseBoolEnv("LENS_POVI_MINTING_ENABLED"),

		AnnotationMintingEnabled: parseBoolEnv("LENS_ANNOTATION_MINTING_ENABLED"), // spendable-immediate mint → explicit opt-in (default off)

		PoolRoyaltyMintingEnabled: parseBoolEnv("LENS_POOL_ROYALTY_MINTING_ENABLED"),
		LXCShadowSpendEnabled:     parseBoolEnv("LENS_LXC_SHADOW_SPEND_ENABLED"),
		LXCGatingEnabled:          parseBoolEnv("LENS_LXC_GATING_ENABLED"),
		AdminLXCGrantEnabled:      parseBoolEnv("LENS_ADMIN_LXC_GRANT_ENABLED"),
		PatternCaptureEnabled:     parseBoolEnv("LENS_PATTERN_CAPTURE_ENABLED"),
		PoolShadowLogEnabled:      parseBoolEnv("LENS_POOL_SHADOW_LOG_ENABLED"),
		PatternEarningEnabled:     parseBoolEnv("LENS_PATTERN_EARNING_ENABLED"),

		ROIIncludeEngineerBreakdown: parseBoolEnv("LENS_ROI_INCLUDE_ENGINEER_BREAKDOWN"),

		GuardrailsEnabled:             parseBoolEnv("LENS_GUARDRAILS_ENABLED"),
		WorkTierEnabled:               parseBoolEnv("LENS_WORKTIER_ENABLED"),
		K4VerifierEnabled:             parseBoolEnv("LENS_K4_VERIFIER_ENABLED"),
		RoutingDecisionCaptureEnabled: parseBoolEnvDefaultTrue("LENS_ROUTING_DECISION_CAPTURE_ENABLED"),
		KeelRoyaltyHaircutEnabled:     parseBoolEnvDefaultTrue("LENS_KEEL_ROYALTY_HAIRCUT_ENABLED"),
		H5BondsEnabled:                parseBoolEnv("LENS_H5_BONDS_ENABLED"),
		H5AttestEnabled:               parseBoolEnv("LENS_H5_ATTEST_ENABLED"),
		H5ArtifactEnabled:             parseBoolEnv("LENS_H5_ARTIFACT_ENABLED"),
		NodeLatencyCaptureEnabled:     parseBoolEnv("LENS_NODE_LATENCY_CAPTURE_ENABLED"),
		NodeAttestationVerifyEnabled:  parseBoolEnv("LENS_NODE_ATTESTATION_VERIFY_ENABLED"),

		RoutingIntelligenceEnabled:      parseBoolEnv("LENS_ROUTING_INTELLIGENCE_ENABLED"),
		RoutingTierCohortsEnabled:       parseBoolEnv("LENS_ROUTING_TIER_COHORTS_ENABLED"),
		RoutingBrainEnabled:             parseBoolEnv("LENS_ROUTING_BRAIN_ENABLED"),
		NodeAutoRouteEnabled:            parseBoolEnv("LENS_NODE_AUTOROUTE_ENABLED"),
		ReputationBondedMintingEnabled:  parseBoolEnv("LENS_REPUTATION_BONDED_MINTING_ENABLED"),
		ProofOfBenchmarkEnabled:         parseBoolEnv("LENS_PROOF_OF_BENCHMARK_ENABLED"),
		ProofOfImprovementEnabled:       parseBoolEnv("LENS_PROOF_OF_IMPROVEMENT_ENABLED"),
		EvalContributionMintingEnabled:  parseBoolEnv("LENS_EVAL_CONTRIBUTION_MINTING_ENABLED"),
		RoutingPredictionMintingEnabled: parseBoolEnv("LENS_ROUTING_PREDICTION_MINTING_ENABLED"),
		LatencyMintingEnabled:           parseBoolEnv("LENS_LATENCY_MINTING_ENABLED"),
		ConfidentialMintingEnabled:      parseBoolEnv("LENS_CONFIDENTIAL_MINTING_ENABLED"),
		RoutingPredictionEnabled:        parseBoolEnv("LENS_ROUTING_PREDICTION_ENABLED"),
		RoutingPredictionScoringEnabled: parseBoolEnv("LENS_ROUTING_PREDICTION_SCORING_ENABLED"),

		BillingEnabled:              parseBoolEnv("LENS_BILLING_ENABLED"),
		EarnRequireLivePurchase:     parseBoolEnv("LENS_EARN_REQUIRE_LIVE_PURCHASE"),
		DashboardEnabled:            parseBoolEnv("LENS_DASHBOARD_ENABLED"),
		StripeSecretKey:             os.Getenv("LENS_STRIPE_SECRET_KEY"),
		StripeWebhookSecret:         os.Getenv("LENS_STRIPE_WEBHOOK_SECRET"),
		StripeTestSecretKey:         os.Getenv("LENS_STRIPE_TEST_SECRET_KEY"),
		StripeTestWebhookSecret:     os.Getenv("LENS_STRIPE_TEST_WEBHOOK_SECRET"),
		StripeTestSubscriptionPlans: parsePlans(os.Getenv("LENS_STRIPE_TEST_SUBSCRIPTION_PLANS")),
		StripeTestMarketBillPriceID: getEnv("LENS_STRIPE_TEST_MARKET_BILL_PRICE_ID", ""),
		StripeIssuingWebhookSecret:  os.Getenv("LENS_STRIPE_ISSUING_WEBHOOK_SECRET"),
		MoneySandboxWebhookSecret:   os.Getenv("LENS_MONEY_SANDBOX_WEBHOOK_SECRET"),
		StripeIssuingCurrency:       getEnv("LENS_STRIPE_ISSUING_CURRENCY", "gbp"),
		// Catalog-drift detection (internal/modelwatch). Poller default-ON: it is read-only, costs one
		// provider GET an hour, and its absence is what let a whole model family be served free. The
		// SINK is separate and, if unset, reported as a boot-time ERROR rather than silently skipped.
		ModelWatchEnabled:          parseBoolEnvDefaultTrue("LENS_MODEL_WATCH_ENABLED"),
		OperatorAlertWebhookURL:    os.Getenv("LENS_OPERATOR_ALERT_WEBHOOK_URL"),
		OperatorAlertWebhookSecret: os.Getenv("LENS_OPERATOR_ALERT_WEBHOOK_SECRET"),
		TrackWebhookURL:            os.Getenv("LENS_TRACK_WEBHOOK_URL"),
		TrackWebhookSecret:         os.Getenv("LENS_TRACK_WEBHOOK_SECRET"),
		BillingSubscriptionPriceID: getEnv("LENS_BILLING_SUBSCRIPTION_PRICE_ID", ""),
		BillingSubscriptionPlans:   parsePlans(os.Getenv("LENS_BILLING_SUBSCRIPTION_PLANS")),
		MarketBillPriceID:          getEnv("LENS_MARKET_BILL_PRICE_ID", ""),
		MarketMeterEvent:           getEnv("LENS_MARKET_METER_EVENT", "talyvor_marketplace_use"),
		MarketTaxPriceID:           getEnv("LENS_MARKET_TAX_PRICE_ID", ""),
		MarketTaxMeterEvent:        getEnv("LENS_MARKET_TAX_METER_EVENT", "talyvor_marketplace_tax"),
		StripeTestMarketTaxPriceID: getEnv("LENS_STRIPE_TEST_MARKET_TAX_PRICE_ID", ""),
		TaxPartner:                 getEnv("LENS_TAX_PARTNER", "test"),
		SupplierLegalName:          getEnv("LENS_SUPPLIER_LEGAL_NAME", "TALYVOR LTD"),
		SupplierAddress:            getEnv("LENS_SUPPLIER_ADDRESS", "71-75 Shelton Street, Covent Garden, London, United Kingdom, WC2H 9JQ"),
		SupplierVATNumber:          getEnv("LENS_SUPPLIER_VAT_NUMBER", ""),
		SelfBillingVAT:             parseBoolEnv("LENS_SELF_BILLING_VAT"),
		MarketPayoutReturnURL:      getEnv("LENS_MARKET_PAYOUT_RETURN_URL", "https://app.talyvor.com/marketplace/selling?payouts=connected"),
		MarketPayoutRefreshURL:     getEnv("LENS_MARKET_PAYOUT_REFRESH_URL", "https://app.talyvor.com/marketplace/selling?payouts=expired"),
		BillingSuccessURL:          getEnv("LENS_BILLING_SUCCESS_URL", "https://app.talyvor.com/billing/success?session_id={CHECKOUT_SESSION_ID}"),
		BillingCancelURL:           getEnv("LENS_BILLING_CANCEL_URL", "https://app.talyvor.com/billing/cancel"),

		LocalEndpoints: os.Getenv("LENS_LOCAL_ENDPOINTS"),

		TLSDomain:   os.Getenv("LENS_TLS_DOMAIN"),
		TLSCacheDir: getEnv("LENS_TLS_CACHE_DIR", "/var/cache/lens-tls"),

		CORSAllowedOrigins: os.Getenv("LENS_CORS_ALLOWED_ORIGINS"),

		RedisTLS:           parseBoolEnv("LENS_REDIS_TLS"),
		RedisTLSSkipVerify: parseBoolEnv("LENS_REDIS_TLS_SKIP_VERIFY"),

		NatsTLS:           parseBoolEnv("LENS_NATS_TLS"),
		NatsTLSSkipVerify: parseBoolEnv("LENS_NATS_TLS_SKIP_VERIFY"),

		NodeTLSSkipVerify:        parseBoolEnv("LENS_NODE_TLS_SKIP_VERIFY"),
		NodePrivateCIDRAllowlist: os.Getenv("LENS_NODE_PRIVATE_CIDR_ALLOWLIST"),

		JWTPrivateKey:   os.Getenv("LENS_JWT_PRIVATE_KEY"),
		TokenTTL:        24 * time.Hour,
		ProvisionSecret: os.Getenv("LENS_PROVISION_SECRET"),
		SyntheticKey:    os.Getenv("LENS_SYNTHETIC_KEY"),

		WebAuthnRPID:          getEnv("LENS_WEBAUTHN_RP_ID", "app.talyvor.com"),
		WebAuthnOrigins:       strings.Split(getEnv("LENS_WEBAUTHN_ORIGINS", "https://app.talyvor.com"), ","),
		DeviceAppleAppID:      os.Getenv("LENS_DEVICE_APPLE_APP_ID"),
		APNsKey:               os.Getenv("LENS_APNS_KEY"),
		APNsKeyID:             os.Getenv("LENS_APNS_KEY_ID"),
		APNsTeamID:            os.Getenv("LENS_APNS_TEAM_ID"),
		APNsTopic:             os.Getenv("LENS_APNS_TOPIC"),
		APNsSandbox:           os.Getenv("LENS_APNS_SANDBOX") == "true",
		FCMCredentials:        os.Getenv("LENS_FCM_CREDENTIALS"),
		DeviceTestAttestation: os.Getenv("LENS_DEVICE_TEST_ATTESTATION") == "true",
		VAPIDPrivateKey:       os.Getenv("LENS_VAPID_PRIVATE_KEY"),
		VAPIDSubject:          getEnv("LENS_VAPID_SUBJECT", "mailto:support@talyvor.com"),

		HAEnabled: parseBoolEnv("LENS_HA_ENABLED"),

		DistillWorkerBin: getEnv("LENS_DISTILL_WORKER_BIN", defaultDistillWorkerBin()),

		DBSSLMode:   getEnv("LENS_DB_SSL_MODE", "require"),
		DBPgBouncer: parseBoolEnv("LENS_DB_PGBOUNCER"),
		DBMaxConns:  25,
		DBMinConns:  2,

		ObsWriteMaxConcurrent: 32,
	}

	// Pool size overrides.
	if v := os.Getenv("LENS_DB_MAX_CONNS"); v != "" {
		// ParseInt with bitSize 32 rejects values that don't fit int32, so the
		// int32(n) cast below can't silently overflow (a huge value errors here
		// instead of wrapping to a negative/garbage pool size).
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid LENS_DB_MAX_CONNS (must be an integer in [1, %d]): %s", math.MaxInt32, v)
		}
		c.DBMaxConns = int32(n)
	}
	if v := os.Getenv("LENS_DB_MIN_CONNS"); v != "" {
		// bitSize 32 → the int32(n) cast can't overflow (see LENS_DB_MAX_CONNS).
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_DB_MIN_CONNS (must be an integer in [0, %d]): %s", math.MaxInt32, v)
		}
		c.DBMinConns = int32(n)
	}
	if v := os.Getenv("LENS_OBS_WRITE_MAX_CONCURRENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_OBS_WRITE_MAX_CONCURRENT (must be ≥ 0; 0 disables the bound): %s", v)
		}
		c.ObsWriteMaxConcurrent = n
	}

	if err := ValidateDBSSLMode(c.DBSSLMode); err != nil {
		return nil, err
	}

	// U18b: billing moves fiat money — refuse to start half-configured. The error
	// names the env vars but NEVER echoes a key value.
	if c.BillingEnabled && (c.StripeSecretKey == "" || c.StripeWebhookSecret == "") {
		return nil, errors.New(
			"LENS_BILLING_ENABLED=true requires both LENS_STRIPE_SECRET_KEY and " +
				"LENS_STRIPE_WEBHOOK_SECRET (one or both are unset)")
	}
	// B25.2: the test workspaces' key moves test money only. A live key there would charge real cards for
	// test credits, so it refuses to start — naming the variable, never echoing the key.
	if k := c.StripeTestSecretKey; k != "" && !strings.HasPrefix(k, "sk_test_") && !strings.HasPrefix(k, "rk_test_") {
		return nil, errors.New("LENS_STRIPE_TEST_SECRET_KEY must be a Stripe test-mode key (sk_test_… or rk_test_…)")
	}
	// B32.45: Stripe Tax answers in test mode only, so it needs a test-mode key.
	switch c.TaxPartner {
	case "test":
	case "stripe":
		if k := c.StripeSecretKey; c.StripeTestSecretKey == "" && !strings.HasPrefix(k, "sk_test_") && !strings.HasPrefix(k, "rk_test_") {
			return nil, errors.New("LENS_TAX_PARTNER=stripe needs a Stripe test-mode key: LENS_STRIPE_TEST_SECRET_KEY, or a test-mode LENS_STRIPE_SECRET_KEY")
		}
	default:
		return nil, fmt.Errorf("LENS_TAX_PARTNER is test or stripe, not %q", c.TaxPartner)
	}

	// HA timers, expressed in whole seconds. Defaults match the
	// documented values; a non-positive override is a misconfiguration.
	c.HAHeartbeat = 5 * time.Second
	c.HAInstanceTTL = 15 * time.Second
	c.HADrainTimeout = 30 * time.Second
	for _, hf := range []struct {
		env string
		dst *time.Duration
	}{
		{"LENS_HA_HEARTBEAT_SEC", &c.HAHeartbeat},
		{"LENS_HA_INSTANCE_TTL_SEC", &c.HAInstanceTTL},
		{"LENS_HA_DRAIN_TIMEOUT_SEC", &c.HADrainTimeout},
	} {
		if v := os.Getenv(hf.env); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("invalid %s (must be ≥ 1): %s", hf.env, v)
			}
			*hf.dst = time.Duration(n) * time.Second
		}
	}

	if v := os.Getenv("LENS_TOKEN_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_TOKEN_TTL: %w", err)
		}
		c.TokenTTL = d
	}

	// Migration guard: LENS_JWT_SECRET (old HS256) is no longer accepted.
	// Operators must switch to an EC P-256 private key.
	if os.Getenv("LENS_JWT_SECRET") != "" {
		return nil, errors.New(
			"LENS_JWT_SECRET is no longer supported — Lens now uses ES256 (EC P-256). " +
				"Generate a key with:\n" +
				"  openssl ecparam -name prime256v1 -genkey -noout | \\\n" +
				"    openssl pkcs8 -topk8 -nocrypt -out ec-private.pem\n" +
				"  export LENS_JWT_PRIVATE_KEY=$(cat ec-private.pem)\n" +
				"Existing HS256 tokens will not be accepted after the upgrade. " +
				"Rotate all tokens on the first deployment with the new key",
		)
	}

	if v := os.Getenv("LENS_GLOBAL_RPM"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_GLOBAL_RPM: %s", v)
		}
		c.GlobalRPM = n
	}
	if v := os.Getenv("LENS_GLOBAL_TPM"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_GLOBAL_TPM: %s", v)
		}
		c.GlobalTPM = n
	}
	c.BurstMultiplier = 1.5
	if v := os.Getenv("LENS_BURST_MULTIPLIER"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 1.0 {
			return nil, fmt.Errorf("invalid LENS_BURST_MULTIPLIER (must be ≥ 1.0): %s", v)
		}
		c.BurstMultiplier = f
	}

	c.POVIMinStake = 100.0
	if v := os.Getenv("LENS_POVI_MIN_STAKE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return nil, fmt.Errorf("invalid LENS_POVI_MIN_STAKE (must be ≥ 0): %s", v)
		}
		c.POVIMinStake = f
	}
	// EvalContributionRatePerPoint: LENS per discrimination-point. Default 0.05 (NOT 0) — inert by the
	// earning flags (default off), not by the rate; a 0 env override would nil the anchor.
	// At 0.05 LENS/discrimination-point: amount = 0.05 × clamp01(discrimination), so a max-
	// discriminating eval pays ≤ 0.05 LENS ($0.005 @ $0.10 peg) ONCE per item, and only after
	// ≥3 INDEPENDENT graders actually scored it (usage-anchored) — far below the value of the
	// accurate model-scoring that eval enables across the fleet. (Minting still gated by the
	// enable flags, default off.)
	c.EvalContributionRatePerPoint = DefaultEvalContributionRatePerPoint
	if v := os.Getenv("LENS_EVAL_CONTRIBUTION_RATE_PER_POINT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return nil, fmt.Errorf("invalid LENS_EVAL_CONTRIBUTION_RATE_PER_POINT (must be ≥ 0): %s", v)
		}
		c.EvalContributionRatePerPoint = f
	}

	// RoutingPredictionRatePerPoint — set 0.02 LENS/skill-point: amount = 0.02 × clamp01(skill_margin),
	// so a prediction pays ≤ 0.02 LENS ($0.002 @ $0.10 peg) ONCE per scored prediction, and only when
	// the predicted model genuinely BEAT the baseline on the author-excluded held slice (margin>0). A
	// better-routing prediction improves EVERY future request in its cohort (compounding COGS/quality
	// value) — so 0.02 LENS is far below the value of the decision it improved. (Minting still gated by
	// the enable flags, default off.)
	c.RoutingPredictionRatePerPoint = DefaultRoutingPredictionRatePerPoint
	if v := os.Getenv("LENS_ROUTING_PREDICTION_RATE_PER_POINT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return nil, fmt.Errorf("invalid LENS_ROUTING_PREDICTION_RATE_PER_POINT (must be ≥ 0): %s", v)
		}
		c.RoutingPredictionRatePerPoint = f
	}

	// LatencyRatePerPoint — DEFAULT 0 (INERT: the held-benchmark anchor refuses a non-positive rate → the
	// latency minter is a total no-op). A deliberate later flip, set only when the economy goes live.
	c.LatencyRatePerPoint = 0
	if v := os.Getenv("LENS_LATENCY_RATE_PER_POINT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return nil, fmt.Errorf("invalid LENS_LATENCY_RATE_PER_POINT (must be ≥ 0): %s", v)
		}
		c.LatencyRatePerPoint = f
	}

	// GatewayPriceCeiling — DEFAULT 0.50 (10× the 0.050 node band; legitimate nodes never clamped). A routing
	// bound used only by StrategyPriceAware; must be > 0. Env: LENS_GATEWAY_PRICE_CEILING.
	c.GatewayPriceCeiling = 0.50
	if v := os.Getenv("LENS_GATEWAY_PRICE_CEILING"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			return nil, fmt.Errorf("invalid LENS_GATEWAY_PRICE_CEILING (must be > 0): %s", v)
		}
		c.GatewayPriceCeiling = f
	}

	// ConfidentialRatePerPoint — DEFAULT 0 (INERT: the anchor refuses a non-positive rate → the confidential
	// minter is a total no-op). A deliberate later flip, set only at CC-hardware go-live.
	c.ConfidentialRatePerPoint = 0
	if v := os.Getenv("LENS_CONFIDENTIAL_RATE_PER_POINT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return nil, fmt.Errorf("invalid LENS_CONFIDENTIAL_RATE_PER_POINT (must be ≥ 0): %s", v)
		}
		c.ConfidentialRatePerPoint = f
	}

	c.POVIUnbondPeriod = 7 * 24 * time.Hour
	if v := os.Getenv("LENS_POVI_UNBOND_PERIOD"); v != "" {
		d, err := time.ParseDuration(v) // Go duration units (h/m/s); e.g. 168h = 7 days
		if err != nil || d < 0 {
			return nil, fmt.Errorf("invalid LENS_POVI_UNBOND_PERIOD (Go duration, e.g. 168h): %s", v)
		}
		c.POVIUnbondPeriod = d
	}
	c.POVIChallengeRate = 0.1
	if v := os.Getenv("LENS_POVI_CHALLENGE_RATE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			return nil, fmt.Errorf("invalid LENS_POVI_CHALLENGE_RATE (must be in [0,1]): %s", v)
		}
		c.POVIChallengeRate = f
	}
	c.PoolMintCapPerPair = 50 // Phase-0 Item D: ON by default. Bounds A→B pool-royalty farming to 50×s×avoided_COGS/pair/24h (~$25/pair/day at s=0.5) even if the owner-linkage guard misses; 0 (env) disables.
	if v := os.Getenv("LENS_POOL_MINT_CAP_PER_PAIR"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_POOL_MINT_CAP_PER_PAIR (must be ≥ 0; 0 disables): %s", v)
		}
		c.PoolMintCapPerPair = n
	}
	c.PoolMintCapPerEntry = 0
	if v := os.Getenv("LENS_POOL_MINT_CAP_PER_ENTRY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_POOL_MINT_CAP_PER_ENTRY (must be >= 0; 0 disables): %s", v)
		}
		c.PoolMintCapPerEntry = n
	}
	c.ApprovalTTL = 24 * time.Hour
	if v := os.Getenv("LENS_APPROVAL_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_APPROVAL_TTL (Go duration > 0, e.g. 24h): %s", v)
		}
		c.ApprovalTTL = d
	}
	c.PoolMintCapWindow = 24 * time.Hour
	if v := os.Getenv("LENS_POOL_MINT_CAP_WINDOW"); v != "" {
		d, err := time.ParseDuration(v) // Go duration units, e.g. 48h
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_POOL_MINT_CAP_WINDOW (Go duration > 0, e.g. 48h): %s", v)
		}
		c.PoolMintCapWindow = d
	}
	// PR1 distill mint caps (separate budget from the cache caps; default 0/0 off + 24h).
	c.DistillMintCapPerPair = 0
	if v := os.Getenv("LENS_DISTILL_MINT_CAP_PER_PAIR"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_DISTILL_MINT_CAP_PER_PAIR (must be >= 0; 0 disables): %s", v)
		}
		c.DistillMintCapPerPair = n
	}
	c.DistillMintCapPerContent = 0
	if v := os.Getenv("LENS_DISTILL_MINT_CAP_PER_CONTENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_DISTILL_MINT_CAP_PER_CONTENT (must be >= 0; 0 disables): %s", v)
		}
		c.DistillMintCapPerContent = n
	}
	c.DistillMintCapWindow = 24 * time.Hour
	if v := os.Getenv("LENS_DISTILL_MINT_CAP_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_DISTILL_MINT_CAP_WINDOW (Go duration > 0, e.g. 48h): %s", v)
		}
		c.DistillMintCapWindow = d
	}
	// S2 routing-pattern earn cap — REAL default (50000), not 0 (see the field
	// comment); 0 = explicit ops-disable.
	c.PatternEarnCapPerWorkspace = 50000
	if v := os.Getenv("LENS_PATTERN_EARN_CAP_PER_WORKSPACE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_PATTERN_EARN_CAP_PER_WORKSPACE (must be ≥ 0; 0 disables): %s", v)
		}
		c.PatternEarnCapPerWorkspace = n
	}
	c.PatternEarnCapWindow = 24 * time.Hour
	if v := os.Getenv("LENS_PATTERN_EARN_CAP_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_PATTERN_EARN_CAP_WINDOW (Go duration > 0, e.g. 48h): %s", v)
		}
		c.PatternEarnCapWindow = d
	}
	// U6 PR2 per-identity mint rate cap — REAL default (1000 LENS/24h), not 0
	// (default-ON-with-a-ceiling; inaction stays safe). 0 = explicit ops-disable;
	// negative is rejected. A safety restriction, NOT economy-killswitched.
	c.MintRateCapLENS24h = 1000
	if v := os.Getenv("LENS_MINT_RATE_CAP_LENS_24H"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return nil, fmt.Errorf("invalid LENS_MINT_RATE_CAP_LENS_24H (must be ≥ 0; 0 disables): %s", v)
		}
		c.MintRateCapLENS24h = f
	}
	c.PoolHoldbackWindow = 72 * time.Hour
	if v := os.Getenv("LENS_POOL_HOLDBACK_WINDOW"); v != "" {
		d, err := time.ParseDuration(v) // Go duration units, e.g. 72h
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_POOL_HOLDBACK_WINDOW (Go duration > 0, e.g. 72h): %s", v)
		}
		c.PoolHoldbackWindow = d
	}
	// Detector thresholds (Stage 2.3b) — defaults, then env overrides with
	// range validation (the NaN/negative lesson).
	c.DetectVolumeMinMints = 50
	if v := os.Getenv("LENS_DETECT_VOLUME_MIN_MINTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_DETECT_VOLUME_MIN_MINTS (must be >= 0): %s", v)
		}
		c.DetectVolumeMinMints = n
	}
	c.DetectVolumeMaxRequesters = 2
	if v := os.Getenv("LENS_DETECT_VOLUME_MAX_REQUESTERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_DETECT_VOLUME_MAX_REQUESTERS (must be >= 0): %s", v)
		}
		c.DetectVolumeMaxRequesters = n
	}
	c.DetectBilateralMinFrac = 0.9
	if v := os.Getenv("LENS_DETECT_BILATERAL_MIN_FRAC"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || f < 0 || f > 1 {
			return nil, fmt.Errorf("invalid LENS_DETECT_BILATERAL_MIN_FRAC (must be in [0,1]): %s", v)
		}
		c.DetectBilateralMinFrac = f
	}
	c.DetectBilateralMinMints = 20
	if v := os.Getenv("LENS_DETECT_BILATERAL_MIN_MINTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_DETECT_BILATERAL_MIN_MINTS (must be >= 0): %s", v)
		}
		c.DetectBilateralMinMints = n
	}
	c.DetectSimilarityMinSample = 30
	if v := os.Getenv("LENS_DETECT_SIMILARITY_MIN_SAMPLE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid LENS_DETECT_SIMILARITY_MIN_SAMPLE (must be >= 1): %s", v)
		}
		c.DetectSimilarityMinSample = n
	}
	c.DetectSimilarityMaxStddev = 0.02
	if v := os.Getenv("LENS_DETECT_SIMILARITY_MAX_STDDEV"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || f < 0 {
			return nil, fmt.Errorf("invalid LENS_DETECT_SIMILARITY_MAX_STDDEV (must be >= 0): %s", v)
		}
		c.DetectSimilarityMaxStddev = f
	}

	// Detector sweep — DEFAULT-TRUE (the smoke detector accompanies minting automatically);
	// runs iff EconomyEnabled too (wired in main.go). The flag is a manual off-switch only.
	c.DetectorSweepEnabled = true
	if os.Getenv("LENS_DETECTOR_SWEEP_ENABLED") != "" {
		c.DetectorSweepEnabled = parseBoolEnv("LENS_DETECTOR_SWEEP_ENABLED")
	}

	// Keel (U25) cross-tenant drift attribution.
	// DEFAULT-ON — closed-test, no external tenants; runs on synthetic/internal corpus so the mechanism is
	// observable. Recalibrate at N3 turn-on. A fresh deploy with no LENS_KEEL_ENABLED env runs the sweep;
	// the env var can still force it OFF (LENS_KEEL_ENABLED=false).
	c.KeelEnabled = true
	if os.Getenv("LENS_KEEL_ENABLED") != "" {
		c.KeelEnabled = parseBoolEnv("LENS_KEEL_ENABLED")
	}
	// PLACEHOLDER — chosen so the detector emits observably in closed-test; NOT a calibrated production
	// threshold. MUST be recalibrated against real-scale distribution at N3 turn-on before any external traffic.
	c.KeelDeviationSigma = 2.0
	if v := os.Getenv("LENS_KEEL_DEVIATION_SIGMA"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			c.KeelDeviationSigma = f
		}
	}
	c.KeelWindowSeconds = 3600
	if v := os.Getenv("LENS_KEEL_WINDOW_SECONDS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.KeelWindowSeconds = n
		}
	}
	// Constants, not env-loaded — see the field comments. Anything that makes these
	// configurable must add the read AND the .env.example line together, or
	// internal/config's env census (W6.23) will say so.
	c.KeelInterval = time.Hour
	c.KeelLookback = 48 * time.Hour

	// Keel HARDENED (K3) — DEFAULT-ON (closed-test): the money-grade leave-one-out detector runs and produces
	// the hardened findings the reduce-only haircut acts on. Safe by the same closed-test logic as the haircut
	// — Talyvor has no external contributors, so a reduction takes nothing from anyone real, and running it is
	// the only way to observe the full drift→haircut chain before N3. ⚠ ALL THRESHOLDS (sigma, MoneyCohortFloor,
	// MinSamples, PersistenceWindows) REMAIN UNCALIBRATED PLACEHOLDERS — recalibrate at N3 before any external
	// contributor earns. Env LENS_KEEL_HARDENED_ENABLED can still force off.
	c.KeelHardenedEnabled = parseBoolEnvDefaultTrue("LENS_KEEL_HARDENED_ENABLED")
	c.KeelMoneyCohortFloor = 10
	if v := os.Getenv("LENS_KEEL_MONEY_COHORT_FLOOR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.KeelMoneyCohortFloor = n
		}
	}
	c.KeelMinSamples = 30
	if v := os.Getenv("LENS_KEEL_MIN_SAMPLES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.KeelMinSamples = n
		}
	}
	c.KeelPersistenceWindows = 3
	if v := os.Getenv("LENS_KEEL_PERSISTENCE_WINDOWS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.KeelPersistenceWindows = n
		}
	}
	c.KeelHardenedSigma = 3.5
	if v := os.Getenv("LENS_KEEL_HARDENED_SIGMA"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			c.KeelHardenedSigma = f
		}
	}

	// LXC agent allocation — DEFAULT-TRUE (owner's call; the capstone ships armed). Off removes a spend
	// guard, so the safe default is on. The flag is a manual off-switch (falls back to plain SpendLXC).
	c.LXCAgentAllocationEnabled = true
	if os.Getenv("LENS_LXC_AGENT_ALLOCATION_ENABLED") != "" {
		c.LXCAgentAllocationEnabled = parseBoolEnv("LENS_LXC_AGENT_ALLOCATION_ENABLED")
	}

	// LXC reservation — DEFAULT-TRUE: the correct billing is the end state. Off = legacy pre-serve estimate.
	c.LXCReservationEnabled = true
	if os.Getenv("LENS_LXC_RESERVATION_ENABLED") != "" {
		c.LXCReservationEnabled = parseBoolEnv("LENS_LXC_RESERVATION_ENABLED")
	}
	// Bounded output allowance for the conservative hold when max_tokens is unset. Sane cap, not catalog max.
	c.LXCReservationMaxOutputTokens = 4096
	if v := os.Getenv("LENS_LXC_RESERVATION_MAX_OUTPUT_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.LXCReservationMaxOutputTokens = n
		}
	}
	c.DetectorSweepInterval = time.Hour
	if v := os.Getenv("LENS_DETECTOR_SWEEP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_DETECTOR_SWEEP_INTERVAL (Go duration > 0, e.g. 1h): %s", v)
		}
		c.DetectorSweepInterval = d
	}
	c.DetectorSweepWindow = 24 * time.Hour
	if v := os.Getenv("LENS_DETECTOR_SWEEP_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_DETECTOR_SWEEP_WINDOW (Go duration > 0, e.g. 24h): %s", v)
		}
		c.DetectorSweepWindow = d
	}

	// Anti-gaming AUTO clawback (Phase 2) — DEFAULT-OFF. Unlike the read-only smoke detector,
	// this one revokes; it ships inert and is turned on deliberately after calibration (Phase 4).
	c.AntiGamingAutoRevokeEnabled = false
	if os.Getenv("LENS_ANTIGAMING_AUTO_REVOKE_ENABLED") != "" {
		c.AntiGamingAutoRevokeEnabled = parseBoolEnv("LENS_ANTIGAMING_AUTO_REVOKE_ENABLED")
	}
	// Scan window default 168h — MUST exceed the 72h holdback so every currently-held row is
	// scanned (a shorter window would let an old held ring slip past the detector to finalize).
	c.PatternConcentrationVelocityMax = 2000 // Phase-4a Item 3 placeholder (per 1h)
	if v := os.Getenv("LENS_PATTERN_CONCENTRATION_VELOCITY_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_PATTERN_CONCENTRATION_VELOCITY_MAX (int >= 0): %s", v)
		}
		c.PatternConcentrationVelocityMax = n
	}
	c.AntiGamingScanWindow = 168 * time.Hour
	if v := os.Getenv("LENS_ANTIGAMING_SCAN_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_ANTIGAMING_SCAN_WINDOW (Go duration > 0, e.g. 168h): %s", v)
		}
		c.AntiGamingScanWindow = d
	}
	c.AntiGamingInterval = time.Minute
	if v := os.Getenv("LENS_ANTIGAMING_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_ANTIGAMING_INTERVAL (Go duration > 0, e.g. 1m): %s", v)
		}
		c.AntiGamingInterval = d
	}
	// Settlement fail-closed — Phase-4a Item 4: DEFAULT TRUE for the closed test. An
	// un-examined held mint HOLDS, never settles (the money guarantee); the accepted
	// trade is that a prolonged detector outage strands legitimate mints (recoverable,
	// deflationary) rather than settling blind — the detector-health signal makes such
	// a stall VISIBLE. Inert when the economy is off (the sweepers/clearers don't run).
	// Env override: LENS_SETTLEMENT_FAIL_CLOSED_ENABLED.
	c.SettlementFailClosedEnabled = true
	if os.Getenv("LENS_SETTLEMENT_FAIL_CLOSED_ENABLED") != "" {
		c.SettlementFailClosedEnabled = parseBoolEnv("LENS_SETTLEMENT_FAIL_CLOSED_ENABLED")
	}

	c.PoolRoyaltyShare = 0.5
	if v := os.Getenv("LENS_POOL_ROYALTY_SHARE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		// math.IsNaN is load-bearing: ParseFloat("NaN") succeeds and NaN
		// compares false to every bound, so the range checks alone would
		// let a balance-poisoning NaN share through. (±Inf is caught by
		// the range checks.)
		if err != nil || math.IsNaN(f) || f < 0 {
			return nil, fmt.Errorf("invalid LENS_POOL_ROYALTY_SHARE (must be in [0,1)): %s", v)
		}
		// s >= 1 is refused because the share is a SECURITY parameter, not a tuning knob.
		//
		// Per-identity workspaces (POST /v1/provision) make Sybil self-dealing possible: one
		// person holds two workspaces and has A's cached answers serve B. The identity guards
		// do not stop it — workspace_owner_links has no writer, and the card-fingerprint half
		// catches only a single operator lazily funding both from one card. What holds is
		// arithmetic: anchor.go clamps a royalty's worth to <= AvoidedCOGSUSD and the mint is
		// funded by the consumer's settled charge, so a self-dealing pair spends C to earn s*C
		// and LOSES (1-s)*C every cycle. At s = 1 that becomes break-even and the only
		// economic control is gone. Refuse at boot rather than rely on a convention.
		if f >= 1 {
			return nil, fmt.Errorf(
				"invalid LENS_POOL_ROYALTY_SHARE (must be < 1): %s — the share is a self-deal control, "+
					"not a tuning knob: a Sybil pair spends C to earn s*C, so it loses (1-s)*C per cycle only "+
					"while s < 1; at s >= 1 that becomes break-even or better and per-identity workspaces lose "+
					"their only economic protection (workspace_owner_links has no writer, and card fingerprints "+
					"catch just the one-card operator)", v)
		}
		c.PoolRoyaltyShare = f
	}
	// r = 0.30 by default; see the field comment for why that number and not another.
	c.PoolConsumerDiscount = 0.30
	if v := os.Getenv("LENS_POOL_CONSUMER_DISCOUNT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		// math.IsNaN is load-bearing for the same reason it is on the share above: ParseFloat("NaN")
		// succeeds and NaN compares false to every bound, so the range checks alone would let a NaN
		// rate reach the charge math — and NaN µLXC ceils to a garbage int64 debit.
		if err != nil || math.IsNaN(f) || f < 0 {
			return nil, fmt.Errorf("invalid LENS_POOL_CONSUMER_DISCOUNT (must be in [0,1)): %s", v)
		}
		// r >= 1 is refused rather than clamped. At r = 1 the consumer is charged nothing, which the
		// funding invariant then turns into a ZERO royalty — the contributor works for free and the
		// pool quietly stops being worth contributing to, with no error anywhere. Above 1 the charge
		// would compute NEGATIVE; the settle floors it at 0, so the damage is contained, but a
		// configuration that only survives because a downstream guard catches it is not configured,
		// it is lucky. Refuse at boot.
		if f >= 1 {
			return nil, fmt.Errorf(
				"invalid LENS_POOL_CONSUMER_DISCOUNT (must be < 1): %s — at r = 1 the consumer is charged "+
					"nothing for a pooled hit, and because the contributor's royalty is funded by the "+
					"consumer's settled charge (the funding invariant), the contributor then earns NOTHING "+
					"too. That is not a generous discount, it is the pool silently ceasing to pay", v)
		}
		c.PoolConsumerDiscount = f
	}
	c.POVISlashFraction = 0.5
	if v := os.Getenv("LENS_POVI_SLASH_FRACTION"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f > 1 {
			return nil, fmt.Errorf("invalid LENS_POVI_SLASH_FRACTION (must be in (0,1]): %s", v)
		}
		c.POVISlashFraction = f
	}
	c.POVIChallengeKey = os.Getenv("LENS_POVI_CHALLENGE_KEY")
	// A SET challenge key must actually parse — base64 of an ed25519 32-byte
	// seed or 64-byte private key. The old path silently replaced an operator-
	// supplied-but-mistyped key with an ephemeral one (loadOrGenChallengeKey's
	// Warn branch), which invalidates node-pinned pubkeys and only surfaces
	// later as honest nodes failing challenges. Mirrors main.go's existing
	// hard failure on an unparsable LENS_JWT_PRIVATE_KEY. Empty stays allowed
	// here (single-node dev keeps the ephemeral+warn path); HA requires it below.
	if c.POVIChallengeKey != "" {
		raw, err := base64.StdEncoding.DecodeString(c.POVIChallengeKey)
		if err != nil || (len(raw) != ed25519.SeedSize && len(raw) != ed25519.PrivateKeySize) {
			return nil, fmt.Errorf("invalid LENS_POVI_CHALLENGE_KEY: must be base64 of an ed25519 32-byte seed or 64-byte private key")
		}
	}
	c.KYASigningKey = os.Getenv("LENS_KYA_SIGNING_KEY")
	if c.KYASigningKey != "" {
		raw, err := base64.StdEncoding.DecodeString(c.KYASigningKey)
		if err != nil || (len(raw) != ed25519.SeedSize && len(raw) != ed25519.PrivateKeySize) {
			return nil, fmt.Errorf("invalid LENS_KYA_SIGNING_KEY: must be base64 of an ed25519 32-byte seed or 64-byte private key")
		}
	}
	c.KYACredentialTTL = 24 * time.Hour
	if v := os.Getenv("LENS_KYA_CREDENTIAL_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_KYA_CREDENTIAL_TTL (must be a positive duration): %s", v)
		}
		c.KYACredentialTTL = d
	}
	c.PublicURL = getEnv("LENS_PUBLIC_URL", "https://lens.talyvor.com")
	if u, err := url.Parse(c.PublicURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("invalid LENS_PUBLIC_URL (must be an absolute http(s) URL): %s", c.PublicURL)
	}
	c.PersonaAPIKey, c.PersonaTemplateID = os.Getenv("LENS_PERSONA_API_KEY"), os.Getenv("LENS_PERSONA_TEMPLATE_ID")
	c.CompaniesHouseAPIKey = os.Getenv("LENS_COMPANIES_HOUSE_API_KEY")
	// B30.6 — the similarity to a listed name at which a payee or payment is held. A malformed value is refused.
	c.ScreeningFuzzyThreshold = 0.9
	if v := os.Getenv("LENS_SCREENING_FUZZY_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || !(f > 0 && f <= 1) {
			return nil, fmt.Errorf("config: LENS_SCREENING_FUZZY_THRESHOLD must be above 0 and at most 1, got %q", v)
		}
		c.ScreeningFuzzyThreshold = f
	}
	// B37.4 — how old the sanctions lists may get before payments hold. A malformed value is refused.
	c.ScreeningMaxAge = 48 * time.Hour
	if v := os.Getenv("LENS_SCREENING_MAX_AGE_HOURS"); v != "" {
		h, err := strconv.Atoi(v)
		if err != nil || h <= 0 {
			return nil, fmt.Errorf("config: LENS_SCREENING_MAX_AGE_HOURS must be a whole number of hours above 0, got %q", v)
		}
		c.ScreeningMaxAge = time.Duration(h) * time.Hour
	}
	// B30.15 — the partner's signing secret for payments in, and how long unmatched money waits. A malformed value is refused.
	c.AccountPartnerWebhookSecret = os.Getenv("LENS_ACCOUNT_PARTNER_WEBHOOK_SECRET")
	c.SuspenseReturnDays = 5
	if v := os.Getenv("LENS_SUSPENSE_RETURN_DAYS"); v != "" {
		d, err := strconv.Atoi(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("config: LENS_SUSPENSE_RETURN_DAYS must be a whole number of business days above 0, got %q", v)
		}
		c.SuspenseReturnDays = d
	}
	// HA fail-closed: with LENS_HA_ENABLED every replica must share STABLE
	// signing keys. Ephemeral per-replica keys "work" at startup and break
	// silently in production: JWTs minted by one instance are rejected by the
	// others, /v1/auth/jwks advertises the wrong key, and a challenge-key
	// mismatch fails honest nodes' pinned pubkeys. Refuse to start instead of
	// Warn — the single-node dev path (HA off) is unchanged.
	if c.HAEnabled {
		if c.JWTPrivateKey == "" {
			return nil, fmt.Errorf("LENS_HA_ENABLED requires LENS_JWT_PRIVATE_KEY: an ephemeral per-replica JWT key makes tokens minted by one instance unverifiable by the others")
		}
		if c.POVIChallengeKey == "" {
			return nil, fmt.Errorf("LENS_HA_ENABLED requires LENS_POVI_CHALLENGE_KEY: an ephemeral per-replica challenge key invalidates node-pinned pubkeys")
		}
	}
	switch env := os.Getenv("LENS_ENV"); env {
	case "", "development":
	case "production":
		c.Production = true
		for _, k := range []struct{ name, val string }{
			{"LENS_JWT_PRIVATE_KEY", c.JWTPrivateKey},
			{"LENS_POVI_CHALLENGE_KEY", c.POVIChallengeKey},
			{"LENS_KYA_SIGNING_KEY", c.KYASigningKey},
		} {
			if k.val == "" {
				return nil, fmt.Errorf("LENS_ENV=production requires %s: without it Lens signs with a key made at this start, and what it signed stops verifying at the next", k.name)
			}
		}
	default:
		return nil, fmt.Errorf("config: LENS_ENV must be empty, development or production, got %q", env)
	}
	// U6 Sybil floor: the legacy trust-mint defaults FALSE — an unprotected mint
	// path (no receipt, caller-asserted tokens, no idempotency) must be opt-IN.
	c.TrustfulComputeMintEnabled = false
	if v := os.Getenv("LENS_TRUSTFUL_COMPUTE_MINT_ENABLED"); v != "" {
		c.TrustfulComputeMintEnabled = parseBoolEnv("LENS_TRUSTFUL_COMPUTE_MINT_ENABLED")
	}

	if v := os.Getenv("LENS_RETRY_MAX_ATTEMPTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid LENS_RETRY_MAX_ATTEMPTS (must be ≥ 1): %s", v)
		}
		c.RetryMaxAttempts = n
	}
	if v := os.Getenv("LENS_RETRY_INITIAL_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_RETRY_INITIAL_DELAY: %w", err)
		}
		c.RetryInitialDelay = d
	}
	if v := os.Getenv("LENS_RETRY_MAX_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_RETRY_MAX_DELAY: %w", err)
		}
		c.RetryMaxDelay = d
	}
	if v := os.Getenv("LENS_CB_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid LENS_CB_THRESHOLD: %s", v)
		}
		c.CBThreshold = n
	}
	if v := os.Getenv("LENS_CB_RESET_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_CB_RESET_TIMEOUT: %w", err)
		}
		c.CBResetTimeout = d
	}

	if v := os.Getenv("LENS_SEMANTIC_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_SEMANTIC_THRESHOLD: %w", err)
		}
		c.SemanticThreshold = f
	}

	if v := os.Getenv("LENS_MAX_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_MAX_CACHE_TTL: %w", err)
		}
		c.MaxCacheTTL = d
	}

	// Semantic-cache retention sweeper. Retention <= 0 disables the sweeper
	// (rows kept indefinitely) and is the default (B21.2); the sweep interval
	// must be > 0 because a time.Ticker panics on a non-positive duration.
	c.SemanticCacheRetention = 0
	if v := os.Getenv("LENS_SEMANTIC_CACHE_RETENTION"); v != "" {
		d, err := time.ParseDuration(v) // Go duration units (h/m/s); e.g. 504h = 21 days
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_SEMANTIC_CACHE_RETENTION (Go duration, e.g. 504h for 21 days): %w", err)
		}
		c.SemanticCacheRetention = d
	}
	c.SemanticCacheSweepInterval = time.Hour

	// U14 audit retention — token_events sweeper. <= 0 / unset disables (default).
	if v := os.Getenv("LENS_AUDIT_RETENTION"); v != "" {
		d, err := time.ParseDuration(v) // Go duration units (h/m/s); e.g. 8760h ≈ 1 year
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_AUDIT_RETENTION (Go duration, e.g. 8760h for ~1 year): %w", err)
		}
		c.AuditRetention = d
	}
	c.AuditRetentionDatabaseURL = os.Getenv("LENS_AUDIT_RETENTION_DATABASE_URL")
	// U14 off-box export loop — empty URL disables (default). Interval default 1h.
	c.AuditExportURL = os.Getenv("LENS_AUDIT_EXPORT_URL")
	c.AuditExportInterval = time.Hour
	if v := os.Getenv("LENS_AUDIT_EXPORT_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_AUDIT_EXPORT_INTERVAL (Go duration): %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("LENS_AUDIT_EXPORT_INTERVAL must be > 0 (a time.Ticker panics on non-positive): %s", v)
		}
		c.AuditExportInterval = d
	}
	// Catalog-drift poll cadence — default 1h, MUST be > 0 (a time.Ticker panics otherwise). Hourly is
	// deliberate: a new model appears once in weeks, so there is nothing to gain from polling a
	// provider's control plane harder with the same key that serves real traffic.
	c.ModelWatchInterval = time.Hour
	if v := os.Getenv("LENS_MODEL_WATCH_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_MODEL_WATCH_INTERVAL (Go duration, e.g. 1h): %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("LENS_MODEL_WATCH_INTERVAL must be > 0 (a time.Ticker panics on non-positive): %s", v)
		}
		c.ModelWatchInterval = d
	}
	// U14 #187 export-before-prune gate. Default false = age-only (today's behaviour);
	// on = prune only at/below the export watermark, skip the sweep if export is off.
	c.AuditRequireExportBeforePrune = parseBoolEnv("LENS_AUDIT_REQUIRE_EXPORT_BEFORE_PRUNE")
	// U7b workspace-config reload interval — default 30s, MUST be > 0.
	c.WorkspaceReloadInterval = 30 * time.Second
	if v := os.Getenv("LENS_WORKSPACE_RELOAD_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_WORKSPACE_RELOAD_INTERVAL (Go duration, e.g. 30s): %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("LENS_WORKSPACE_RELOAD_INTERVAL must be > 0 (a time.Ticker panics on non-positive): %s", v)
		}
		c.WorkspaceReloadInterval = d
	}
	// Workspace fail-closed staleness bound — default 3× the reload interval, MUST
	// exceed it (a bound <= the interval would fail closed during normal operation).
	c.WorkspaceMaxStaleness = 3 * c.WorkspaceReloadInterval
	if v := os.Getenv("LENS_WORKSPACE_MAX_STALENESS"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_WORKSPACE_MAX_STALENESS (Go duration, e.g. 90s): %w", err)
		}
		c.WorkspaceMaxStaleness = d
	}
	if c.WorkspaceMaxStaleness <= c.WorkspaceReloadInterval {
		return nil, fmt.Errorf("LENS_WORKSPACE_MAX_STALENESS (%s) must be > LENS_WORKSPACE_RELOAD_INTERVAL (%s): a bound <= the reload interval trips during normal operation", c.WorkspaceMaxStaleness, c.WorkspaceReloadInterval)
	}
	// #189 guardrails-policy reload interval — default 30s, MUST be > 0.
	c.GuardrailsReloadInterval = 30 * time.Second
	if v := os.Getenv("LENS_GUARDRAILS_RELOAD_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_GUARDRAILS_RELOAD_INTERVAL (Go duration, e.g. 30s): %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("LENS_GUARDRAILS_RELOAD_INTERVAL must be > 0 (a time.Ticker panics on non-positive): %s", v)
		}
		c.GuardrailsReloadInterval = d
	}
	if v := os.Getenv("LENS_SEMANTIC_CACHE_SWEEP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_SEMANTIC_CACHE_SWEEP_INTERVAL (Go duration, e.g. 1h): %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("invalid LENS_SEMANTIC_CACHE_SWEEP_INTERVAL (must be > 0): %s", v)
		}
		c.SemanticCacheSweepInterval = d
	}

	var missing []string
	if c.RedisURL == "" {
		missing = append(missing, "LENS_REDIS_URL")
	}
	if c.DatabaseURL == "" {
		missing = append(missing, "LENS_DATABASE_URL")
	}
	if c.NatsURL == "" {
		missing = append(missing, "LENS_NATS_URL")
	}
	if c.OpenAIAPIKey == "" {
		missing = append(missing, "LENS_OPENAI_API_KEY")
	}
	if c.AnthropicAPIKey == "" {
		missing = append(missing, "LENS_ANTHROPIC_API_KEY")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %v", ErrMissingEnv, missing)
	}

	// U3 master economy kill-switch. Default TRUE (explicit opt-out, mirroring the
	// TrustfulCompute default-true-then-override pattern above). When false, force
	// OFF every economy state-creation gate — regardless of its individual env —
	// so no mint/earn/pool/LXC path can fire. The route SURFACE is unregistered
	// separately in main.go (registerEconomyRoutes). Adding a NEW economy gate?
	// add it to this block AND to the manifest in cmd/lens/economy_killswitch_test.go.
	// Default OFF — see the field comment. An unbilled lane that defaults on leaves every
	// deployment exactly where it started.
	c.BatchEnabled = false
	if os.Getenv("LENS_BATCH_ENABLED") != "" {
		c.BatchEnabled = parseBoolEnv("LENS_BATCH_ENABLED")
	}

	// B32.21 — the most trial uses an offer may give. A malformed value is refused.
	c.MarketTrialMax = 5
	if v := os.Getenv("LENS_MARKET_TRIAL_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("config: LENS_MARKET_TRIAL_MAX must be a non-negative integer, got %q", v)
		}
		c.MarketTrialMax = n
	}

	// B32.24 — the remix share's ceiling and the lineage read's depth; B32.26 — the royalties' cap. A malformed value
	// is refused.
	c.LineageMaxShareBPS = 3000
	if v := os.Getenv("LENS_LINEAGE_MAX_SHARE_BPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10000 {
			return nil, fmt.Errorf("config: LENS_LINEAGE_MAX_SHARE_BPS must be 1 to 10000, got %q", v)
		}
		c.LineageMaxShareBPS = n
	}
	c.LineageMaxDepth = 5
	if v := os.Getenv("LENS_LINEAGE_MAX_DEPTH"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("config: LENS_LINEAGE_MAX_DEPTH must be a positive integer, got %q", v)
		}
		c.LineageMaxDepth = n
	}
	c.LineageTotalCapBPS = 5000
	if v := os.Getenv("LENS_LINEAGE_TOTAL_CAP_BPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 10000 {
			return nil, fmt.Errorf("config: LENS_LINEAGE_TOTAL_CAP_BPS must be 0 to 10000, got %q", v)
		}
		c.LineageTotalCapBPS = n
	}
	// B32.46 — the similarity at which a publish is held as an undeclared copy. A malformed value is refused.
	c.MarketSimilarityHold = 0.92
	if v := os.Getenv("LENS_MARKET_SIMILARITY_HOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || !(f > 0 && f <= 1) {
			return nil, fmt.Errorf("config: LENS_MARKET_SIMILARITY_HOLD must be above 0 and at most 1, got %q", v)
		}
		c.MarketSimilarityHold = f
	}
	// B32.47 — the days a seller has to counter an IP claim. A malformed value is refused.
	c.IPCounterDays = 10
	if v := os.Getenv("LENS_IP_COUNTER_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("config: LENS_IP_COUNTER_DAYS must be a positive integer, got %q", v)
		}
		c.IPCounterDays = n
	}
	// B32.41 — the days between the requests for a seller's tax details. A malformed value is refused.
	c.SellerTaxReminderDays = 30
	if v := os.Getenv("LENS_SELLER_TAX_REMINDER_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("config: LENS_SELLER_TAX_REMINDER_DAYS must be a positive integer, got %q", v)
		}
		c.SellerTaxReminderDays = n
	}
	// B32.42 — the day of the week sellers are paid on. A malformed value is refused.
	c.MarketPayoutWeekday = time.Monday
	if v := os.Getenv("LENS_MARKET_PAYOUT_WEEKDAY"); v != "" {
		d, ok := map[string]time.Weekday{"MON": time.Monday, "TUE": time.Tuesday, "WED": time.Wednesday, "THU": time.Thursday,
			"FRI": time.Friday, "SAT": time.Saturday, "SUN": time.Sunday}[strings.ToUpper(strings.TrimSpace(v))]
		if !ok {
			return nil, fmt.Errorf("config: LENS_MARKET_PAYOUT_WEEKDAY must be MON, TUE, WED, THU, FRI, SAT or SUN, got %q", v)
		}
		c.MarketPayoutWeekday = d
	}
	// B32.30 — a member's messages a minute in a room. A malformed value is refused.
	c.RoomMessagesPerMinute = 20
	if v := os.Getenv("LENS_ROOM_MESSAGES_PER_MINUTE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("config: LENS_ROOM_MESSAGES_PER_MINUTE must be a positive integer, got %q", v)
		}
		c.RoomMessagesPerMinute = n
	}
	// B32.33 — the room's latest messages its AI reads. A malformed value is refused.
	c.RoomContextMessages = 30
	if v := os.Getenv("LENS_ROOM_CONTEXT_MESSAGES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("config: LENS_ROOM_CONTEXT_MESSAGES must be a positive integer, got %q", v)
		}
		c.RoomContextMessages = n
	}
	// B32.52 — the workspaces whose open reports take a public room off the list. A malformed value is refused.
	c.RoomReportsHide = 3
	if v := os.Getenv("LENS_ROOM_REPORTS_HIDE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("config: LENS_ROOM_REPORTS_HIDE must be a positive integer, got %q", v)
		}
		c.RoomReportsHide = n
	}

	// D — the Model 2 allowance, in µLXC (W4.6.1 step 2). Default 0 = "no allowance
	// priced yet", which leaves the mechanism inert. A malformed value is REFUSED
	// rather than silently defaulted: a typo'd allowance that quietly becomes 0
	// would sell a subscription entitling the buyer to nothing, and the operator
	// would have no signal at all.
	if v := os.Getenv("LENS_SUBSCRIPTION_ALLOWANCE_ULXC"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("config: LENS_SUBSCRIPTION_ALLOWANCE_ULXC must be a non-negative integer (µLXC), got %q", v)
		}
		c.SubscriptionAllowanceULXC = n
	}
	if v := os.Getenv("LENS_STARTER_TEST_MONEY_ULXC"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("config: LENS_STARTER_TEST_MONEY_ULXC must be a non-negative integer (µLXC), got %q", v)
		}
		c.StarterTestMoneyULXC = n
	}

	// W4.6.1 step 4 — session-scoped keys. OFF unless explicitly enabled.
	c.SessionKeysEnabled = parseBoolEnv("LENS_SESSION_KEYS_ENABLED")
	c.SessionKeyTTL = DefaultSessionKeyTTL
	if v := os.Getenv("LENS_SESSION_KEY_TTL"); v != "" {
		d, err := time.ParseDuration(v) // Go duration units (h/m/s); e.g. 45m
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("config: LENS_SESSION_KEY_TTL must be a positive Go duration (e.g. 45m), got %q", v)
		}
		if d > MaxSessionKeyTTL {
			// REFUSED, not clamped — see the field comment. An operator who reads back 30 days and
			// gets 12 hours has a config file that lies about the process.
			return nil, fmt.Errorf("config: LENS_SESSION_KEY_TTL %v exceeds the %v ceiling; a session key is scoped to a sign-in, not to a month", d, MaxSessionKeyTTL)
		}
		c.SessionKeyTTL = d
	}

	c.EconomyEnabled = true
	if os.Getenv("LENS_ECONOMY_ENABLED") != "" {
		c.EconomyEnabled = parseBoolEnv("LENS_ECONOMY_ENABLED")
	}

	// Phase-4a Item 5: the THREE live traffic mints ship DEFAULT-ON for the closed
	// test (pool-royalty + distill share PoolRoyaltyMintingEnabled; distill also needs
	// its poolable-serve flag; pool-royalty needs CachePoolableEnabled to produce a
	// pooled hit; pattern needs PatternEarningEnabled). Each is env-overridable and
	// force-off'd below when EconomyEnabled=false, so production (LENS_ECONOMY_ENABLED
	// =false) stays inert until the operator turns the master switch on + vouches a
	// workspace. Node/eval/routing/confidential mints are NOT here — they stay OFF.
	for _, f := range []struct {
		p   *bool
		env string
	}{
		{&c.PoolRoyaltyMintingEnabled, "LENS_POOL_ROYALTY_MINTING_ENABLED"}, // arms pool-royalty AND distill mints
		{&c.CachePoolableEnabled, "LENS_CACHE_POOLABLE_ENABLED"},            // enables the pooled cross-tenant cache serve → pool-royalty
		{&c.DistillPoolableEnabled, "LENS_DISTILL_POOLABLE_ENABLED"},        // enables the cross-tenant distill serve → distill basis
		{&c.PatternEarningEnabled, "LENS_PATTERN_EARNING_ENABLED"},          // arms the pattern held mint
	} {
		*f.p = true
		if os.Getenv(f.env) != "" {
			*f.p = parseBoolEnv(f.env)
		}
	}

	// Shadow mode: default off, env-overridable, and NOT force-off'd below (see the field's note).
	c.ShadowMintsEnabled = parseBoolEnv("LENS_SHADOW_MINTS_ENABLED")

	if !c.EconomyEnabled {
		c.PatternMiningEnabled = false
		c.PatternCaptureEnabled = false
		c.PatternEarningEnabled = false
		c.PoolRoyaltyMintingEnabled = false
		c.POVIMintingEnabled = false
		c.AnnotationMintingEnabled = false   // the annotation mint credits LENS — force-off with the economy master switch
		c.TrustfulComputeMintEnabled = false // U6: now defaults false; still force-off'd if an operator opted in
		c.CachePoolableEnabled = false
		c.DistillPoolableEnabled = false
		c.RoutingIntelligenceEnabled = false
		c.RoutingTierCohortsEnabled = false
		c.EvalContributionMintingEnabled = false  // P-o-I instance 1: the eval-contribution EARNING gate (mints LENS) — force-off with the economy
		c.RoutingPredictionMintingEnabled = false // P-o-I instance 2: the routing-prediction EARNING gate (mints LENS) — force-off with the economy
		c.LatencyMintingEnabled = false           // P-o-I instance 3: the latency-locality EARNING gate (mints LENS) — force-off with the economy
		c.ConfidentialMintingEnabled = false      // P-o-I instance 4: the confidential-compute EARNING gate (mints LENS) — force-off with the economy
		// U18: LXCGatingEnabled / LXCShadowSpendEnabled are DELIBERATELY NOT
		// forced off here — LXC is the fiat-pegged usage credit, not token
		// economy. They keep their own env values so a fiat-SaaS deployment can
		// still meter + gate paid LXC credit with the economy off. (Both still
		// default false, so default enterprise-mode behavior is unchanged.) The
		// LENS→LXC conversion route stays economy-gated in main.go.
	}

	// PoVI minting footgun (fail-closed, mirroring the HA signing-key refusals
	// above). Provisional receipt minting credits SPENDABLE LENS (povi/mint.go
	// Credit) and is made safe ONLY by random challenge-and-slash, whose deterrent
	// is the inequality P(challenge) × slash > gain. LENS_POVI_CHALLENGE_RATE is
	// P(challenge); at 0 the inequality collapses to 0 > gain and every fabricated
	// receipt mints unchallenged — "mint on the receipt alone", the exact state
	// PoVI exists to prevent, reachable by MISCONFIGURATION rather than any code
	// defect. Refuse to boot.
	//
	// Keyed on the EFFECTIVE POVIMintingEnabled — this runs AFTER the economy
	// kill-switch force-off — so the guard is strictly additive: the ONLY config
	// that newly fails to boot is effective-minting-on + rate<=0. An economy-off
	// deployment (minting force-disabled) never trips it.
	//
	// We refuse only rate<=0, NOT a positive floor. slash = POVISlashFraction ×
	// stake is bounded by the node's stake, but gain = ReceiptMineRate ×
	// attacker-chosen OutputTokens is unbounded, so the safe minimum rate
	// (gain/slash) is a function of three other configurable knobs (POVIMinStake,
	// POVISlashFraction, and a per-receipt mint cap that does not yet exist) plus
	// an attacker-controlled input — an economics decision, not a code constant.
	// (A negative rate is already rejected by the [0,1] parse above; <=0 restates
	// the invariant so it survives that bound being loosened.)
	if c.POVIMintingEnabled && c.POVIChallengeRate <= 0 {
		return nil, fmt.Errorf("LENS_POVI_MINTING_ENABLED=true requires LENS_POVI_CHALLENGE_RATE > 0: provisional receipt minting credits spendable LENS and is deterred ONLY by random challenge-and-slash (P(challenge) × slash > gain); a zero rate sets P(challenge)=0, removing the only deterrent so a staked node can mint LENS against fabricated, never-challenged traces")
	}

	// Provider-secret KEK — OPTIONAL, but if provided every listed key must decode to exactly 32
	// bytes and no key may repeat. Fail-LOUD at boot on a misconfigured ring, never a broken-crypto
	// surprise at first use; absent ⇒ ProviderSecretKeyring stays nil ⇒ custody is disabled with no
	// plaintext fallback. Mirrors talyvor-track's TRACK_INTEGRATION_ENCRYPTION_KEY posture, which is
	// the half of that pointer W6.10's premise audit found worth copying.
	if v := os.Getenv("LENS_PROVIDER_SECRET_KEK"); v != "" {
		ring, err := envelope.ParseKeyring(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_PROVIDER_SECRET_KEK (comma-separated base64 keys, primary first, each exactly %d bytes — `openssl rand -base64 %d`): %w", envelope.KEKLen, envelope.KEKLen, err)
		}
		c.ProviderSecretKeyring = ring
	}

	return c, nil
}

// ProviderSecretsEnabled reports whether Lens may take custody of a customer's provider credential.
// It is false unless LENS_PROVIDER_SECRET_KEK is set and valid — W6.10's decision is that BYOK is
// meaningless without the envelope, so this is the single gate every custody path must consult.
func (c *Config) ProviderSecretsEnabled() bool { return c.ProviderSecretKeyring != nil }

var ErrMissingEnv = errors.New("missing required environment variables")

// ValidateDBSSLMode returns an error if mode is not one of the six values
// accepted by libpq / pgx for the sslmode parameter.  It is exported so that
// code paths that bypass Load (e.g. the migrate sub-command) can apply the
// same validation and produce the same clear error message.
func ValidateDBSSLMode(mode string) error {
	switch mode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
		return nil
	default:
		return fmt.Errorf(
			"invalid LENS_DB_SSL_MODE %q: must be one of disable, allow, prefer, require, verify-ca, verify-full",
			mode,
		)
	}
}

// defaultDistillWorkerBin resolves the distill-worker path to a binary named
// "distill-worker" sitting beside the running executable (e.g. /distill-worker
// next to /lens in the Docker image). Falls back to a bare "distill-worker"
// (PATH lookup) if the executable path can't be determined.
func defaultDistillWorkerBin() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "distill-worker")
	}
	return "distill-worker"
}

// DefaultEmbeddingModel is the embedder Lens ships with. Exported because the cross-tenant pooling
// safety margin is a property of the CONFIGURED embedder, so cmd/lens warns at boot when this has
// been changed while pooling is on — and that comparison must use the same value as the default
// here, not a copy that can drift away from it.
const DefaultEmbeddingModel = "text-embedding-3-small"

// DefaultSemanticThreshold is the cosine similarity a pooled cache entry must reach before it may
// be served. Exported for the same reason as DefaultEmbeddingModel: it was stated in THREE places
// — this default, `.env.example` (the file operators copy), and a bare literal in cmd/conscheck
// whose comment cited `config.DefaultSemanticThreshold`, a symbol that did not exist. Three copies
// of one decision with nothing between them, so the checker could keep measuring a threshold
// production no longer ran.
//
// ⚠ CHANGING THIS IS NOT A TUNING DECISION. It is bounded by the pool-safety attestation
// (poolsafety.Attestation.MatchesLive), which refuses to serve when the live threshold is LOWER
// than the value poolcheck measured. Raising it is the conservative direction and needs no
// re-attestation; lowering it below the attested value turns pooling off until poolcheck is re-run.
const DefaultSemanticThreshold = 0.98

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseBoolEnv recognises the common "true" forms (1, true, yes,
// on) case-insensitively. Anything else (including empty) is
// false so the feature stays opt-in by default.
// parsePlans reads "name=price_id,name=price_id". Blank entries and entries without both halves are skipped.
func parsePlans(v string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(v, ",") {
		name, price, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.TrimSpace(name) != "" && strings.TrimSpace(price) != "" {
			out[strings.TrimSpace(name)] = strings.TrimSpace(price)
		}
	}
	return out
}

func parseBoolEnv(key string) bool {
	v := os.Getenv(key)
	if v == "" {
		return false
	}
	switch v {
	case "1", "true", "TRUE", "True", "yes", "YES", "Yes", "on", "ON", "On":
		return true
	}
	return false
}

// parseBoolEnvDefaultTrue defaults TRUE; only an explicit falsey value disables. For descriptive,
// money-decoupled captures that are on in closed-test unless deliberately turned off.
func parseBoolEnvDefaultTrue(key string) bool {
	switch os.Getenv(key) {
	case "0", "false", "FALSE", "False", "no", "NO", "No", "off", "OFF", "Off":
		return false
	}
	return true
}
