package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	capabilityterms "github.com/talyvor/lens/docs/terms"
	"github.com/talyvor/lens/internal/a2acard"
	"github.com/talyvor/lens/internal/ab"
	"github.com/talyvor/lens/internal/agentcard"
	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/anomaly"
	"github.com/talyvor/lens/internal/ap2"
	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/attest"
	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/audit"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/backpressure"
	"github.com/talyvor/lens/internal/batch"
	"github.com/talyvor/lens/internal/benchprobe"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/budget"
	"github.com/talyvor/lens/internal/budgets"
	"github.com/talyvor/lens/internal/buildverify"
	"github.com/talyvor/lens/internal/byok"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/coderun"
	"github.com/talyvor/lens/internal/cohort"
	"github.com/talyvor/lens/internal/compat"
	"github.com/talyvor/lens/internal/compliance"
	"github.com/talyvor/lens/internal/compressmeasure"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/controlplane"
	"github.com/talyvor/lens/internal/costanomaly"
	"github.com/talyvor/lens/internal/dashboard"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/dbrouting"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/distill"
	"github.com/talyvor/lens/internal/distillattrib"
	"github.com/talyvor/lens/internal/distillpreview"
	"github.com/talyvor/lens/internal/documents"
	"github.com/talyvor/lens/internal/earnings"
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/econflags"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/eval"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/forecast"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/haircutobs"
	"github.com/talyvor/lens/internal/inference"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/keel"
	"github.com/talyvor/lens/internal/keypool"
	"github.com/talyvor/lens/internal/kya"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/mcp"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/modality"
	"github.com/talyvor/lens/internal/modelcapability"
	"github.com/talyvor/lens/internal/modelwatch"
	"github.com/talyvor/lens/internal/moderatorkey"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/monitoring"
	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/nodelatency"
	"github.com/talyvor/lens/internal/operatoraudit"
	"github.com/talyvor/lens/internal/opsusage"
	"github.com/talyvor/lens/internal/oracle"
	"github.com/talyvor/lens/internal/outputverify"
	"github.com/talyvor/lens/internal/pairverify"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/plans"
	"github.com/talyvor/lens/internal/platformreport"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/poolshadow"
	"github.com/talyvor/lens/internal/povi"
	"github.com/talyvor/lens/internal/prompts"
	"github.com/talyvor/lens/internal/provenance"
	"github.com/talyvor/lens/internal/proxy"
	"github.com/talyvor/lens/internal/quality"
	"github.com/talyvor/lens/internal/ratelimit"
	"github.com/talyvor/lens/internal/reqtrack"
	"github.com/talyvor/lens/internal/retry"
	"github.com/talyvor/lens/internal/roi"
	"github.com/talyvor/lens/internal/rooms"
	"github.com/talyvor/lens/internal/routedecision"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/routing"
	"github.com/talyvor/lens/internal/routingbrain"
	"github.com/talyvor/lens/internal/routingpredict"
	"github.com/talyvor/lens/internal/routingscore"
	"github.com/talyvor/lens/internal/royaltyhaircut"
	"github.com/talyvor/lens/internal/safehttp"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/sellertax"
	"github.com/talyvor/lens/internal/session"
	"github.com/talyvor/lens/internal/sessionkey"
	"github.com/talyvor/lens/internal/shadowmint"
	"github.com/talyvor/lens/internal/status"
	"github.com/talyvor/lens/internal/stepup"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/tare"
	"github.com/talyvor/lens/internal/tare/kompress"
	"github.com/talyvor/lens/internal/taxprofile"
	"github.com/talyvor/lens/internal/templates"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/webpush"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/internal/worktier"
	"github.com/talyvor/lens/migrations"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	// Subcommand dispatch. Only an explicit "migrate" or "version" diverts; with
	// no subcommand (or any other arg, as before) the process starts the gateway
	// server exactly as it always has — the default entrypoint is unchanged.
	// `lens version` prints the build stamp and exits. This is the operational
	// point of injecting it: a deployment can be identified by asking the binary
	// what it is — `docker exec … lens version` — rather than trusting the tag
	// that was pulled. CI asserts this output is a real stamp, not "dev".
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(lensVersion)
		return
	}
	// B28.119: a Run code sandbox child (internal/coderun) — the gateway starts one per run, never a person.
	if len(os.Args) > 1 && os.Args[1] == coderun.ChildArg {
		coderun.ChildMain()
		return
	}
	// `lens poolcheck` is a DEPLOY PREFLIGHT for cross-tenant cache pooling. It embeds a
	// corpus of "same fixed preamble, unrelated content" prompts through the CONFIGURED
	// embedder and exits non-zero if any unrelated pair reaches LENS_SEMANTIC_THRESHOLD —
	// i.e. if this configuration could serve one workspace's cached response to another.
	//
	// It is a subcommand rather than a boot check on purpose: it costs real embedding calls,
	// and a gateway restart is the wrong moment to discover a config problem AND pay for it
	// on every replica. Run it in the deploy pipeline, and after ANY change to
	// LENS_EMBEDDING_MODEL, LENS_SEMANTIC_THRESHOLD, or a major client's prompt templates.
	// It is also the only thing standing between "today's model happens to be safe" and
	// "we would notice if it stopped being".
	// `lens deletion-requests` (B21.3): list and complete requests to delete everything Talyvor holds
	// for a workspace. See deletion_requests_cli.go and docs/deletion-requests-runbook.md.
	if len(os.Args) > 1 && os.Args[1] == "deletion-requests" {
		if err := runDeletionRequests(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// `lens moderator-keys` (B20.13): create, list and revoke the marketplace moderator keys.
	if len(os.Args) > 1 && os.Args[1] == "moderator-keys" {
		if err := runModeratorKeys(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// `lens clearances` (B30.1; `lens wallet-clearances`, B22.1): clear an AMBER or RED wallet capability for real
	// money, or revoke it.
	if len(os.Args) > 1 && (os.Args[1] == "clearances" || os.Args[1] == "wallet-clearances") {
		if err := runWalletClearances(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// `lens terms` (B30.9): each capability's terms, and publishing a changed text as the next version.
	if len(os.Args) > 1 && os.Args[1] == "terms" {
		if err := runCapabilityTerms(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// `lens verification-limits` (B30.4): the most one movement of live money may move at each verification level.
	if len(os.Args) > 1 && os.Args[1] == "verification-limits" {
		if err := runVerificationLimits(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// `lens credit-lines` (B22.4): mark company workspaces and set, pause or resume their credit lines.
	if len(os.Args) > 1 && os.Args[1] == "credit-lines" {
		if err := runCreditLines(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// `lens escrows` (B22.6): the disputed escrows, and the operator's decision on each.
	if len(os.Args) > 1 && os.Args[1] == "escrows" {
		if err := runEscrows(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// `lens market journal-check` (B32.17): each seller's marketplace journal reconciled with their earnings and payouts.
	if len(os.Args) > 1 && os.Args[1] == "market" {
		if err := runMarket(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// `lens tax` (B32.37): load the tax rates and Talyvor's registrations the tax partner decides from.
	if len(os.Args) > 1 && os.Args[1] == "tax" {
		if err := runTax(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// `lens platform-report` (B32.44): the annual platform-reporting export for the UK reporting rules and EU DAC7.
	if len(os.Args) > 1 && os.Args[1] == "platform-report" {
		if err := runPlatformReport(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// `lens synthetic-crossings` (B26.15): the money that crossed between a test workspace and a real one before
	// the wall, and --reverse to give each back.
	if len(os.Args) > 1 && os.Args[1] == "synthetic-crossings" {
		if err := runSyntheticCrossings(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "poolcheck" {
		if err := runPoolCheck(); err != nil {
			slog.Error("poolcheck failed", slog.String("err", err.Error()))
			os.Exit(1)
		}
		return
	}
	// `rephrasecheck` is poolcheck's other half. poolcheck answers "is the threshold high enough
	// that unrelated prompts never collide" — the safety ceiling. This answers "is it low enough
	// that two people asking the same question in different words actually match" — the utility
	// floor. The pool's promise to a consumer is a claim about REPHRASINGS, and a deployment that
	// measures only the ceiling has measured whether the pool is SAFE, not whether it WORKS.
	// Read-only: it prints numbers and writes nothing.
	if len(os.Args) > 1 && os.Args[1] == "rephrasecheck" {
		if err := runRephraseCheck(); err != nil {
			slog.Error("rephrasecheck failed", slog.String("err", err.Error()))
			os.Exit(1)
		}
		return
	}
	// engcheck measures the ENGINEERING corpora — rephrasings, entity-mismatch danger pairs, and
	// the post-gate separation. Read-only.
	if len(os.Args) > 1 && os.Args[1] == "engcheck" {
		if err := runEngCheck(); err != nil {
			slog.Error("engcheck failed", slog.String("err", err.Error()))
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "d2qcheck" {
		if err := runD2QCheck(); err != nil {
			slog.Error("d2qcheck failed", slog.String("err", err.Error()))
			os.Exit(1)
		}
		return
	}
	// canoncheck measures W2.6's tier-2 proposal: rewrite each prompt into a canonical question
	// with a cheap model, hash THAT, match exactly — no threshold anywhere in the path. It reports
	// key stability FIRST (an unstable key makes every later number a fact about one sample), then
	// the collapse rate on rephrasings and on danger pairs. Read-only; it is not on any serve path
	// and internal/canonq's no_serve_path_test.go asserts that.
	if len(os.Args) > 1 && os.Args[1] == "canoncheck" {
		if err := runCanonCheck(); err != nil {
			slog.Error("canoncheck failed", slog.String("err", err.Error()))
			os.Exit(1)
		}
		return
	}
	// realdist measures the nearest-neighbour similarity of REAL stored prompts. Read-only; reads
	// no prompt content (there is none stored).
	if len(os.Args) > 1 && os.Args[1] == "realdist" {
		if err := runRealDist(); err != nil {
			slog.Error("realdist failed", slog.String("err", err.Error()))
			os.Exit(1)
		}
		return
	}
	// `lens vapid-key` prints a new VAPID key pair for LENS_VAPID_PRIVATE_KEY (B19.16's approval pushes).
	if len(os.Args) > 1 && os.Args[1] == "vapid-key" {
		private, public, err := webpush.GenerateKey()
		if err != nil {
			slog.Error("vapid-key failed", slog.String("err", err.Error()))
			os.Exit(1)
		}
		fmt.Printf("LENS_VAPID_PRIVATE_KEY=%s\n# public key (browsers subscribe with it; Lens serves it too): %s\n", private, public)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := runMigrate(); err != nil {
			slog.Error("migrate failed", slog.String("err", err.Error()))
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("startup failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

// runMigrate applies the embedded SQL migrations and exits. It reads the SAME
// LENS_DATABASE_URL the server uses, but deliberately does NOT call
// config.Load() — that validates Redis/NATS/provider keys a migration Job has
// no business needing. Idempotent (already-applied versions are skipped) and
// fail-loud (a migration error returns non-nil → os.Exit(1) so the K8s Job
// fails and the rollout is held back from an unmigrated DB).
func runMigrate() error {
	level := os.Getenv("LENS_LOG_LEVEL")
	if level == "" {
		level = "info"
	}
	slog.SetDefault(newLogger(level))

	rawDBURL := os.Getenv("LENS_DATABASE_URL")
	if rawDBURL == "" {
		return dbmigrate.ErrNoDatabaseURL
	}
	// Apply the same sslmode default as the main server so the migration
	// connection is also encrypted. The operator can override by either
	// embedding sslmode= in LENS_DATABASE_URL or by setting LENS_DB_SSL_MODE.
	migrateSSLMode := os.Getenv("LENS_DB_SSL_MODE")
	if migrateSSLMode == "" {
		migrateSSLMode = "require"
	}
	// Validate before injecting — without this, an invalid value like
	// LENS_DB_SSL_MODE=badinput would pass silently and produce a confusing
	// pgx connect error instead of a clear startup failure.
	if err := config.ValidateDBSSLMode(migrateSSLMode); err != nil {
		return err
	}
	dbURL := injectSSLMode(rawDBURL, migrateSSLMode)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("migrate: connect: %w", err)
	}
	defer func() {
		// context.Background() would hang indefinitely if the network is
		// degraded; use a short bounded timeout so the process can exit cleanly.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = conn.Close(closeCtx)
	}()

	applied, err := dbmigrate.Run(ctx, conn, migrations.FS)
	if err != nil {
		return err
	}
	// B39.6: the lens_app / lens_retention roles and their grants, re-derived after every run.
	if err := dbmigrate.ApplyRoles(ctx, conn, os.Getenv("LENS_APP_DB_PASSWORD"), os.Getenv("LENS_RETENTION_DB_PASSWORD")); err != nil {
		return err
	}
	if len(applied) == 0 {
		slog.Info("migrate: database already up to date")
	} else {
		slog.Info("migrate: applied migrations",
			slog.Int("count", len(applied)),
			slog.Any("versions", applied))
	}
	return nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := fees.Check(); err != nil {
		return err
	}
	if err := plans.Check(); err != nil {
		return err
	}
	if err := rooms.CheckLimits(); err != nil {
		return err
	}
	enterpriseFromUSDCents, err := billing.EnterpriseFromUSDCents(os.Getenv("LENS_ENTERPRISE_FROM_USD_CENTS"))
	if err != nil {
		return err
	}

	// Boot-time environment checks. Neither blocks startup; both surface a state that no test in
	// this repository can see — see cmd/lens/env_hygiene.go for why each has to be here.
	logEnvironmentHygiene()
	warnEmbeddingModelAffectsPoolingMargin(cfg.EmbeddingModel, config.DefaultEmbeddingModel, cfg.CachePoolableEnabled)

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tp, err := initTracing(ctx)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(shutdownCtx); err != nil {
			logger.Warn("tracer shutdown", slog.String("err", err.Error()))
		}
	}()

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return err
	}
	// redisTLSActive is true when either:
	//   (a) the URL uses rediss:// — go-redis already set TLSConfig, or
	//   (b) LENS_REDIS_TLS=true — operator forces TLS on a redis:// URL.
	redisTLSActive := redisOpts.TLSConfig != nil || cfg.RedisTLS
	if redisTLSActive {
		applyRedisTLS(redisOpts, cfg.RedisTLSSkipVerify)
		logger.Info("redis TLS enabled", slog.Bool("skip_verify", cfg.RedisTLSSkipVerify))
		if cfg.RedisTLSSkipVerify {
			logger.Warn("redis TLS certificate verification is disabled (LENS_REDIS_TLS_SKIP_VERIFY=true)" +
				" — only appropriate for self-signed certs in controlled environments")
		}
	} else {
		logger.Warn("redis TLS is disabled — connection is unencrypted;" +
			" use a rediss:// URL or set LENS_REDIS_TLS=true for production")
	}
	redisClient := redis.NewClient(redisOpts)
	defer func() { _ = redisClient.Close() }()
	// B27.11: a request in a Redis command reads "redis" on /healthz and in the slow-request log.
	redisClient.AddHook(reqtrack.RedisHook{})

	if err := redisClient.Ping(ctx).Err(); err != nil {
		logger.Warn("redis ping failed", slog.String("err", err.Error()))
	}

	// Inject sslmode before parsing so every pool connection is encrypted.
	// injectSSLMode is a no-op when the operator already set sslmode= in the URL.
	dbURL := injectSSLMode(cfg.DatabaseURL, cfg.DBSSLMode)
	if cfg.DBSSLMode == "disable" {
		logger.Warn("postgres TLS is disabled — connection is unencrypted; set LENS_DB_SSL_MODE=require for production")
	} else {
		logger.Info("postgres TLS", slog.String("sslmode", cfg.DBSSLMode))
	}

	poolCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return fmt.Errorf("pgxpool parse config: %w", err)
	}
	poolCfg.MaxConns = cfg.DBMaxConns
	poolCfg.MinConns = cfg.DBMinConns
	// B27.11: a request waiting for a pool connection reads "db_acquire", one in a query "db_query".
	poolCfg.ConnConfig.Tracer = reqtrack.PgxTracer{}
	if cfg.DBPgBouncer {
		// PgBouncer in transaction mode doesn't support the extended query
		// protocol (prepared statements). Simple protocol is compatible with
		// all pooling modes and has negligible overhead for OLTP queries.
		poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("pgxpool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		logger.Warn("postgres ping failed", slog.String("err", err.Error()))
	}

	// U8/U9 read-replica: an OPTIONAL second pool for analytics/display reads.
	// It stays nil unless LENS_DB_REPLICA_URL is set AND the replica pings OK at
	// boot (OpenReplica degrades every failure mode to nil + a WARN — never a
	// crash). Money/authz/transaction reads NEVER receive this handle; only the
	// recon-confirmed analytics readers are wired through dbrouting.ReadPool.
	replicaPool := dbrouting.OpenReplica(ctx, cfg.DBReplicaURL, dbrouting.ReplicaOpts{
		MaxConns: cfg.DBMaxConns, MinConns: cfg.DBMinConns, PgBouncer: cfg.DBPgBouncer, Log: logger,
	})
	if replicaPool != nil {
		defer replicaPool.Close()
	}
	// U8/U9 lag observability: sample replica replay lag into the Prometheus
	// gauge + surface it on /healthz. A no-op when replicaPool is nil (gauge
	// stays 0, health entry reports a plain healthy).
	replicaLagMonitor := dbrouting.NewLagMonitor(replicaPool, metrics.SetReplicaLagSeconds, 0, logger)
	replicaLagMonitor.Start(ctx)

	nc, err := connectNATS(cfg.NatsURL, cfg, logger)
	if err != nil {
		return err
	}
	defer nc.Close()

	exactCache := cache.NewExactCache(redisClient, cfg.MaxCacheTTL)
	openAIEmbedder := embedder.NewOpenAIEmbedder(cfg.OpenAIAPIKey, cfg.EmbeddingModel, cfg.EmbeddingBaseURL)
	// Bound to the EMBEDDER that produces its vectors (migrations/0110). Rows are stamped
	// with it on write and only compared against rows from the same embedder on read, so a
	// change to LENS_EMBEDDING_MODEL can no longer make vectors from two different spaces
	// look comparable.
	semanticCache := cache.NewSemanticCache(pool, openAIEmbedder, cfg.SemanticThreshold, cfg.SemanticCacheRetention)
	// B9.7: the pair verifier (docs/pool-b92-measured.md, 0/86 danger pairs) gates every pooled
	// semantic serve, and is the only gate for a question that names no entity. Without an
	// Anthropic key the pooled read stays exactly as before: entity lane only.
	if cfg.AnthropicAPIKey != "" {
		semanticCache.SetPairVerifier(pairverify.NewAnthropicVerifier(cfg.AnthropicAPIKey, ""))
		slog.Info("B9.7 pair verifier: ACTIVE — every pooled semantic serve is verified (LENS_ANTHROPIC_API_KEY is set)")
	} else {
		slog.Warn("B9.7 pair verifier: INACTIVE — LENS_ANTHROPIC_API_KEY is not set, so pooled semantic reads use the entity lane only")
	}
	promptCompressor := compressor.New()
	modelRouter := router.New()
	piiDetector := pii.New()
	alertManager := alerts.New(pool, nc, nil) // rules loaded from DB in a future iteration
	// SEC-7: outbound Track spend-alert emitter (async, fail-open, never blocks a
	// serve). Disabled no-op when the URL/secret are unset (the default).
	alertManager.SetEmitter(alerts.NewEmitter(cfg.TrackWebhookURL, cfg.TrackWebhookSecret))
	alertManager.StartMonitor(ctx)
	templateDetector := templates.New(pool)
	qualityScorer := quality.New(pool)
	abEngine := ab.NewEngine(pool)
	abEngine.RunAutoCompleteLoop(ctx, time.Hour)
	branchTracker := attribution.New() // header attribution only; branch_spend writes retired (#157)
	// One shared bound across all post-serve observational writers
	// (attribution + pattern capture), so their combined claim on the DB
	// pool is capped. Drop-on-overflow by design: under overload these
	// writers otherwise churn pool connections until PgBouncer's
	// max_client_conn is exhausted (#122). nil when set to 0 (bound off).
	obsLimiter := backpressure.New(cfg.ObsWriteMaxConcurrent)

	attrStore := attribution.NewStore(pool)
	attrStore.SetWriteLimiter(obsLimiter)
	budgetStore := budgets.NewStore(pool)
	budgetService := budgets.NewService(budgetStore)
	// Predictive cost forecasting (Upgrade 20). Read-only analytics over
	// token_events + budgets; cached, off the request hot path.
	forecaster := forecast.New(forecast.NewStore(dbrouting.ReadPool(pool, replicaPool)))
	// Cross-sectional cost anomaly detection (Upgrade 21). Read-only,
	// cached, off the hot path. Distinct from the temporal anomaly.Detector.
	costAnomalyStore := costanomaly.NewStore(dbrouting.ReadPool(pool, replicaPool))
	costAnomalyDetector := costanomaly.New(costAnomalyStore)
	// Executive ROI reporting (Upgrade 24). Read-only orchestration of
	// budgets + forecast + costanomaly + attribution; cached, off the hot
	// path. Per-engineer breakdown is opt-in (sensitive — see config).
	roiReporter := roi.New(costAnomalyStore, budgetStore, budgetStore, forecaster, costAnomalyDetector, attrStore, roi.Config{
		IncludeEngineerBreakdown: cfg.ROIIncludeEngineerBreakdown,
	})
	wsManager := workspace.New(pool)
	if err := wsManager.LoadAll(ctx); err != nil {
		logger.Warn("workspace: LoadAll failed", slog.String("err", err.Error()))
	}
	if err := wsManager.RegisterWorkspace(ctx, workspace.Workspace{
		ID: "default", Name: "Default Workspace", Active: true,
	}); err != nil {
		logger.Warn("workspace: default registration failed", slog.String("err", err.Error()))
	}

	// Phase-0 Item E: parse the operator's private-CIDR allowlist for the node
	// call-out SSRF guard. Empty ⇒ nil ⇒ today's all-private-blocked behavior. A bad
	// CIDR or one containing cloud metadata fails LOUD at boot (never a silent hole).
	nodeCIDRAllow, err := safehttp.ParseCIDRAllowlist(cfg.NodePrivateCIDRAllowlist)
	if err != nil {
		slog.Error("node private CIDR allowlist invalid", slog.String("err", err.Error()))
		os.Exit(1)
	}
	if len(nodeCIDRAllow) > 0 {
		logger.Warn("safehttp: node private-CIDR allowlist ACTIVE — these private ranges are dialable",
			slog.String("cidrs", cfg.NodePrivateCIDRAllowlist))
	}

	lr := localrouter.New(cfg.OllamaURL)
	lr.SetHTTPClient(newNodeHTTPClient(cfg.NodeTLSSkipVerify, 5*time.Second, nodeCIDRAllow))

	// Multi-endpoint local router (additive, see internal/localrouter/multi.go).
	// Parses LENS_LOCAL_ENDPOINTS; if empty, the registry stays empty and
	// the admin API can register endpoints dynamically at runtime.
	localRouterMulti := localrouter.NewRouterFromConfig(pool, cfg.LocalEndpoints)
	localRouterMulti.SetHTTPClient(newNodeHTTPClient(cfg.NodeTLSSkipVerify, 5*time.Second, nodeCIDRAllow))
	if cfg.NodeTLSSkipVerify {
		logger.Warn("node TLS certificate verification is disabled (LENS_NODE_TLS_SKIP_VERIFY=true)" +
			" — only appropriate for self-signed certs on controlled private networks")
	}
	localRouterMulti.StartHealthChecks(ctx, localrouter.DefaultHealthCheckInterval)
	injectionDetector := injection.New(injection.DefaultPolicy())
	budgetEnforcer := budget.New(pool, budget.BudgetPolicy{MaxOutputTokens: 4096})
	batchRouter := batch.New(pool, cfg.AnthropicAPIKey)

	// Workspace-scoped distill evidence (see the /distill/usage route). Reads token_events by
	// workspace_id; nil pool leaves it unconfigured and the route answers 503 rather than 0.
	var distillUsageStore api.DistillUsageStore
	if pool != nil {
		distillUsageStore = api.NewDistillUsageStore(pool)
	}

	// W6.1 step 2 — the durable record of what the PROMPT REWRITER actually
	// removed from the wire, and of whether it reached the bill. Wired
	// unconditionally: on a nil pool the write side is a no-op and the read side
	// answers ErrUnwired, which the route turns into 503. Assigned to the
	// concrete *Store rather than to an interface variable on purpose — a
	// typed-nil in an interface is not == nil, and this reader's whole job is to
	// keep "not wired" distinguishable from "measured nothing".
	compressionStore := compressmeasure.NewStore(pool)
	go batchRouter.StartPoller(ctx)
	sessionTracker := session.New(pool)
	go sessionTracker.StartCleanup(ctx)

	promptManager := prompts.New(pool)
	fallbackRouter := fallback.New()
	keyPool := keypool.New()
	auditExporter := audit.New(pool)
	evalPipeline := eval.New(pool, qualityScorer, cfg.OpenAIAPIKey, cfg.AnthropicAPIKey, cfg.GoogleAPIKey)
	// Attribute eval spend through the normal cost ledger (feature/modality
	// "eval") so eval runs show up in budgets/forecasts/reports, and run the
	// scheduler for cadence-based dataset evals (both off the request hot path).
	evalPipeline.SetSpendRecorder(alertManager)
	// eval scheduler and anomaly monitor goroutines are launched after setupHA
	// below so they can be wrapped with leader election.
	anomalyDetector := anomaly.New(dbrouting.ReadPool(pool, replicaPool))
	statusPage := status.New(pool, redisClient, nc, lensVersion)
	go statusPage.StartCacher(ctx, 60*time.Second)
	// Model-catalog runtime overrides (Upgrade 16): an operator can add or
	// reprice models without a rebuild via LENS_MODEL_CATALOG_OVERRIDES (a
	// JSON array of catalog.Model), layered on top of the embedded default.
	applyCatalogOverrides(os.Getenv("LENS_MODEL_CATALOG_OVERRIDES"), logger)

	guardrailsEngine := guardrails.New(piiDetector, injectionDetector)
	// Output-stage guardrails (Upgrade 13) are OFF by default; when off the
	// input guardrails behave exactly as today and no output guardrails run.
	guardrailsEngine.SetOutputEnabled(cfg.GuardrailsEnabled)
	// #189: persist custom per-workspace guardrail policies (guardrail_policies,
	// 0014) so they survive restart and propagate across replicas. SetStore wires
	// the pool; Load seeds the cache at boot (on failure the engine serves
	// defaultPolicy — PII redact + injection block stay ON — and flags Degraded());
	// StartRefresh bounds cross-replica staleness, not leader-gated (each replica
	// refreshes its own cache).
	guardrailsEngine.SetStore(pool)
	if err := guardrailsEngine.Load(ctx); err != nil {
		logger.Warn("guardrails: initial policy load failed (serving defaults, Degraded)", slog.String("err", err.Error()))
	}
	guardrailsEngine.StartRefresh(ctx, cfg.GuardrailsReloadInterval)

	l := learner.New(nc, pool)
	go l.StartBackground(ctx)

	// Semantic-cache retention sweeper: deletes prompt_embeddings rows unused
	// within the retention window (their updated_at is bumped on every hit, so
	// the timer resets on use). Bounds table + ivfflat-index growth. Inert when
	// SemanticCacheRetention <= 0 (StartSweeper logs and returns).
	go semanticCache.StartSweeper(ctx, cfg.SemanticCacheSweepInterval)

	p := proxy.New(exactCache, semanticCache, openAIEmbedder, promptCompressor, modelRouter, piiDetector, alertManager, templateDetector, qualityScorer, branchTracker, wsManager, lr, injectionDetector, budgetEnforcer, batchRouter, sessionTracker, promptManager, fallbackRouter, keyPool, auditExporter, guardrailsEngine, cfg.OpenAIAPIKey, cfg.AnthropicAPIKey, cfg.GoogleAPIKey, l)

	// B27.35 — Tare phase 2a: the kompress-small prose model, for workspaces that opt in
	// (PUT /v1/workspaces/{ws}/tare-model). Loaded on first use, so a lens whose workspaces never opt in
	// never reads the 279 MB of weights. The image carries them at kompress.DefaultDir.
	tareModelDir := os.Getenv("LENS_TARE_MODEL_DIR")
	if tareModelDir == "" {
		tareModelDir = kompress.DefaultDir
	}
	tareModel := kompress.NewCompressor(tareModelDir)
	p.SetTareModel(tareModel.Reduction(nil))
	if _, err := os.Stat(filepath.Join(tareModelDir, kompress.WeightsFile)); err != nil {
		logger.Warn("tare: phase 2a weights not found; opted-in workspaces' prose will go upstream unchanged", "dir", tareModelDir, "error", err)
	}
	// B27.36 — Tare phase 2b's training set: prose from workspaces that opt in (PUT .../tare-training) and from
	// synthetic test workspaces, nothing else.
	p.SetTareTraining(wsManager)
	// CONSUMER DISCOUNT on cross-tenant pooled cache hits (r). Wired HERE, unconditionally, and
	// NOT beside the royalty minter below: a pooled hit CHARGES the consumer whether or not royalty
	// minting is enabled (the mint is skipped, the bill is not), so gating the discount on the mint
	// flag would leave consumers paying full price for pooled hits on any deploy with minting off —
	// the exact complaint this closes, reintroduced through the wiring rather than the code.
	p.SetPoolConsumerDiscount(cfg.PoolConsumerDiscount)
	slog.Info("economy: cross-tenant pooled hits are DISCOUNTED for the consumer",
		slog.Float64("consumer_discount_rate", cfg.PoolConsumerDiscount),
		slog.Float64("contributor_share_of_discounted_charge", cfg.PoolRoyaltyShare),
		slog.String("effect", "a pooled hit bills list×(1−r); the contributor's royalty is a share of "+
			"that DISCOUNTED charge, so the funding invariant holds unchanged"))

	// Upgraded per-request attribution store (Upgrade Batch 1 / Item 3).
	// Wired as a setter so the existing proxy.New signature stays put.
	p.SetAttributionStore(attrStore)
	// DISTILL request-path integration (stage 3, PR #2). The isolated
	// distill-worker subprocess converts an opted-in request's document to
	// Markdown before the model call; a Redis-backed conversion cache avoids
	// re-converting the same document. ON by default: new workspaces get DistillPolicy
	// `always` (workspace.DefaultDistillPolicy, migration 0134); `disabled` turns it off.
	p.SetDistiller(
		&distill.ProcessIsolator{WorkerBin: cfg.DistillWorkerBin},
		cache.NewDistillCache(redisClient, cfg.MaxCacheTTL),
		// Cross-tenant distill-share consent gate (S0). Default private: the
		// distill cache is wsID-scoped unless LENS_DISTILL_POOLABLE_ENABLED is on
		// AND both the owner and requester have distill_poolable=true (PUT
		// /v1/workspaces/{wsID}/distill-poolable). A SEPARATE consent from
		// cache_poolable — distill artifacts are document-derived.
		cache_pooling.New(
			func() bool { return cfg.DistillPoolableEnabled },
			// B17.1: a synthetic workspace never shares a conversion nor is served one.
			func(wsID string) bool { return wsManager.GetDistillPoolable(wsID) && !wsManager.GetSynthetic(wsID) },
		),
		// S1 distill attribution sink (MINT-FREE): records consented cross-tenant
		// pooled-distill serves into distill_serve_attribution (migration 0052).
		// No ledger, no read endpoint yet — write-only. Inert unless a serve is
		// actually consented cross-tenant (requires the distill_poolable flags on).
		distillattrib.NewStore(pool),
	)
	// B18.13 — documents uploaded to POST /v1/documents (up to 25 MB), which a chat request references
	// by id instead of carrying the file in its 4 MiB body; converted on the way to the model.
	docStore := documents.NewStore(pool)
	p.SetDocuments(docStore)
	// B41.101 — invoices uploaded as bills are read through the same isolated distill worker.
	billReader := &distill.ProcessIsolator{WorkerBin: cfg.DistillWorkerBin}
	// Phase-2 Stage 2.0 shared-cache governance gate (exact cache). Read-only:
	// reads the global switch + each workspace's cache_poolable opt-in, mutates
	// nothing. Inert by default — pooling stays off until LENS_CACHE_POOLABLE_ENABLED
	// is set AND a workspace opts in (PUT /v1/workspaces/{wsID}/cache-poolable).
	// BOOT BINDING. Cross-tenant pooling is allowed only when the CURRENTLY CONFIGURED
	// embedding model + threshold are the ones that last PASSED `lens poolcheck`. A change
	// to either invalidates the measurement that justified pooling, and nothing else would
	// notice: the preflight runs in the deploy pipeline, and the urgent cost tweak — edit
	// .env, restart — is precisely the change that skips it.
	//
	// ⚠ IT FORCES POOLING OFF RATHER THAN REFUSING TO BOOT, deliberately.
	// Refusing to boot is the louder action but not the safer one: forcing pooling off
	// already fails to the safe side (nothing is served across tenants), while refusing
	// would additionally take down inference for every tenant because an OPTIONAL cache
	// optimisation could not be verified. That is disproportionate — the gateway's job is
	// proxying and metering, and neither depends on this. The real objection to the quiet
	// option is that it goes unnoticed, and the answer to that is observability, not an
	// outage: the mismatch is logged at ERROR with the specific reason on every boot.
	poolGate := poolSafetyGate(ctx, pool, cfg)
	p.SetPoolGate(cache_pooling.New(
		func() bool { return cfg.CachePoolableEnabled && poolGate.Attested() },
		wsManager.GetCachePoolable,
	))
	// B17.1: synthetic workspaces pool among themselves only, and earn only test money (B25.2, B26.6).
	p.SetSyntheticLookup(wsManager.GetSynthetic)
	// B28.119: Run code — the model may run JavaScript in the sandbox while it answers, when the request asks.
	if runner, err := coderun.NewRunner(); err != nil {
		slog.Warn("run code is off: this binary cannot find itself to start the sandbox", slog.String("err", err.Error()))
	} else {
		p.SetCodeRunner(runner)
	}
	// Per-team / per-sprint budget governance (Upgrade 19). Seed the
	// in-memory snapshot from token_events, refresh it periodically, then
	// wire the gate into the proxy hot path. Load is best-effort — a cold
	// start simply begins with zero budgets until the first refresh.
	if err := budgetService.Load(ctx); err != nil {
		logger.Warn("budgets: initial load failed", slog.String("err", err.Error()))
	}
	budgetService.StartRefresh(ctx)
	// U7b: bound cross-replica staleness of workspace config (logging policy +
	// the cache-pooling privacy flag) — every replica rebuilds its in-memory
	// workspace cache from Postgres on this interval (build-then-swap LoadAll).
	// NOT leader-gated: each replica must refresh its OWN cache. ctx is the run
	// lifecycle context (cancelled on shutdown → the ticker goroutine exits).
	// Past WorkspaceMaxStaleness without a successful reload, the consent/privacy
	// accessors fail closed (pause pooling / clamp logging) until the DB recovers.
	wsManager.SetMaxStaleness(cfg.WorkspaceMaxStaleness)
	wsManager.StartRefresh(ctx, cfg.WorkspaceReloadInterval)
	p.SetBudgetService(budgetService)
	p.SetBedrockConfig(proxy.BedrockConfig{
		Region:          cfg.AWSRegion,
		AccessKeyID:     cfg.AWSAccessKeyID,
		SecretAccessKey: cfg.AWSSecretAccessKey,
		SessionToken:    cfg.AWSSessionToken,
	})
	p.SetExtraProviderConfig(proxy.ExtraProviderConfig{
		MistralKey: cfg.MistralAPIKey,
		GroqKey:    cfg.GroqAPIKey,
		VLLMURL:    cfg.VLLMBaseURL,
		VLLMKey:    cfg.VLLMAPIKey,
	})

	keyStore := auth.New(pool)
	// Same shared observational-write bound as attribution/pattern-capture:
	// the async last_used_at updater is goroutine-per-authenticated-request
	// and must not be able to pile up against the pool under overload (#174).
	keyStore.SetWriteLimiter(obsLimiter)
	if err := keyStore.LoadAll(ctx); err != nil {
		logger.Warn("auth: LoadAll failed", slog.String("err", err.Error()))
	}
	tenantStore := tenant.NewStore(pool)
	spendTracker := tenant.NewSpendTracker(tenantStore)
	// B18.3: the spending cap and rate limits set through PUT /v1/workspaces/{ws}/config are enforced.
	p.SetWorkspaceLimits(tenantStore, spendTracker)
	rateLimiter := ratelimit.New(redisClient, ratelimit.DefaultRules())
	// B39.9: an operator can give one workspace its own limits (PUT /v1/admin/workspaces/{wsID}/rate-limits).
	workspaceRateLimits := ratelimit.NewWorkspaceLimits(pool)
	if err := workspaceRateLimits.Reload(ctx); err != nil {
		logger.Warn("ratelimit: workspace limits load failed; defaults until the next reload", slog.String("err", err.Error()))
	}
	workspaceRateLimits.StartRefresh(ctx)
	rateLimiter.AttachWorkspaceLimits(workspaceRateLimits)

	// Token-bucket multi-tier limiter (Item 8). ⚠ ONE TIER, NO CALLER.
	//
	// This said: "per-workspace tiers are layered on at request time by callers
	// that build a per-request limiter from tenant.WorkspaceConfig." Nothing does.
	// The limiter is blank-assigned four lines below — "exposed for future
	// per-request wiring" — and the two claims contradicted each other in the same
	// screen.
	//
	// What actually rate-limits traffic is the sliding-window limiter above:
	// ratelimit.DefaultRules() (100/s · 1000/min · 10000/hr) for every workspace,
	// except one an operator has given its own limits (B39.9, workspace_rate_limits).
	// A workspace's own rate_limit_rpm and rate_limit_tpm, set through
	// PUT /v1/workspaces/{wsID}/config, are enforced too (B18.3), and can only lower them.
	multiTierLimiter := ratelimit.NewMultiTierLimiter(redisClient,
		ratelimit.LimitTier{
			Name: "global",
			Key:  "global",
			RPM:  cfg.GlobalRPM,
			TPM:  cfg.GlobalTPM,
		},
	)
	_ = multiTierLimiter // exposed for future per-request wiring

	// Parse or generate the EC P-256 JWT signing key (ES256).
	// Production deployments must set LENS_JWT_PRIVATE_KEY so that tokens
	// survive restarts and are shared across HA instances. When the env var
	// is absent we generate an ephemeral key — acceptable for a single-node
	// dev run but useless for HA (each instance would mint unverifiable tokens).
	var jwtKey *ecdsa.PrivateKey
	if cfg.JWTPrivateKey != "" {
		var keyErr error
		jwtKey, keyErr = auth.ParseECPrivateKeyPEM(cfg.JWTPrivateKey)
		if keyErr != nil {
			logger.Error("failed to parse LENS_JWT_PRIVATE_KEY", slog.String("error", keyErr.Error()))
			os.Exit(1)
		}
		logger.Info("JWT signing: loaded EC P-256 key from LENS_JWT_PRIVATE_KEY",
			slog.String("kid", auth.JWTKid))
	} else {
		var keyErr error
		jwtKey, keyErr = auth.GenerateECKey()
		if keyErr != nil {
			logger.Error("failed to generate ephemeral JWT key", slog.String("error", keyErr.Error()))
			os.Exit(1)
		}
		// ⚠ SEVERITY IS CONDITIONAL, AND THE HA CASE IS A REFUSAL, NOT A WARNING.
		//
		// The old line was a flat WARN saying "tokens will not survive a restart". Two problems: it
		// understated what happens (every signed-in user is logged out, which reads as an auth bug
		// rather than a restart), and a WARN on every boot is a line people learn to scroll past.
		//
		// The distinction drawn here is whether the configuration CAN work:
		//
		//   HA + no key  → it CANNOT. GenerateECKey is a plain in-process keygen with no
		//                  persistence and no sharing, so every instance holds a DIFFERENT key —
		//                  while JWTKid is the constant "lens-1", so the header does not even
		//                  reveal which. Behind a load balancer a token minted by one instance
		//                  fails on every other, so roughly (1 - 1/N) of authenticated requests
		//                  fail PER REQUEST, intermittently, looking exactly like a flaky auth bug.
		//                  A configuration that cannot work is a refusal, not a warning.
		//
		//   economy on   → it works and DEGRADES: sessions survive until the next restart. Refusing
		//                  here would convert an occasional partial outage into a certain total
		//                  one, which is worse for the people it is meant to protect. So: ERROR —
		//                  loud, alertable, but it boots.
		//
		//   otherwise    → a local single-instance experiment. WARN, as before.
		const jwtKeyRemedy = "generate one with: openssl ecparam -name prime256v1 -genkey -noout " +
			"(the -noout is REQUIRED — without it openssl emits an EC PARAMETERS block first and " +
			"parsing fails with `unexpected PEM type \"EC PARAMETERS\"`), then set " +
			"LENS_JWT_PRIVATE_KEY to the whole PEM. PKCS#8 (openssl pkcs8 -topk8 -nocrypt) is also " +
			"accepted. The curve must be P-256."
		switch {
		case cfg.HAEnabled:
			logger.Error("LENS_JWT_PRIVATE_KEY is REQUIRED when LENS_HA_ENABLED is set, and Lens will "+
				"not start without it. Each instance would otherwise generate its own ephemeral "+
				"signing key, so a token issued by one instance is rejected by every other — "+
				"authentication would fail intermittently, per request, for as long as more than one "+
				"instance is running.",
				slog.String("remedy", jwtKeyRemedy))
			os.Exit(1)
		case cfg.EconomyEnabled:
			logger.Error("JWT signing: using an EPHEMERAL EC P-256 key. THE NEXT RESTART LOGS OUT "+
				"EVERY SIGNED-IN USER — existing tokens cannot be verified by the new key, and the "+
				"failure surfaces as ordinary auth rejections rather than as a restart. Booting "+
				"anyway: refusing would turn a restart-scoped outage into a total one.",
				slog.String("remedy", jwtKeyRemedy))
		default:
			logger.Warn("JWT signing: using ephemeral EC P-256 key — every signed-in user is logged "+
				"out on restart. Fine for a single-instance local run; set LENS_JWT_PRIVATE_KEY for "+
				"anything serving real sessions.",
				slog.String("remedy", jwtKeyRemedy))
		}
	}

	// Auth manager — unified JWT + workspace-key + global-key
	// authenticator. Coexists with the legacy auth.AuthMiddleware
	// (which is still mounted below for backward compat); new
	// /v1/auth/* routes use authManager directly.
	// LENS_MINT_KEY is the NARROW credential the content services (Docs, Track) hold: it can mint a
	// per-workspace token and nothing else. Unset ⇒ absent, and minting stays admin-only exactly as
	// before. It is deliberately NOT defaulted to the global key — a default that silently grants
	// admin is the failure this exists to remove.
	// LENS_OPERATOR_READ_KEY is the read-side sibling: the credential the suite BFF presents for the
	// cross-tenant admin READS behind its operator boundary (suite #87). Unset ⇒ absent, and those
	// reads stay reachable only by the global key exactly as before. Also deliberately NOT defaulted
	// to LENS_API_KEY: the whole point is that a session gateway never holds a credential that can
	// mint LXC, approve a conversion rate or revoke a payout.
	authManager := auth.NewManager(os.Getenv("LENS_API_KEY"), jwtKey, keyStore, tenantStore).
		WithMintKey(os.Getenv("LENS_MINT_KEY")).
		WithOperatorReadKey(os.Getenv("LENS_OPERATOR_READ_KEY"))

	// B20.13 — the marketplace moderator key: the web app's credential for the review queue and
	// nothing else. Created and revoked with `lens moderator-keys`; see docs/moderator-keys.md.
	moderatorKeys := moderatorkey.NewStore(pool)
	authManager = authManager.WithModeratorKeys(moderatorKeys)

	// W4.6.1 step 4 — session-scoped keys, the credential a browser chat holds instead of a
	// workspace key.
	//
	// ⚠ ATTACHED ONLY WHEN THE FLAG IS ON. Unset ⇒ authManager.sessionKeys stays nil, every
	// tlv_sk_ bearer is refused by the same branch that would have admitted it, and the three
	// routes below are never registered — a chi 404 rather than a route that exists and refuses.
	// A deployment that does not opt in is byte-for-byte unchanged by this feature.
	var sessionKeyStore *sessionkey.Store
	if cfg.SessionKeysEnabled {
		sessionKeyStore = sessionkey.NewStore(pool)
		authManager = authManager.WithSessionKeys(sessionKeyStore)
		logger.Info("session-scoped keys ENABLED",
			slog.Duration("ttl_ceiling", cfg.SessionKeyTTL),
			slog.String("note", "a browser chat holds a tlv_sk_ credential scoped to {proxy} and to its own sign-in, instead of a workspace API key"))
	}

	// requireAdmin (admin-gate) is now a package-level helper in
	// authz_admin_handlers.go — it takes authManager explicitly so it is
	// testable over HTTP. Used for /metrics, /ha/status, and the #153
	// global-config write routes.

	// Retry policy + per-provider circuit breaker registry (Item 9).
	// Coexists with the legacy retry.Do helper.
	retryPolicy := retry.DefaultPolicy
	if cfg.RetryMaxAttempts > 0 {
		retryPolicy.MaxAttempts = cfg.RetryMaxAttempts
	}
	if cfg.RetryInitialDelay > 0 {
		retryPolicy.InitialDelay = cfg.RetryInitialDelay
	}
	if cfg.RetryMaxDelay > 0 {
		retryPolicy.MaxDelay = cfg.RetryMaxDelay
	}
	breakerRegistry := retry.NewBreakerRegistry(cfg.CBThreshold, cfg.CBResetTimeout, 2)
	// The circuit state-change hook (structured logging + HA gossip publish) is
	// installed by setupHA below, so the two compose in one place and behave
	// identically whether or not HA is enabled.
	retryExecutor := retry.NewExecutor(&retryPolicy, breakerRegistry)
	_ = retryExecutor // wired into proxy paths in a follow-up

	// High-Availability layer (Upgrade 7). Strictly opt-in via LENS_HA_ENABLED;
	// a no-op / in-process fallback when disabled, so single-instance behaviour
	// is unchanged. Built here so its health handlers are mountable below and
	// its registry/breaker drive graceful drain on shutdown.
	haComps := setupHA(ctx, cfg, redisClient, pool, breakerRegistry, rateLimiter, logger)

	// Singleton background jobs — wrapped with leader election so exactly one
	// instance runs each when HA is enabled; runs directly when disabled.
	go haComps.leader.Run(ctx, "eval-scheduler", 30*time.Second, func(lctx context.Context) {
		evalPipeline.StartScheduler(lctx, time.Minute)
	})
	go haComps.leader.Run(ctx, "anomaly-monitor", 30*time.Second, func(lctx context.Context) {
		anomalyDetector.StartMonitor(lctx, nc, 1*time.Hour)
	})

	// Keel (U25) cross-tenant DRIFT ATTRIBUTION — the population axis alongside the temporal anomaly
	// monitor above. DEFAULT-ON (cfg.KeelEnabled — see the flag's own doc in config.go): the whole
	// sweep + emission is gated here, so ⚠ A FRESH DEPLOYMENT DOES RECORD; only LENS_KEEL_ENABLED=false
	// makes it record nothing. Read-only over the consented corpus (routing_patterns opted_in=TRUE),
	// append-only into keel_findings, NEVER touches money (import-guarded). Leader-elected, NOT a ticker.
	if cfg.KeelEnabled {
		keelSweep := keel.NewSweep(
			keel.NewReader(dbrouting.ReadPool(pool, replicaPool)),
			keel.NewFindingsWriter(pool),
			keel.Config{MinWorkspaces: keel.DefaultMinWorkspaces, DeviationSigma: cfg.KeelDeviationSigma},
			cfg.KeelWindowSeconds, cfg.KeelLookback,
		)
		// K3 money-grade hardened emission — DEFAULT-ON (cfg.KeelHardenedEnabled). Reuses the same sweep's
		// single corpus read + primary-pool writer; adds NO new replica reader. PLACEHOLDER thresholds —
		// NO MONEY MAY MOVE ON THESE UNTIL N3 CALIBRATION.
		if cfg.KeelHardenedEnabled {
			keelSweep.EnableHardened(keel.HardenedConfig{
				MoneyCohortFloor:   cfg.KeelMoneyCohortFloor,
				MinSamples:         cfg.KeelMinSamples,
				PersistenceWindows: cfg.KeelPersistenceWindows,
				HardenedSigma:      cfg.KeelHardenedSigma,
			})
		}
		go haComps.leader.Run(ctx, "keel-drift-sweep", 30*time.Second, func(lctx context.Context) {
			keelSweep.StartScheduler(lctx, cfg.KeelInterval)
		})
	}

	// U14 audit-trail integrity — leader-only singleton jobs (exactly one instance).
	// Both DEFAULT OFF: the token_events retention sweeper is inert unless
	// LENS_AUDIT_RETENTION > 0, and the off-box export is inert unless
	// LENS_AUDIT_EXPORT_URL is set — StartSweeper/StartLoop return immediately when
	// their feature is disabled, so leader election spins on a no-op.
	// B39.6: under the lens_app role the main pool cannot DELETE token_events, so the sweeper takes
	// the lens_retention role's own URL when one is set.
	retentionPool := pool
	if cfg.AuditRetentionDatabaseURL != "" {
		rcfg, err := pgxpool.ParseConfig(injectSSLMode(cfg.AuditRetentionDatabaseURL, cfg.DBSSLMode))
		if err != nil {
			return fmt.Errorf("LENS_AUDIT_RETENTION_DATABASE_URL: %w", err)
		}
		rcfg.MaxConns = 2
		rcfg.ConnConfig.DefaultQueryExecMode = poolCfg.ConnConfig.DefaultQueryExecMode
		if retentionPool, err = pgxpool.NewWithConfig(ctx, rcfg); err != nil {
			return fmt.Errorf("retention pgxpool: %w", err)
		}
		defer retentionPool.Close()
	}
	auditRetention := audit.NewRetention(retentionPool, cfg.AuditRetention,
		cfg.AuditRequireExportBeforePrune, cfg.AuditExportURL != "")
	go haComps.leader.Run(ctx, "audit-retention", 30*time.Second, func(lctx context.Context) {
		auditRetention.StartSweeper(lctx, time.Hour)
	})
	auditExport := audit.NewScheduledExport(pool, cfg.AuditExportURL)
	go haComps.leader.Run(ctx, "audit-export", 30*time.Second, func(lctx context.Context) {
		auditExport.StartLoop(lctx, cfg.AuditExportInterval)
	})

	// Catalog-drift detection — ask the providers what they serve, alert on anything the catalog cannot
	// price. This is the PREVENTIVE half of the unpriced-model fix: catalog.ResolveRates stops unknown
	// traffic being free, but it bills it on a floor, and the only real repair is a person adding the
	// published rate. Until this existed, the discovery mechanism was a customer's request — i.e. after
	// the money was gone.
	//
	// ⚠ DETECTION ONLY, PERMANENTLY. It must never set a price: no provider serves rates in any API, so
	// anything automated would be scraping HTML on a money path. See internal/modelwatch's doc comment.
	//
	// ReportReadiness runs on EVERY instance, before leader election, and deliberately so: whether an
	// alert can reach a person is a property of the deployment's configuration, not of which pod won an
	// election, and it must be stated at boot even on a follower that will never poll.
	var watcher *modelwatch.Watcher // B10.5: also serves the discovered-models and price routes below
	if cfg.ModelWatchEnabled {
		alertSink, err := modelwatch.NewWebhookNotifier(cfg.OperatorAlertWebhookURL, cfg.OperatorAlertWebhookSecret)
		if err != nil {
			// A malformed sink URL is a configuration error worth failing on: the operator intended to be
			// alerted, and starting anyway would leave them believing they are covered.
			slog.Error("LENS_OPERATOR_ALERT_WEBHOOK_URL is invalid", slog.String("err", err.Error()))
			os.Exit(1)
		}
		// A nil *WebhookNotifier must be passed as a nil INTERFACE, not as a typed nil wrapped in a
		// non-nil interface — otherwise the `notifier == nil` readiness check silently reads false and
		// the whole anti-inert guarantee inverts into exactly the bug it is there to prevent.
		var notifier modelwatch.Notifier
		if alertSink != nil {
			notifier = alertSink
		}
		watcher = modelwatch.New(cfg.AnthropicAPIKey, cfg.OpenAIAPIKey, notifier)
		watcher.ReportReadiness()
		// B10.5 — the leader's poll records each provider's list and new models; EVERY instance applies
		// that record (retirements, confirmed prices) to its own in-memory catalog.
		watcher.SetStore(modelwatch.NewStore(pool))
		go watcher.ApplyLoop(ctx, modelwatch.DefaultApplyInterval)
		go haComps.leader.Run(ctx, "model-catalog-drift", 30*time.Second, func(lctx context.Context) {
			watcher.StartLoop(lctx, cfg.ModelWatchInterval)
		})
	}

	// Control-plane — stateless reconciler (leader-only) + syncer (every instance).
	//
	// Reconciler: marks stale nodes inactive in Postgres, builds a NodeSnapshot,
	// and publishes it to Redis on every tick.
	//
	// NodeSyncer: every Lens instance reads the Redis snapshot on every tick and
	// syncs live inference nodes into localRouterMulti so the proxy's smart
	// endpoint selection (least-loaded / lowest-latency) picks from the live
	// mining fleet rather than only statically configured endpoints.
	// HeartbeatStore buffers heartbeats in Redis so any Lens instance can
	// receive a heartbeat and the Reconciler on any instance (after leader
	// election) sees current liveness state immediately — the xDS HA property.
	cpHB := controlplane.NewHeartbeatStore(redisClient)
	cpStore := controlplane.NewNodeStore(pool, cpHB)
	const cpReconcileInterval = 30 * time.Second
	cpPublisher := controlplane.NewPublisher(redisClient, cpReconcileInterval)
	cpReconciler := controlplane.NewReconciler(cpStore, cpPublisher)
	go haComps.leader.Run(ctx, "node-reconciler", cpReconcileInterval, func(lctx context.Context) {
		cpReconciler.Run(lctx, cpReconcileInterval)
	})
	cpSyncer := controlplane.NewNodeSyncer(cpPublisher, localRouterMulti)
	go cpSyncer.Run(ctx, 30*time.Second)

	// LENS token mining ledger + cache-mining engine (Batch 2 Item 1).
	tokenLedger := mining.NewLedgerStore(pool)
	tokenLedger.SetMoneyWall(workspace.CheckMoneyWall) // B25.1: test LENS never reaches a real workspace
	earningsReader := earnings.NewReader(pool)
	// U6 Sybil floor: wire the verified-to-earn gate UNCONDITIONALLY at the
	// ledger chokepoint — a safety restriction must not be liftable by the
	// economy toggle (mirrors the LXC-fiat unconditional wiring). Every
	// mint-type credit now requires a verified-to-earn workspace; conservation
	// credits pass through. nil-safe for tests (they construct ledgers without it).
	tokenLedger.SetMintVerifier(earnverify.New(cfg.EarnRequireLivePurchase).WithTestWorkspaces()) // B26.6

	// SHADOW MODE for the six UNPROVEN mints (LENS_SHADOW_MINTS_ENABLED, default off). Each
	// computes what it WOULD pay, records it to lens_shadow_mints, and credits NOTHING — no
	// balance change, no spendable token, no supply movement. Wired at the SAME ledger chokepoint
	// as the U6 verifier, so a mint added later is covered without touching its call site.
	//
	// The sink is a shadowmint.Recorder, which holds a pool and no ledger — it cannot credit. See
	// internal/mining/shadow.go for the four structural obstacles that keep a shadow row from
	// becoming real, and migrations/0108 for why it is a separate table.
	if cfg.ShadowMintsEnabled {
		tokenLedger.SetShadowSink(shadowmint.New(pool), mining.ShadowableMintTypes())
		slog.Warn("SHADOW MODE: the six unproven mints record what they would pay and credit NOTHING",
			slog.String("mint_types", strings.Join(mining.ShadowableMintTypes(), ",")),
			slog.String("query", "SELECT mint_type, count(*), sum(would_mint_micro_lens) FROM lens_shadow_mints GROUP BY mint_type"),
		)
	}
	// U6 PR2: the per-identity mint rate cap — the universal steady-state bound on
	// Sybil wash yield. Wired UNCONDITIONALLY (a safety restriction the economy
	// kill must not lift, like the verifier). Default 1000 LENS/24h; 0 = off.
	// SEC-2: the cap config is whole LENS; the ledger cap is µLENS.
	tokenLedger.SetMintRateCap(mining.FloatToMicroFloor(cfg.MintRateCapLENS24h), 24*time.Hour)
	// P1 #9: the reputation bond on PoVI-receipt + pool-royalty mints. The gate func reads the flag
	// live; OFF (default) ⇒ the mint path is byte-identical (no reputation read). An ADDITIVE
	// constraint downstream of the U6 floor/rate-cap — only ever reduces or blocks a mint.
	tokenLedger.SetReputationGate(func() bool { return cfg.ReputationBondedMintingEnabled })
	// KE-2: Keel HARDENED idiosyncratic drift → a REDUCE-ONLY haircut on bonded reuse royalties. DEFAULT-ON
	// (closed-test — see the flag's own doc in config.go; LENS_KEEL_ROYALTY_HAIRCUT_ENABLED can force it off).
	// ⚠ SO ON A DEFAULT DEPLOYMENT THIS SEAM IS WIRED AND THE MINT PATH IS NOT BYTE-IDENTICAL. Only when the
	// flag is forced off is the seam never wired and the mint path byte-identical. The oracle reads keel_findings via the mint's tx (mining imports no keel); the money-path
	// floor/clamp/fail-open is enforced in mining.reputationBondedAmount. minWindowBucket restricts to findings
	// within the recent keel-persistence horizon (a few windows), so a long-ago drift does not penalise forever.
	if cfg.KeelRoyaltyHaircutEnabled {
		windowSecs := cfg.KeelWindowSeconds
		if windowSecs <= 0 {
			windowSecs = 3600
		}
		const recentWindows = 6 // recency horizon (placeholder — calibrate at N3)
		tokenLedger.SetDriftHaircut(func(ctx context.Context, tx pgx.Tx, workspaceID string) (float64, error) {
			minBucket := (time.Now().Unix() / windowSecs) - recentWindows
			return royaltyhaircut.Factor(ctx, tx, workspaceID, minBucket)
		})
	}
	// Phase-4a Item 0: the CacheMiner is RETIRED (removed, not left constructed).
	// The cache moat — a contributor earning when their cached work is reused
	// cross-tenant — is DELIVERED by pool-royalty (mintPooledRoyalty at the pooled
	// serve points, s×avoided_COGS, held+examined+clawback-guarded). CacheMiner's
	// cross-tenant rates duplicated that (and would double-mint if wired at the same
	// serve point); its only unique path — own-cache minting (owner==requester) — is
	// self-inflation (no second party, no value RECEIVED) and trivially farmable in a
	// single-workspace loop no linkage guard can see. So it is superseded, not needed.

	// Phase-2 Stage 2.1 Pool-B royalty mint: a SERVED cross-tenant pooled hit
	// mints s × avoided_COGS to the contributing tenant, exactly once per
	// serving request (UNIQUE(request_id) claim + CreditTx in one tx — extends
	// the existing FOR UPDATE ledger discipline, no advisory lock). Inert by
	// default: with LENS_POOL_ROYALTY_MINTING_ENABLED=false (the default) the
	// Minter no-ops before touching the DB, and pooled hits serve exactly as
	// Stage 2.0 left them.
	royaltyMinter := poolroyalty.NewMinter(
		pool, tokenLedger, cfg.PoolRoyaltyShare,
		func() bool { return cfg.PoolRoyaltyMintingEnabled },
	)
	// 2.3b primitive #1: the per-pair rolling-window mint cap — bounds any
	// party's worst case to cap × s × avoided_COGS per pair per window.
	// 0 (default) disables; exact under concurrency via the after-CreditTx
	// count inside the existing mint tx (no new lock).
	royaltyMinter.SetCap(cfg.PoolMintCapPerPair, cfg.PoolMintCapWindow)
	// 2.3b per-ENTRY cap — bounds one entry_id's mints across all contributors
	// (ownership churn). 0 (default) disables. Shares the per-pair window.
	royaltyMinter.SetEntryCap(cfg.PoolMintCapPerEntry, cfg.PoolMintCapWindow)
	// 2.3a holdback: mints credit HELD; the sweeper below settles them after
	// this window. Trigger-agnostic — billing settlement can replace the
	// timed sweeper later without touching the mint path.
	royaltyMinter.SetHoldbackWindow(cfg.PoolHoldbackWindow)
	// U6 PR2: enable the owner-linkage wash guard in production — deny a pooled
	// royalty mint between two workspaces sharing a captured card fingerprint.
	// Default-allow-on-missing; the rate cap bounds yield regardless.
	royaltyMinter.SetOwnerLinkageCheck(true)
	royaltyMinter.SetMoneyWall(workspace.CheckMoneyWall) // B25.1: no royalty between a test and a real workspace
	p.SetRoyaltyMinter(royaltyMinter)

	// ⚠ SAY WHEN POOLED HITS WILL SERVE AND PAY NOBODY.
	//
	// The mint being off is a deliberate default — it credits spendable LENS, so it is an
	// explicit opt-in. What was NOT deliberate is that the off state is completely silent:
	// Minter.MintServedHit returns (Result{}, nil) when its enabled predicate is false, before
	// touching the database, and mintPooledRoyalty only logs when the mint returns an ERROR. So
	// a deployment serving cross-tenant pooled hits writes NO row to pool_royalty_mints, NO row
	// to lens_token_ledger, and NO line to the log explaining either.
	//
	// That is how an operator arrives at an empty lens_token_ledger having verified the three
	// POOLING gates — the global flag, the pool-safety attestation, and both workspaces'
	// cache_poolable + earn_verified — and finds nothing wrong, because the gate that is shut is
	// a FOURTH one those three say nothing about.
	//
	// Logged at boot rather than per hit: it is a configuration state, not an event, and a line
	// per pooled serve would be noise that teaches people to filter it.
	if cfg.CachePoolableEnabled && !cfg.PoolRoyaltyMintingEnabled {
		slog.Warn("economy: cross-tenant pooling is ON but ROYALTY MINTING IS OFF — pooled hits "+
			"will serve and be charged, and contributors will earn NOTHING for them",
			slog.String("remedy", "set LENS_POOL_ROYALTY_MINTING_ENABLED=true to arm the mint"),
			slog.String("expect_while_off", "no rows in pool_royalty_mints and none in "+
				"lens_token_ledger, however many pooled hits are served — this is the reason, "+
				"not a fault in the mint"),
			slog.String("unaffected", "the consumer discount is wired independently, so pooled "+
				"hits are still billed at list×(1−r)"))
	}

	// Stage-2.3a finalize sweeper: settles due held mints (held → spendable;
	// supply counts here via the TypePoolRoyalty row FinalizeHeldTx writes).
	// Mirrors the povi-challenge loop: leader-elected, minute tick.
	// Registered UNCONDITIONALLY — never gated on the minting flag: committed
	// held rows must finalize even if minting is later disabled, or
	// contributor LENS strands in held forever. With minting off and no held
	// rows, each sweep is a single cheap indexed SELECT.
	finalizeSweeper := poolroyalty.NewFinalizeSweeper(pool, tokenLedger, "pool_royalty_mints")
	// Phase-3 Item 3: when the settlement fail-closed layer is armed, the sweeper
	// settles ONLY adjudicated-clean ('cleared') rows — an un-examined held row never
	// finalizes. DEFAULT ON ⇒ 'cleared'; only with the flag forced off is it 'held'
	// (byte-identical). ⚠ SO ON A FRESH DEPLOYMENT THIS SWEEPER IS ALREADY FAIL-CLOSED: a
	// pool-royalty mint the ring detector never examined holds instead of settling. The
	// traffic-mint sweeper below says DEFAULT-ON about this same flag; they agree now.
	// Paired with the SettlementClearer below.
	if cfg.SettlementFailClosedEnabled {
		finalizeSweeper.SetSettleStatus("cleared")
	}
	// U3: economy worker — gated on the master switch so no held→final settlement
	// (an economy-state ledger write) runs when the economy is off.
	if cfg.EconomyEnabled {
		go haComps.leader.Run(ctx, "pool-royalty-finalize", 30*time.Second, func(lctx context.Context) {
			finalizeSweeper.StartScheduler(lctx, time.Minute)
		})
	}
	if cfg.PoolRoyaltyMintingEnabled {
		logger.Warn("poolroyalty: Pool-B royalty minting is ENABLED — served cross-tenant pooled hits mint LENS to contributors",
			slog.Float64("royalty_share", cfg.PoolRoyaltyShare))
	}

	// Phase-1/3 Item 1 (settlement half): the traffic-mint finalize sweeper settles
	// due held CreditOnceHeld mints (cache/compute/embedding traffic mints — ALL
	// three now route to held) — held → spendable, per-row CAS on traffic_mint_holds,
	// supply counted here via the counted final type FinalizeHeldTxAs writes. Mirrors
	// the pool-royalty finalize sweeper: leader-elected, minute tick, registered
	// UNCONDITIONALLY (EconomyEnabled-gated START) so any committed held row settles
	// even if the producing mint is later disabled — otherwise held LENS strands
	// forever. Producers of traffic_mint_holds rows: cache is RETIRED (Phase-4a
	// Item 0 — superseded by pool-royalty); the pattern mint routes here HELD
	// (Phase-4a Item 1, mint_type=pattern_mine); the compute/embedding node mints
	// route to held (Phase-3) but stay dormant until the node network lands
	// (compute's NotifyServed hook has no caller; embeddingMiner is unwired).
	trafficMintSweeper := mining.NewTrafficMintSweeper(pool, tokenLedger)
	// Phase-4a Items 3+4: the fail-closed layer for the single-party traffic mints
	// (the pattern held mint). When SettlementFailClosedEnabled, the sweeper settles
	// ONLY 'cleared' rows, and the TrafficSettlementClearer promotes examined-clean-
	// due held→cleared using the single-party concentration detector (velocity spike
	// ⇒ flagged ⇒ never cleared ⇒ withheld from settlement). An un-examined held row
	// is never cleared → never settles (fail-closed). DEFAULT-ON for the closed test;
	// byte-identical (settle 'held') when off. Over pattern_mine (the live single-party
	// mint); the dormant compute/embedding node mints share the same table + guard.
	if cfg.SettlementFailClosedEnabled {
		trafficMintSweeper.SetSettleStatus("cleared")
	}
	patternConcentrationDetector := mining.NewSinglePartyConcentrationDetector(
		pool, mining.TypePatternMine, cfg.PatternConcentrationVelocityMax, time.Hour)
	trafficClearer := mining.NewTrafficSettlementClearer(
		patternConcentrationDetector, pool,
		func() bool { return cfg.SettlementFailClosedEnabled }, cfg.AntiGamingScanWindow)
	// Phase-4a Item 4: detector health. maxStale = 15× the sweep interval — a stall
	// past that flips unhealthy (fail-closed strands mints; the stall must not be
	// silent). Exposed on the gauge lens_detector_last_run_age_seconds + the health
	// endpoint below; PublishAge ticks it between runs so a stall keeps rising.
	patternDetectorHealth := mining.NewDetectorHealth("traffic-pattern-clearer", 15*cfg.AntiGamingInterval)
	trafficClearer.SetHealth(patternDetectorHealth)
	if cfg.EconomyEnabled {
		go haComps.leader.Run(ctx, "traffic-mint-finalize", 30*time.Second, func(lctx context.Context) {
			trafficMintSweeper.StartScheduler(lctx, time.Minute)
		})
		go haComps.leader.Run(ctx, "traffic-settlement-clearer", 30*time.Second, func(lctx context.Context) {
			trafficClearer.StartScheduler(lctx, cfg.AntiGamingInterval)
		})
		go func() { // publish detector staleness on a tick so a stall is visible between runs
			t := time.NewTicker(cfg.AntiGamingInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					patternDetectorHealth.PublishAge()
				}
			}
		}()
	}

	// L2/S4 PR3: the distill reuse-royalty mint. A flag-gated, leader-elected
	// sweeper mints s × avoided_cogs_usd to the OCR contributor (owner A), ONCE
	// per cross-tenant reuse relationship, off the DEDUPLICATED distill_royalty_basis
	// table (PR2) — claim-then-act exactly-once, reusing the Pool-B held-ledger
	// kernel (CreditHeldTx → U6 floor on the contributor + per-identity rate cap +
	// TypePoolRoyaltyHeld). Inert by default: with LENS_POOL_ROYALTY_MINTING_ENABLED
	// =false the sweeper no-ops BEFORE any DB access, so basis rows accumulate but
	// nothing mints. The distill finalize sweeper is the SAME kernel parameterized
	// for the distill claim table; registered UNCONDITIONALLY (EconomyEnabled-gated
	// start) so held rows settle even if minting is later disabled.
	distillMinter := poolroyalty.NewDistillMinter(
		pool, tokenLedger, cfg.PoolRoyaltyShare,
		func() bool { return cfg.PoolRoyaltyMintingEnabled },
	)
	distillMinter.SetOwnerLinkageCheck(true) // U6 PR2 wash guard, like the cache minter
	distillMinter.SetMoneyWall(workspace.CheckMoneyWall)
	distillMinter.SetHoldbackWindow(cfg.PoolHoldbackWindow)
	// PR1 distill mint caps (default 0/0 = off; deflationary — a cap only denies).
	distillMinter.SetCap(cfg.DistillMintCapPerPair, cfg.DistillMintCapWindow)
	distillMinter.SetContentCap(cfg.DistillMintCapPerContent, cfg.DistillMintCapWindow)
	// Proof-of-Improvement piece 1: both royalty minters keep the DEFAULT CostAnchor (share ×
	// avoided_COGS) — no SetAnchor call here, so the mint amount is byte-identical to #2. The
	// capability flag is reserved for a future eval-contribution mint that supplies a held-benchmark
	// anchor; it wires nothing reachable today.
	if cfg.ProofOfImprovementEnabled {
		logger.Info("proof-of-improvement: capability flag ON — the cost anchor stands for the royalty mints; the held-benchmark anchor is selected ONLY by the eval-contribution minter (instance 1)")
	}
	distillFinalizeSweeper := poolroyalty.NewFinalizeSweeper(pool, tokenLedger, "distill_royalty_mints")
	if cfg.SettlementFailClosedEnabled {
		distillFinalizeSweeper.SetSettleStatus("cleared") // Phase-3 Item 3: settle only adjudicated-clean rows
	}

	// Proof-of-Improvement instance 1: proof-of-eval-contribution. NewEvalContributionMinter is the SOLE
	// live caller of HeldBenchmarkAnchor (it constructs it from the rate). INERT by default: rate 0 ⇒ nil
	// anchor ⇒ RunOnce is a total no-op even with both flags on. Mints only when BOTH the dedicated
	// earning flag AND the capability flag are on AND the rate is positive. Routes through the SAME
	// held-ledger/U6 chokepoint (TypeEvalContributionHeld ∈ mintTypeList).
	evalContributionMinter := poolroyalty.NewEvalContributionMinter(
		pool, tokenLedger, cfg.EvalContributionRatePerPoint,
		func() bool { return cfg.EvalContributionMintingEnabled && cfg.ProofOfImprovementEnabled },
	)
	evalContributionMinter.SetHoldbackWindow(cfg.PoolHoldbackWindow)
	evalContributionFinalizeSweeper := poolroyalty.NewFinalizeSweeper(pool, tokenLedger, "eval_contribution_mints")
	if cfg.SettlementFailClosedEnabled { // examined-before-settle: settle only rows the single-party clearer cleared
		evalContributionFinalizeSweeper.SetSettleStatus("cleared")
	}

	// Proof-of-Improvement instance 2: proof-of-routing-prediction. NewRoutingPredictionMinter is the SECOND
	// sanctioned caller of HeldBenchmarkAnchor (it constructs it from the rate). INERT by default: rate 0 ⇒
	// nil anchor ⇒ RunOnce is a total no-op even with both flags on. Mints only when BOTH the dedicated
	// earning flag AND the capability flag are on AND the rate is positive. Routes through the SAME
	// held-ledger/U6 chokepoint (TypeRoutingPredictionHeld ∈ mintTypeList); the generic FinalizeSweeper
	// settles routing_prediction_mints unchanged (generic request_id/contributor_workspace_id columns).
	routingPredictionMinter := poolroyalty.NewRoutingPredictionMinter(
		pool, tokenLedger, cfg.RoutingPredictionRatePerPoint,
		func() bool { return cfg.RoutingPredictionMintingEnabled && cfg.ProofOfImprovementEnabled },
	)
	routingPredictionMinter.SetHoldbackWindow(cfg.PoolHoldbackWindow)
	routingPredictionFinalizeSweeper := poolroyalty.NewFinalizeSweeper(pool, tokenLedger, "routing_prediction_mints")
	if cfg.SettlementFailClosedEnabled { // examined-before-settle: settle only rows the single-party clearer cleared
		routingPredictionFinalizeSweeper.SetSettleStatus("cleared")
	}

	// Proof-of-Improvement instance 3: proof-of-latency-locality. NewLatencyMinter is the THIRD sanctioned
	// caller of HeldBenchmarkAnchor (it constructs it from the rate). INERT by default: rate 0 ⇒ nil anchor ⇒
	// RunOnce is a total no-op even with both flags on. Mints only when BOTH the dedicated earning flag AND
	// the capability flag are on AND the rate is positive. Routes through the SAME held-ledger/U6 chokepoint
	// (TypeLatencyLocalityHeld ∈ mintTypeList); the generic FinalizeSweeper settles node_latency_mints
	// unchanged (generic request_id/contributor_workspace_id columns).
	latencyMinter := poolroyalty.NewLatencyMinter(
		pool, tokenLedger, cfg.LatencyRatePerPoint,
		func() bool { return cfg.LatencyMintingEnabled && cfg.ProofOfImprovementEnabled },
	)
	latencyMinter.SetHoldbackWindow(cfg.PoolHoldbackWindow)
	latencyFinalizeSweeper := poolroyalty.NewFinalizeSweeper(pool, tokenLedger, "node_latency_mints")

	// Proof-of-Improvement instance 4: proof-of-confidential-compute. NewConfidentialMinter is the FOURTH
	// sanctioned HeldBenchmarkAnchor caller. INERT by default (rate 0 ⇒ nil anchor ⇒ no-op) AND
	// inert-by-substrate-absence (no key_bound=true attestation exists until CC hardware). Pays verified
	// confidential capacity through the SAME held-ledger/U6 chokepoint (TypeConfidentialComputeHeld ∈
	// mintTypeList); the generic FinalizeSweeper settles confidential_compute_mints unchanged.
	confidentialMinter := poolroyalty.NewConfidentialMinter(
		pool, tokenLedger, cfg.ConfidentialRatePerPoint,
		func() bool { return cfg.ConfidentialMintingEnabled && cfg.ProofOfImprovementEnabled },
	)
	confidentialMinter.SetHoldbackWindow(cfg.PoolHoldbackWindow)
	confidentialFinalizeSweeper := poolroyalty.NewFinalizeSweeper(pool, tokenLedger, "confidential_compute_mints")

	if cfg.EconomyEnabled {
		go haComps.leader.Run(ctx, "distill-royalty-mint", 30*time.Second, func(lctx context.Context) {
			distillMinter.StartScheduler(lctx, time.Minute) // RunOnce no-ops while the mint flag is off
		})
		go haComps.leader.Run(ctx, "distill-royalty-finalize", 30*time.Second, func(lctx context.Context) {
			distillFinalizeSweeper.StartScheduler(lctx, time.Minute)
		})
		go haComps.leader.Run(ctx, "eval-contribution-mint", 30*time.Second, func(lctx context.Context) {
			evalContributionMinter.StartScheduler(lctx, time.Minute) // RunOnce no-ops while inert (rate 0 / flags off)
		})
		go haComps.leader.Run(ctx, "eval-contribution-finalize", 30*time.Second, func(lctx context.Context) {
			evalContributionFinalizeSweeper.StartScheduler(lctx, time.Minute)
		})
		go haComps.leader.Run(ctx, "routing-prediction-mint", 30*time.Second, func(lctx context.Context) {
			routingPredictionMinter.StartScheduler(lctx, time.Minute) // RunOnce no-ops while inert (rate 0 / flags off)
		})
		go haComps.leader.Run(ctx, "confidential-compute-mint", 30*time.Second, func(lctx context.Context) {
			confidentialMinter.StartScheduler(lctx, time.Minute) // RunOnce no-ops while inert (rate 0 / flags off / no key_bound=true rows)
		})
		go haComps.leader.Run(ctx, "confidential-compute-finalize", 30*time.Second, func(lctx context.Context) {
			confidentialFinalizeSweeper.StartScheduler(lctx, time.Minute)
		})
		go haComps.leader.Run(ctx, "routing-prediction-finalize", 30*time.Second, func(lctx context.Context) {
			routingPredictionFinalizeSweeper.StartScheduler(lctx, time.Minute)
		})
		go haComps.leader.Run(ctx, "latency-mint", 30*time.Second, func(lctx context.Context) {
			latencyMinter.StartScheduler(lctx, time.Minute) // RunOnce no-ops while inert (rate 0 / flags off)
		})
		go haComps.leader.Run(ctx, "latency-finalize", 30*time.Second, func(lctx context.Context) {
			latencyFinalizeSweeper.StartScheduler(lctx, time.Minute)
		})

		// The "smoke detector" — the scheduled cache+distill fraud-detector sweep. Mirrors
		// the finalize sweeper's leader.Run start. Economy-gated by this enclosing block (no
		// mints → nothing to scan); DetectorSweepEnabled defaults TRUE so detection accompanies
		// minting automatically (a manual off-switch only). READ-ONLY: it reads the mint tables
		// + records append-only findings + sets a gauge — it NEVER resolves/adjudicates or
		// touches a mint/balance/held row (the never-auto-act invariant; see detector_sweep.go).
		detectorSweep := poolroyalty.NewDetectorSweep(
			poolroyalty.NewDetectorReader(pool, thresholdsFromConfig(cfg)),
			poolroyalty.NewDistillDetectorReader(pool, thresholdsFromConfig(cfg)),
			poolroyalty.NewFindingsWriter(pool),
			cfg.DetectorSweepWindow,
		)
		go haComps.leader.Run(ctx, "royalty-detector-sweep", 30*time.Second, func(lctx context.Context) {
			if cfg.DetectorSweepEnabled {
				detectorSweep.StartScheduler(lctx, cfg.DetectorSweepInterval)
			}
		})

		// Phase-2 anti-gaming AUTO clawback — the leader-elected sweep that detects
		// self-dealing RINGS (transitive identity closure over held cross-tenant royalty
		// mints — the case the pairwise mint-time guard misses) and REVOKES them before
		// settlement, through the SAME durable Adjudicate path the admin endpoint uses
		// (record-before-revoke + the exactly-once Revoker). Unlike the read-only smoke
		// detector above, this one ACTS — so it is DEFAULT-OFF: this EconomyEnabled block
		// only starts the loop; RunOnce is a total no-op until AntiGamingAutoRevokeEnabled
		// is set (a Phase-4 decision, after threshold calibration). FAIL-CLOSED: a detector
		// error aborts the tick with no clawback (never a partial revoke).
		antiGamingAdjudicator := poolroyalty.NewAutoAdjudicator(
			poolroyalty.NewRingDetector(pool, "pool_royalty_mints"),
			poolroyalty.NewAdjudicationWriter(pool, poolroyalty.NewRevoker(pool, tokenLedger)),
			func() bool { return cfg.AntiGamingAutoRevokeEnabled },
			cfg.AntiGamingScanWindow,
		)
		go haComps.leader.Run(ctx, "antigaming-auto-adjudicate", 30*time.Second, func(lctx context.Context) {
			antiGamingAdjudicator.StartScheduler(lctx, cfg.AntiGamingInterval)
		})

		// The SAME auto clawback over distill_royalty_mints — the other cross-tenant reuse
		// faucet where a self-dealing ring can form (contributor↔requester). Reuses the
		// identical parameterized detector (both tables expose contributor_workspace_id +
		// requester_workspace_id + status + finalize_after) and the already-tested distill
		// Revoker/AdjudicationWriter (distill_royalty_adjudications). Same default-OFF gate,
		// same fail-closed. The single-party P-o-I tables (eval/routing/latency/confidential)
		// and traffic_mint_holds have no contributor↔requester edge — no ring shape — so they
		// are out of the ring detector's scope (see report: Sybil-cluster detection is the
		// follow-on for those, and they are inert today).
		distillAntiGaming := poolroyalty.NewAutoAdjudicator(
			poolroyalty.NewRingDetector(pool, "distill_royalty_mints"),
			poolroyalty.NewAdjudicationWriterForTable(pool,
				poolroyalty.NewRevokerForTable(pool, tokenLedger, "distill_royalty_mints"),
				"distill_royalty_adjudications"),
			func() bool { return cfg.AntiGamingAutoRevokeEnabled },
			cfg.AntiGamingScanWindow,
		)
		go haComps.leader.Run(ctx, "antigaming-auto-adjudicate-distill", 30*time.Second, func(lctx context.Context) {
			distillAntiGaming.StartScheduler(lctx, cfg.AntiGamingInterval)
		})

		// Phase-3 Item 3: the settlement CLEARER. It promotes examined-clean-AND-due held
		// rows to 'cleared' so the fail-closed FinalizeSweeper (armed above when
		// SettlementFailClosedEnabled) can settle them; a held row the ring detector never
		// examined is never cleared → never settles (the fail-closed money guarantee). Reuses
		// the SAME read-only RingDetector as the auto-adjudicator (examined = the held rows it
		// scanned this tick; flagged rings excluded). DEFAULT ON — the same flag that arms the
		// finalize sweepers above, so on a deployment whose environment says nothing these
		// clearers RUN and held rows do reach 'cleared'; fail-closed on a detector error.
		// Same scan window as anti-gaming (MUST exceed the 72h holdback so every held
		// row is examined before it is due). Over both cross-tenant reuse tables.
		poolClearer := poolroyalty.NewSettlementClearer(
			poolroyalty.NewRingDetector(pool, "pool_royalty_mints"), pool, "pool_royalty_mints",
			func() bool { return cfg.SettlementFailClosedEnabled }, cfg.AntiGamingScanWindow)
		go haComps.leader.Run(ctx, "settlement-clearer", 30*time.Second, func(lctx context.Context) {
			poolClearer.StartScheduler(lctx, cfg.AntiGamingInterval)
		})
		distillClearer := poolroyalty.NewSettlementClearer(
			poolroyalty.NewRingDetector(pool, "distill_royalty_mints"), pool, "distill_royalty_mints",
			func() bool { return cfg.SettlementFailClosedEnabled }, cfg.AntiGamingScanWindow)
		go haComps.leader.Run(ctx, "settlement-clearer-distill", 30*time.Second, func(lctx context.Context) {
			distillClearer.StartScheduler(lctx, cfg.AntiGamingInterval)
		})

		// The single-party P-o-I mints (routing-prediction, eval-contribution) are HELD
		// + fail-closed too, but a workspace earns for its OWN contribution — no ring. So
		// a per-workspace VELOCITY concentration detector examines them (mirrors pattern's
		// single-party guard). CRITICAL: without these clearers, arming their finalize
		// sweepers fail-closed (below) would STRAND every held row (nothing would ever
		// clear them). Same default-on/fail-closed/scan-window discipline; velocity
		// threshold shared with pattern (PatternConcentrationVelocityMax placeholder).
		routingClearer := poolroyalty.NewSettlementClearer(
			poolroyalty.NewSinglePartyConcentrationDetector(pool, "routing_prediction_mints", cfg.PatternConcentrationVelocityMax, time.Hour),
			pool, "routing_prediction_mints",
			func() bool { return cfg.SettlementFailClosedEnabled }, cfg.AntiGamingScanWindow)
		go haComps.leader.Run(ctx, "settlement-clearer-routing", 30*time.Second, func(lctx context.Context) {
			routingClearer.StartScheduler(lctx, cfg.AntiGamingInterval)
		})
		evalClearer := poolroyalty.NewSettlementClearer(
			poolroyalty.NewSinglePartyConcentrationDetector(pool, "eval_contribution_mints", cfg.PatternConcentrationVelocityMax, time.Hour),
			pool, "eval_contribution_mints",
			func() bool { return cfg.SettlementFailClosedEnabled }, cfg.AntiGamingScanWindow)
		go haComps.leader.Run(ctx, "settlement-clearer-eval", 30*time.Second, func(lctx context.Context) {
			evalClearer.StartScheduler(lctx, cfg.AntiGamingInterval)
		})
	}

	// Compute mining (Batch 2 Item 2). Wires its hook into the
	// localrouter so any verified GPU node that serves a
	// cross-workspace request gets credited automatically.
	computeMiner := mining.NewComputeMiner(tokenLedger, pool)
	computeMiner.SetHTTPClient(newNodeHTTPClient(cfg.NodeTLSSkipVerify, 5*time.Second, nodeCIDRAllow))
	computeMiner.SetHoldbackWindow(cfg.PoolHoldbackWindow) // Phase-3 Item 1: node mint lands HELD, finalizes after this window (mirrors cache/royalty)
	localRouterMulti.SetOnRequestServed(func(nodeID, requesterWS, requestID string, tokens int, latencyMs int64) {
		// RETIREMENT SWITCH (PoVI Part 3): the LEGACY trust-based mint (mints LENS
		// per served request, no receipt). U6: now DEFAULT OFF — an unprotected
		// mint path is opt-in. receipt-minting (LENS_POVI_MINTING_ENABLED) is the
		// intended successor. NOTE: NotifyServed has no caller today, so this hook
		// is dormant; when it is wired, requestID MUST be a server-derived
		// work-product hash (RecordServedRequest mints idempotently on it, and an
		// empty requestID mints nothing).
		if !cfg.TrustfulComputeMintEnabled {
			return
		}
		if err := computeMiner.RecordServedRequest(ctx, nodeID, requesterWS, requestID, tokens, latencyMs); err != nil {
			logger.Warn("compute mining: record served request failed",
				slog.String("node_id", nodeID), slog.String("err", err.Error()))
		}
	})

	// PoVI receipts (Token Economy Phase 1, Part 1). Verifies node-signed
	// receipts against each node's registered ed25519 pubkey, records them for
	// audit, and — ONLY when LENS_POVI_MINTING_ENABLED is on (default OFF,
	// and UNSAFE: a node can sign a fabricated trace) — performs a gated
	// provisional mint. Default behavior: verify + record, mint NOTHING.
	// NOTE: the existing ComputeMiner.RecordServedRequest above already mints
	// LENS on trust (no receipt); PoVI is designed to REPLACE it once Part 3
	// (challenge-and-slash) provides the economic security — that path stays
	// active and unsecured until then (tracked, not an oversight).
	poviStore := povi.NewStore(pool)
	// Mint-basis gate (migration 0099): the proxy WRITES Lens's own measurement of
	// each node-served request here; the PoVI processor READS it to price the mint
	// on the measurement (not the node's claim) and bind it to the serving node.
	// One store, shared: proxy = writer, processor = reader.
	poviMeasureStore := povi.NewMeasurementStore(pool)
	poviLookup := func(lookupCtx context.Context, nodeID string) (ed25519.PublicKey, error) {
		enc, err := computeMiner.NodePubKey(lookupCtx, nodeID)
		if err != nil {
			return nil, err
		}
		return povi.DecodePublicKey(enc)
	}
	// PoVI staking (Part 2): node collateral that gates minting-eligibility and
	// that Part 3 will slash. Reuses the ledger's atomic lock primitive
	// (tokenLedger satisfies povi.StakeLedger) via the locked_balance column.
	poviStakeStore := povi.NewNodeStakeStore(pool)
	poviStakeManager := povi.NewStakeManager(
		poviStakeStore, tokenLedger, computeMiner.NodeWorkspace,
		mining.FloatToMicroFloor(cfg.POVIMinStake), cfg.POVIUnbondPeriod, pool) // SEC-2: whole LENS → µLENS
	stakeEligible := func(eligCtx context.Context, nodeID string) bool {
		return poviStakeManager.IsEligible(eligCtx, nodeID)
	}
	// The mint gate is now verified AND stake-eligible AND minting-enabled
	// (still OFF by default).
	poviProcessor := povi.NewProcessor(poviStore, tokenLedger, poviLookup, stakeEligible, cfg.POVIMintingEnabled)
	// Price the mint on Lens's OWN gateway measurement (never the node's claimed
	// OutputTokens): resolve the receipt's request_id to the served-measurement
	// row, require node_id to match, price on the measured tokens. NO ROW ⇒ NO MINT.
	poviProcessor.SetMeasurementLookup(poviMeasureStore)
	if cfg.POVIMintingEnabled {
		logger.Warn("PoVI receipt minting is ENABLED (LENS_POVI_MINTING_ENABLED). Now SAFE only because Parts 1+2+3 are in place (verified receipt + staked node + challenge-and-slash). Ensure LENS_POVI_CHALLENGE_RATE > 0 so cheating is deterred.")
	}

	// PoVI challenge-and-slash (Part 3): the keystone. Lens randomly challenges
	// nodes to prove sampled Merkle paths; failures slash stake (Part 2),
	// making receipt-minting economically safe. Lens signs challenges with its
	// own ed25519 key (symmetric to Part 1's node-signed receipts); the node
	// verifies + answers from its retained trace.
	lensChallengePub, lensChallengePriv := loadOrGenChallengeKey(cfg.POVIChallengeKey, logger)
	challengeClient := povi.NewChallengeClient(lensChallengePriv, 10*time.Second)
	challengeClient.SetHTTPClient(newNodeHTTPClient(cfg.NodeTLSSkipVerify, 10*time.Second, nodeCIDRAllow))
	challengeStore := povi.NewChallengeStore(pool)
	// Blocker 6: gateway auto-route to registered nodes. Wired here (not at proxy.New) because it
	// reuses the EXISTING challenge private key (lensChallengePriv, just loaded above) to sign the
	// per-request node-auth token. enabled=cfg.NodeAutoRouteEnabled (default false) → fully inert.
	p.SetNodeRouter(localRouterMulti, lensChallengePriv, newNodeHTTPClient(cfg.NodeTLSSkipVerify, 30*time.Second, nodeCIDRAllow), cfg.NodeAutoRouteEnabled)
	// When the gateway serves a request from a node, record Lens's own measurement
	// of it (mint basis, 0099) so the node's later receipt mints against measurement.
	p.SetServedMeasurementRecorder(poviMeasureStore)
	poviChallenger := povi.NewChallenger(
		computeMiner.NodeURL, challengeClient, poviStakeManager, challengeStore,
		4, cfg.POVISlashFraction)
	challengeScheduler := povi.NewChallengeScheduler(poviChallenger, poviStore, cfg.POVIChallengeRate)
	// U3: economy worker — gated on the master switch (PoVI challenge/slash is an
	// economy-state mutation; no slashing when the economy is off).
	if cfg.EconomyEnabled {
		go haComps.leader.Run(ctx, "povi-challenge", 30*time.Second, func(lctx context.Context) {
			challengeScheduler.StartScheduler(lctx, time.Minute)
		})
	}

	// P1 #10 proof-of-benchmark (PR-A.5): wire the probe-mint SUPPRESSION into the receipt processor +
	// start the probe scheduler. Gated by LENS_PROOF_OF_BENCHMARK_ENABLED (default false); off ⇒ no
	// suppression checker (mint path byte-identical), no probes, no scheduler. MEASUREMENT, not economy
	// — NOT in the kill-switch force-off block, and the scheduler runs independently of EconomyEnabled.
	if cfg.ProofOfBenchmarkEnabled {
		benchStore := benchprobe.NewStore(pool)
		poviProcessor.SetProbeChecker(benchStore.IsProbe) // record-but-skip-mint for probe request_ids
		benchSigner := func(nodeID, requestID, bodyHash string, exp int64) (string, error) {
			return povi.SignNodeAuthToken(lensChallengePriv, nodeID, requestID, bodyHash, exp) // reuse the #242 challenge key
		}
		benchDelivery := benchprobe.NewHTTPDelivery(benchSigner, computeMiner.NodeURL, newNodeHTTPClient(cfg.NodeTLSSkipVerify, 30*time.Second, nodeCIDRAllow))
		benchScheduler := benchprobe.NewScheduler(benchStore, benchDelivery, func() bool { return cfg.ProofOfBenchmarkEnabled })
		benchScheduler.SetNodeLister(func(lctx context.Context) ([]benchprobe.NodeTarget, error) {
			rows, err := pool.Query(lctx, `SELECT id, COALESCE(models[1], '') FROM inference_nodes WHERE active`)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var out []benchprobe.NodeTarget
			for rows.Next() {
				var t benchprobe.NodeTarget
				if err := rows.Scan(&t.NodeID, &t.Model); err != nil {
					return nil, err
				}
				out = append(out, t)
			}
			return out, rows.Err()
		})
		go haComps.leader.Run(ctx, "proof-of-benchmark", time.Minute, func(lctx context.Context) {
			benchScheduler.StartScheduler(lctx, time.Minute)
		})

		// P1 #10 PR-B: bias node auto-route by the per-node benchmark score (earning-DISTRIBUTION, no
		// mint). SelectEndpoint composes quality WITHIN the strategy; the score loads via the periodic
		// quality-sync loop (leader-elected), never per request. Same flag; off ⇒ no bias, no loop.
		localRouterMulti.SetQualityEnabled(func() bool { return cfg.ProofOfBenchmarkEnabled })
		go haComps.leader.Run(ctx, "proof-of-benchmark-quality-sync", time.Minute, func(lctx context.Context) {
			localRouterMulti.StartQualitySync(lctx, 30*time.Second)
		})
	}

	// F4-capstone step B/C.1 — price-aware routing for agent requests. The gateway price ceiling clamps the
	// node-declared price (min(declared, ceiling)); the price loads via the leader-elected price-sync loop,
	// never per request. Always wired (the ceiling is a harmless bound; StrategyPriceAware is opt-in per
	// request via the agent path). StartPriceSync is blocking, so it composes with leader election.
	localRouterMulti.SetPriceCeiling(func() float64 { return cfg.GatewayPriceCeiling })
	go haComps.leader.Run(ctx, "price-sync", time.Minute, func(lctx context.Context) {
		localRouterMulti.StartPriceSync(lctx, 30*time.Second)
	})

	// Embedding mining (Batch 2 Item 3). Shares the ledger. The node-management
	// surface IS live and used below — RegisterNode, ListNodes,
	// ListAvailableNodes, DeactivateEmbeddingNode, GetStats are all wired to
	// routes. What is NOT wired is the embedding MINT: RecordEmbeddingsServed has
	// no caller (the semantic-cache serve path does not credit embedding LENS), so
	// — exactly like the compute node mint — the earning half is dormant until the
	// embedding node network lands. Constructed here and consumed by the routes;
	// there is no dead `_ =` handle (an earlier one falsely signalled "parked").
	embeddingMiner := mining.NewEmbeddingMiner(tokenLedger, pool)
	embeddingMiner.SetHTTPClient(newNodeHTTPClient(cfg.NodeTLSSkipVerify, 5*time.Second, nodeCIDRAllow))
	embeddingMiner.SetHoldbackWindow(cfg.PoolHoldbackWindow) // Phase-3 Item 1: node mint lands HELD, finalizes after this window (mirrors cache/royalty)

	// Annotation mining (Batch 2 Item 4). Proof-of-useful-work
	// — annotators stake LENS, review pairs of responses, and
	// earn per validated annotation.
	annotationMiner := mining.NewAnnotationMiner(tokenLedger, pool)
	// Phase-0 Item C: gate the annotation MINT on the economy master switch at the
	// source, so EconomyEnabled=false stops it too (its submit route is unconditional,
	// so route-gating alone previously missed it). Reads the flags live.
	// GA activation: AND with the dedicated AnnotationMintingEnabled (default OFF) —
	// the annotation reward is spendable-immediate (no held→examine→settle path), so
	// it is an explicit opt-in and is NOT in the GA on-by-default mint set (which is
	// only the two sound, funded mints: pattern held + pool royalty).
	annotationMiner.SetEconomyGate(func() bool { return cfg.EconomyEnabled && cfg.AnnotationMintingEnabled })

	// Annotation reputation SWEEP — ONE scheduled job that per tick (a) RESOLVES TTL-expired
	// tasks into agreement_outcome events from the FINAL consensus, and (b) DECAYS dormant
	// annotators' earned reputation toward baseline. Both are append-only writes to
	// reputation_events. Economy-gated + leader-elected, mirroring the detector sweep. Reputation
	// is MONEY-DECOUPLED: computed/gated/decayed/displayed but NEVER in the earning path
	// (annotation_mining.go SubmitAnnotation stays base + bonus) — pinned by an AST guard.
	reputationStore := mining.NewReputationStore(pool)
	// P1 #9: wire the reputation SIGNAL emitters only when the flag is on (off ⇒ Slash/Challenge
	// append nothing → byte-identical). slash → negative δ IN the slash tx (invariant 4); a passed
	// challenge → small positive δ best-effort (the recoverability / good-behavior signal).
	if cfg.ReputationBondedMintingEnabled {
		poviStakeManager.SetReputationSink(reputationStore)
		poviChallenger.SetReputationSink(reputationStore)
	}
	if cfg.EconomyEnabled {
		go haComps.leader.Run(ctx, "annotation-reputation-resolution", 30*time.Second, func(lctx context.Context) {
			reputationStore.StartScheduler(lctx, time.Hour)
		})
	}

	// Pattern mining (Batch 2 Item 5). The deployment-level
	// PatternMiningEnabled flag ANDs with the per-workspace
	// opt-in before earnings fire (RecordPattern's optedIn arg).
	patternMiner := mining.NewPatternMiner(tokenLedger, pool)
	// S2 routing-pattern earn cap — overrides the miner's real default from config.
	patternMiner.SetEarnCap(cfg.PatternEarnCapPerWorkspace, cfg.PatternEarnCapWindow)
	// Phase-4a Item 1: pattern mint lands HELD (traffic_mint_holds, mint_type=pattern_mine)
	// and finalizes after the holdback window — the same held+examined+settle path as
	// pool/distill, so the single-party concentration guard can examine it before settle.
	patternMiner.SetHoldbackWindow(cfg.PoolHoldbackWindow)

	// Routing intelligence (Upgrade 22). Consumes the opted-in pattern
	// aggregate to recommend best quality-per-dollar models. OFF by default;
	// when on, only auto-route requests are influenced. In-memory cohort
	// cache refreshed on a timer — the per-request path never hits the DB.
	// Cost basis reuses the alerts price table (blended $/1k tokens).
	routingAdvisor := routing.New(
		patternMiner,
		func(m string) float64 { return alerts.CostUSD(m, 500, 500) },
		routing.Config{Enabled: cfg.RoutingIntelligenceEnabled, TierCohorts: cfg.RoutingTierCohortsEnabled},
	)
	routingAdvisor.StartRefresh(ctx)
	p.SetRoutingAdvisor(routingAdvisor)

	// B27.23: every cost downgrade under automatic routing (cost-optimised routing, "auto", the
	// auto-route header, the Advisor, the Routing Brain) stands only when the cheaper model's
	// measured quality on the request's cohort (routing_patterns) is at or above the replaced
	// model's. Not flag-gated: it can only keep the more capable model, never pick one.
	qualityGate := routing.NewQualityGate(patternMiner, routing.Config{})
	qualityGate.StartRefresh(ctx)
	p.SetQualityGate(qualityGate)

	// Proof-of-Improvement piece 3: the routing-prediction SCORER, behind an Inferer interface. PR-3c
	// wires the REAL provider-backed Inferer (internal/inference) — it calls the SAME providers with the
	// SAME credentials the gateway uses (p.Endpoints()), resolving model→provider via the catalog. Wiring
	// it does NOT turn scoring on: RunOnce is still gated by LENS_ROUTING_PREDICTION_SCORING_ENABLED
	// (default off ⇒ inert: no DB scan, no inference call). NOT economy-gated (measurement, no mint).
	routingInferer := inference.NewProviderInferer(&http.Client{Timeout: 120 * time.Second}, retry.DefaultConfig(), p.Endpoints())
	routingScorer := routingscore.NewScorer(
		pool,
		routingscore.NewAdvisorBaseliner(routingAdvisor),
		routingInferer,
		func() bool { return cfg.RoutingPredictionScoringEnabled },
	)
	if cfg.RoutingPredictionScoringEnabled {
		logger.Info("routing-prediction scorer: flag ON with the real provider-backed Inferer (PR-3c) — scoring active")
	} else {
		logger.Info("routing-prediction scorer: real Inferer wired (PR-3c) but LENS_ROUTING_PREDICTION_SCORING_ENABLED is off — inert (RunOnce no-ops, no inference)")
	}
	go haComps.leader.Run(ctx, "routing-prediction-score", 30*time.Second, func(lctx context.Context) {
		routingScorer.StartScheduler(lctx, time.Minute) // RunOnce no-ops while inert (nil Inferer / flag off)
	})

	// Token marketplace + staking (Batch 3 Phase 1).
	marketplace := economy.NewMarketplaceStore(tokenLedger, pool)

	// Quality Oracle (Batch 3 Phase 5). Wraps the existing
	// annotation miner with a 1% request sampler + the
	// dashboard rollup query.
	oracleEngine := oracle.New(annotationMiner, tokenLedger, pool)

	// Two-token split (Master Plan Upgrade 1). RateEngine derives
	// the LENS->LXC conversion rate from backing + supply; the
	// admin can only approve its output. DualTokenStore owns the
	// one-way LENS->LXC conversion + LXC spend path.
	rateEngine := economy.NewRateEngine(tokenLedger, pool)
	dualToken := economy.NewDualTokenStore(tokenLedger, pool, rateEngine)
	// B30.5: every agent's Know Your Agent credential, signed with Ed25519.
	kyaKey, kyaEphemeral, err := kya.ParseKey(cfg.KYASigningKey)
	if err != nil {
		return fmt.Errorf("LENS_KYA_SIGNING_KEY: %w", err)
	}
	if kyaEphemeral {
		logger.Warn("LENS_KYA_SIGNING_KEY is not set: agent credentials are signed with a key made at this start — set a fixed one in production",
			"kid", kyaKey.Kid())
	}
	kyaSvc := kya.New(dualToken, kyaKey, cfg.KYACredentialTTL)
	// B45.23: every agent's A2A Agent Card, signed with a key derived from the same secret.
	agentCards := a2acard.New(dualToken, kyaKey.Derive, cfg.PublicURL)
	// B45.28: the owner's approvals as AP2 v0.2 mandates, and agents' approval evidence, signed with a key derived from it too.
	mandateSigner := ap2.NewSigner(kyaKey.Derive("talyvor/ap2/mandates/v1").Seed(), cfg.PublicURL)
	dualToken.SetMandateSigner(mandateSigner)

	// U18b billing (fiat Stripe → LXC credit). Constructed unconditionally so the
	// route handler method values are valid; the routes themselves register only
	// when cfg.BillingEnabled (billReg). Stripe keys are validated at config.Load
	// (startup fails if billing is enabled without them) and never logged.
	liveStripe := billing.NewLiveStripe(cfg.StripeSecretKey, cfg.BillingSuccessURL, cfg.BillingCancelURL)
	billingSvc := billing.New(pool, dualToken, liveStripe, cfg.StripeWebhookSecret)
	// B22.1: with a live Stripe key a bill is real money, which an uncleared AMBER or RED capability refuses.
	dualToken.SetLiveStripe(billing.LiveKey(cfg.StripeSecretKey))
	// B32.11: every model call charged to credits carries its plan's platform fee (LENS_PLATFORM_FEE_BPS).
	dualToken.SetPlatformFee(func(ctx context.Context, q economy.FeeQuerier, workspaceID string) (int64, error) {
		return billing.PlatformFeeBPS(ctx, q, workspaceID, fees.Current())
	})
	// MODEL 2 (W4.6.1 step 1). Subscriptions are a SECOND gate on top of billing: a
	// deployment selling one-off top-ups does not start selling a subscription because
	// it has a Stripe key. With no price configured the routes below are not registered
	// (chi-native 404) and the service refuses ErrNoSubscriptionPrice.
	if cfg.BillingSubscriptionPriceID != "" {
		billingSvc = billingSvc.WithSubscriptions(liveStripe, cfg.BillingSubscriptionPriceID)
	}
	// B13.1 — the named plans (Plus, Pro, Max). Each period's included usage is computed from the plan's
	// price at grant time; LENS_SUBSCRIPTION_ALLOWANCE_ULXC is only the fallback for an unknown fee.
	// B17.21 — on a Stripe test-mode key with no plan configured, the plans its Stripe account holds.
	livePlans := cfg.BillingSubscriptionPlans
	if cfg.BillingEnabled && cfg.BillingSubscriptionPriceID == "" {
		livePlans = sellablePlans(ctx, livePlans, cfg.StripeSecretKey, liveStripe, "LENS_BILLING_SUBSCRIPTION_PLANS")
	}
	if len(livePlans) > 0 {
		billingSvc = billingSvc.WithPlans(liveStripe, livePlans)
		backfillPlans(ctx, billingSvc, "live")
	}
	sellsSubscriptions := cfg.BillingSubscriptionPriceID != "" || len(livePlans) > 0
	// B13.2 — a subscriber's final royalty earnings come off their next renewal invoice (capped at the fee).
	if sellsSubscriptions {
		billingSvc = billingSvc.WithBillCredits(tokenLedger, liveStripe)
	}
	// B20.2 — a buyer's paid marketplace uses go on their monthly Stripe bill, and a paid invoice clears them.
	marketStore := market.NewStore(pool)
	marketStore.SetTrialMax(cfg.MarketTrialMax)                               // B32.21
	marketStore.SetLineageLimits(cfg.LineageMaxShareBPS, cfg.LineageMaxDepth) // B32.24
	marketStore.SetLineageTotalCap(cfg.LineageTotalCapBPS)                    // B32.26
	dualToken.SetListingCharger(marketStore)                                  // B19.17: a schedule may pay a marketplace listing
	// B32.46: each version is fingerprinted with the embeddings API, or — without a key to reach it — the offline hashed
	// embedder, and one at or above LENS_MARKET_SIMILARITY_HOLD to another seller's listing it does not declare is held.
	var marketEmbedder market.Embedder = openAIEmbedder
	if cfg.OpenAIAPIKey == "" && cfg.EmbeddingBaseURL == "" {
		marketEmbedder = embedder.NewHashedEmbedder()
	}
	marketStore.SetSimilarity(marketEmbedder, cfg.MarketSimilarityHold)
	marketStore.SetIPCounterDays(cfg.IPCounterDays) // B32.47
	// The real workspaces' Stripe (B25.6: a test workspace's is on the test-mode key, below).
	liveSide := stripeSide{cards: agentcard.NewStripe(cfg.StripeSecretKey, cfg.StripeIssuingCurrency)} // B19.12
	if cfg.BillingEnabled && cfg.MarketBillPriceID != "" {
		billingSvc = billingSvc.WithMarketBill(liveStripe, cfg.MarketBillPriceID, cfg.MarketMeterEvent, marketStore).
			WithMarketTax(cfg.MarketTaxMeterEvent, cfg.MarketTaxPriceID) // B32.39
		liveSide.bill = billingSvc
	}
	// B20.5 — sellers connect a Stripe account and are paid monthly; a buyer's refund or chargeback reverses
	// the earnings it paid for.
	if cfg.BillingEnabled {
		liveSide.connect = liveStripe
		billingSvc = billingSvc.WithMarketPayouts(liveStripe, marketStore)
	}
	// B19.25 — an agent card purchase's capture, reversal or refund settles the agent's balance, from the
	// Issuing events on the regular webhook. Cards exist only with the real-time endpoint configured.
	if cfg.StripeIssuingWebhookSecret != "" {
		billingSvc = billingSvc.WithAgentCards(dualToken)
	}
	// B1.6 — D, the allowance each paid period grants. Zero (the default) grants
	// nothing, and then there is nothing for a served request to draw down either.
	billingSvc = billingSvc.WithAllowance(cfg.SubscriptionAllowanceULXC)
	// B25.2 — test (synthetic) workspaces pay through Stripe TEST MODE, always, even once the key above is
	// live: a Service of their own with the test-mode key, webhook (/v1/billing/webhook/test) and plans. Each
	// Service takes only its own kind of workspace's money. Unset, a test workspace cannot buy or subscribe.
	var testBillingSvc *billing.Service
	var testStripe *billing.LiveStripe
	var testSide stripeSide // B25.6 — a test workspace's marketplace bill, payouts and agent cards, in test mode
	if cfg.StripeTestSecretKey != "" {
		testSide.cards = agentcard.NewStripe(cfg.StripeTestSecretKey, cfg.StripeIssuingCurrency)
	}
	if cfg.BillingEnabled && cfg.StripeTestSecretKey != "" && cfg.StripeTestWebhookSecret != "" {
		testStripe = billing.NewTestModeStripe(cfg.StripeTestSecretKey, cfg.BillingSuccessURL, cfg.BillingCancelURL)
		testBillingSvc = billing.New(pool, dualToken, testStripe, cfg.StripeTestWebhookSecret).
			ForTestWorkspaces(true).WithAllowance(cfg.SubscriptionAllowanceULXC)
		if plans := sellablePlans(ctx, cfg.StripeTestSubscriptionPlans, cfg.StripeTestSecretKey, testStripe, "LENS_STRIPE_TEST_SUBSCRIPTION_PLANS"); len(plans) > 0 {
			testBillingSvc = testBillingSvc.WithPlans(testStripe, plans)
			backfillPlans(ctx, testBillingSvc, "test-mode")
		}
		billingSvc = billingSvc.ForTestWorkspaces(false)
		// B25.6 — its marketplace bill is metered, refunded and cleared, its seller account made and paid, and its
		// agents' card purchases settled, all in test mode, from the test-mode webhook.
		if cfg.StripeTestMarketBillPriceID != "" {
			testBillingSvc = testBillingSvc.WithMarketBill(testStripe, cfg.StripeTestMarketBillPriceID, cfg.MarketMeterEvent, marketStore).
				WithMarketTax(cfg.MarketTaxMeterEvent, cfg.StripeTestMarketTaxPriceID)
			testSide.bill = testBillingSvc
		}
		testBillingSvc = testBillingSvc.WithMarketPayouts(testStripe, marketStore)
		testSide.connect = testStripe
		if cfg.StripeIssuingWebhookSecret != "" {
			testBillingSvc = testBillingSvc.WithAgentCards(dualToken)
		}
	}
	// B30.20 — an invoice's card payment posts to the issuing agent, from whichever webhook its Stripe key calls.
	billingSvc = billingSvc.WithInvoices(dualToken)
	if testBillingSvc != nil {
		testBillingSvc = testBillingSvc.WithInvoices(dualToken)
	}
	// B22.4 — company credit lines: each month's draws go on one Stripe invoice; paying it repays the line.
	// B26.4 — a test company's goes on a Stripe test-mode invoice, from the test workspaces' Service.
	if cfg.BillingEnabled {
		creditLines := creditLineInvoicers(billingSvc, liveStripe, billing.LiveKey(cfg.StripeSecretKey), testBillingSvc, testStripe)
		go haComps.leader.Run(ctx, "credit-line-invoices", 30*time.Second, func(lctx context.Context) {
			invoiceCreditLines(lctx, creditLines...)
		})
	}
	stripeKinds := newStripeByKind(wsManager.GetSynthetic, billing.LiveKey(cfg.StripeSecretKey), testBillingSvc != nil, liveSide, testSide)
	// B32.38 — a buyer's tax profile, its tax id checked with the tax partner (the Test one until B32.45), and where
	// the buyer resolves to: the declaration weighed against its Stripe customer's billing and card countries.
	partnerRegistry := partners.NewRegistry(dualToken)
	taxData := partners.NewTaxStore(pool)
	partnerRegistry.UseTaxData(taxData)
	// B32.45 — LENS_TAX_PARTNER=stripe: Stripe Tax, in test mode, works out the tax on Talyvor's own sales.
	if err := useTaxPartner(partnerRegistry, cfg, taxData); err != nil {
		return err
	}
	// B30.112 — a person's identity check goes to Persona and a UK company's to Companies House once their keys are set;
	// synthetic workspaces' checks stay with the Test provider.
	if cfg.PersonaAPIKey != "" && cfg.PersonaTemplateID != "" {
		partnerRegistry.UseVerifier(partners.KYCPerson, &partners.PersonaKYC{Key: cfg.PersonaAPIKey, TemplateID: cfg.PersonaTemplateID})
	}
	if cfg.CompaniesHouseAPIKey != "" {
		partnerRegistry.UseVerifier(partners.KYCCompany, &partners.CompaniesHouseKYC{Key: cfg.CompaniesHouseAPIKey}, "GB")
	}
	// B30.12 — the status page lists each partner, Test or live, its last success and failure, and its clearances.
	statusPage.UseMoneyRails(partnerRegistry, dualToken)
	// B30.6 — every payee and every payment in or out through a partner is screened against the UK Sanctions List and
	// OFAC's SDN list, downloaded at start and each day; the Test screening provider screens against them, and a match
	// opens a compliance case the operator sees at /v1/admin/screening.
	screeningLists := screening.NewStore(pool, screening.Sources(), cfg.ScreeningFuzzyThreshold, cfg.ScreeningMaxAge)
	partnerRegistry.UseScreeningList(screeningLists)
	// B37.5 — every call through a partner rail leaves one audit row in partner_calls.
	partnerRegistry.UseCallLog(partners.NewCallStore(pool))
	// B46.3 — the money layer: every switch is off, so the sandbox (the Test partners above) serves every call, and partner
	// webhooks enter at POST /v1/money/webhooks/{partner}.
	moneyLayer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(partnerRegistry, cfg.MoneySandboxWebhookSecret),
		Clearances: dualToken, Limits: noLimitsYet{}, Audit: dualToken, Ledger: moneyLedger{dualToken}, Replays: money.NewReplayStore(pool)})
	if err != nil {
		return err
	}
	// B37.2 — every rail answers a read-only probe every 5 minutes, and its last answers are kept in partner_rails, so
	// a restart and every Lens process show them.
	go partnerRegistry.WatchRails(ctx, partners.NewRailStore(pool), 5*time.Minute)
	screener := screening.NewScreener(pool, partnerRegistry)
	// B37.4 — lists older than LENS_SCREENING_MAX_AGE_HOURS hold every payment in or out for an operator, and the
	// status page shows the Sanctions screening rail down with their age.
	screener.HoldWhenListsStale(screeningLists)
	statusPage.UseScreeningLists(screeningLists)
	dualToken.SetScreener(screener)
	// B30.16 — an outside payee is screened when it is saved and again before it is paid.
	dualToken.SetPayeeScreener(screener)
	// B30.13 — a company's accounts in pounds, euros and dollars open at the account partner (the Test one until cleared).
	dualToken.SetAccountPartners(partnerRegistry)
	// B30.9 — each capability's terms, accepted before first use: version 1 of every text Lens carries
	// (docs/terms/<capability>.md) is published now, and the capability refuses a workspace that has not accepted it.
	if published, err := dualToken.PublishFirstTerms(ctx, economy.CapabilityTermsTexts(capabilityterms.FS)); err != nil {
		slog.Error("terms: publishing the capabilities' first terms failed; Lens publishes them when it next starts", slog.String("err", err.Error()))
	} else if len(published) > 0 {
		slog.Info("terms: published version 1 of the capabilities' terms", slog.Any("capabilities", published))
	}
	sanctionsAlert := &sanctionsListAlert{}
	if sink, err := modelwatch.NewWebhookNotifier(cfg.OperatorAlertWebhookURL, cfg.OperatorAlertWebhookSecret); err != nil {
		slog.Error("screening: LENS_OPERATOR_ALERT_WEBHOOK_URL is invalid; a sanctions list that does not download is only logged", slog.String("err", err.Error()))
	} else if sink != nil {
		sanctionsAlert.sink = sink
	}
	go haComps.leader.Run(ctx, "sanctions-lists", 30*time.Second, func(lctx context.Context) {
		refreshSanctionsLists(lctx, screeningLists, sanctionsAlert)
	})
	taxProfiles := taxprofile.NewStore(pool, partnerRegistry)
	if cfg.BillingEnabled {
		taxProfiles.SetStripe(taxStripeByKind{isTest: wsManager.GetSynthetic, mainKeyLive: billing.LiveKey(cfg.StripeSecretKey),
			live: liveStripe, test: testStripe})
	}
	// B32.41 — a seller's tax details, sealed under LENS_PROVIDER_SECRET_KEK (none stored without it), their VAT number
	// checked with the tax partner; a seller with earnings is asked for incomplete ones and, after the last request,
	// held from the payout run until they are complete.
	sellerTax := sellertax.NewStore(pool, cfg.ProviderSecretKeyring, partnerRegistry)
	sellerTax.SetReminderEvery(time.Duration(cfg.SellerTaxReminderDays) * 24 * time.Hour)
	go haComps.leader.Run(ctx, "seller-tax-reminders", 30*time.Second, func(lctx context.Context) {
		remindSellersOfTaxDetails(lctx, sellerTax)
	})
	// B30.20 — invoices: a pay link points at the web app, and a card pays through Stripe test mode only — the test-mode
	// key, or the main key while it is a test one. Each minute, transfers are matched to invoices and reminders sent.
	dualToken.SetInvoicePayBase(originOf(cfg.BillingSuccessURL))
	var invoiceCards invoiceCardCheckout
	switch {
	case testStripe != nil:
		invoiceCards = testStripe
	case cfg.BillingEnabled && !billing.LiveKey(cfg.StripeSecretKey):
		invoiceCards = liveStripe
	}
	go haComps.leader.Run(ctx, "invoice-sweep", 30*time.Second, func(lctx context.Context) {
		sweepInvoices(lctx, dualToken)
	})
	// B32.39 — every billed marketplace use carries its buyer's tax, worked out when it is metered; while the key is
	// live, a real buyer's sale Talyvor cannot account for the tax on is refused before it runs.
	marketStore.SetTax(market.Tax{Partners: partnerRegistry, Buyers: taxProfiles, Registrations: taxData})
	marketStore.SetLiveStripe(billing.LiveKey(cfg.StripeSecretKey))
	// Every workspace's bill is picked per workspace: in Stripe, or for a test workspace on a Lens with none in
	// Stripe, kept by Lens (B17.15). A workspace with neither is refused a paid use and a company payment.
	var marketMeter market.Meter = stripeKinds
	var marketRefunder market.Refunder = stripeKinds // B20.4: a taken-down listing's buyers are credited on their bill
	// B19.15: an agent pays another company's agent on the monthly bill
	dualToken.SetCompanyPayments(companyPaymentsOnBill{Store: marketStore, bills: stripeKinds})
	go haComps.leader.Run(ctx, "market-meter-pending", 30*time.Second, func(lctx context.Context) {
		meterPendingMarketUses(lctx, marketStore, stripeKinds)
	})
	go haComps.leader.Run(ctx, "market-refund-pending", 30*time.Second, func(lctx context.Context) {
		refundTakenDownMarketUses(lctx, marketStore, stripeKinds)
	})
	// B32.17: an earning leaves the holdback for the seller's available balance once its 14 days are over.
	go haComps.leader.Run(ctx, "market-release-holdback", 30*time.Second, func(lctx context.Context) {
		releaseMarketHoldbacks(lctx, marketStore)
	})
	// B32.50: each night, the listings' daily stats and trending scores.
	go haComps.leader.Run(ctx, "market-discovery-stats", 30*time.Second, func(lctx context.Context) {
		refreshMarketDiscovery(lctx, marketStore)
	})
	if cfg.BillingEnabled {
		go haComps.leader.Run(ctx, "market-payouts", 30*time.Second, func(lctx context.Context) {
			payMarketSellers(lctx, marketStore, stripeKinds, cfg.MarketPayoutWeekday) // B32.42: weekly
		})
	}
	billRoute := newBillingRouter(billingSvc, testBillingSvc, wsManager.GetSynthetic, cfg.StripeSecretKey, cfg.StripeTestSecretKey, cfg.StripeTestWebhookSecret)
	if cfg.SubscriptionAllowanceULXC > 0 || (cfg.BillingEnabled && billRoute.sellsSubscriptions()) {
		p.SetSubscriptionAllowance(billingSvc)
	}
	// B27.26 — BYOK: a workspace on the BYOK plan is served on its own provider keys, sealed under
	// LENS_PROVIDER_SECRET_KEK. Without the KEK there is no custody: no key routes and no own-key serving.
	var byokStore *byok.Store
	if cfg.ProviderSecretsEnabled() {
		byokStore = byok.New(pool, cfg.ProviderSecretKeyring)
		p.SetOwnKeys(byokStore)
	}
	bill := billReg{on: cfg.BillingEnabled}
	subs := billReg{on: cfg.BillingEnabled && billRoute.sellsSubscriptions()}
	// Stage 2.4/2.5 shadow LXC spend — observational, post-serve, flag-gated
	// (LENS_LXC_SHADOW_SPEND_ENABLED, default off). The proxy debits LXC
	// alongside the cost_usd write; void/non-gating, cannot affect serving.
	p.SetLXCSpendSink(dualToken, func() bool { return cfg.LXCShadowSpendEnabled })
	// B17.110 — an eval run asks the model on Talyvor's keys: each run is one charge on the workspace's ledger.
	evalPipeline.SetLXCCharger(dualToken)
	// LXC gating (Stage 2.4/2.5) — pre-serve block; inert unless LXCGatingEnabled
	// AND LXCShadowSpendEnabled are both on. Default off.
	p.SetLXCGate(dualToken, func() bool { return cfg.LXCGatingEnabled })
	// B9.8 — every browser-chat request is charged (allowance, then prepaid) and one chat session may be
	// charged at most what an agent key may by default. Needs the session-key store, so only when enabled.
	if sessionKeyStore != nil {
		p.SetSessionSpend(sessionKeyStore, economy.DefaultAgentCeilingLXC)
	}
	// F4-capstone step C.1 — the agent allocator: pre-serve estimate-debit against the per-scoped-key LXC
	// sub-budget (SpendLXCForAgent), gated on LXCAgentAllocationEnabled. Non-agent traffic (empty APIKeyID)
	// is unaffected. Mints the server-derived debit-key salt on wire.
	p.SetAgentSpender(dualToken, func() bool { return cfg.LXCAgentAllocationEnabled })
	// Billing redesign: the reservation path (HOLD → SETTLE/RELEASE) — DEFAULT ON. When on, the pre-serve
	// seam holds a conservative bounded reservation and the post-serve seam settles the delivered cost;
	// shadow and settle are mutually exclusive by this flag (no double charge).
	p.SetReservation(func() bool { return cfg.LXCReservationEnabled }, func() int { return cfg.LXCReservationMaxOutputTokens })
	p.SetDeepgram(cfg.DeepgramAPIKey, cfg.DeepgramURL) // B40.10: speech
	// Crash-recovery sweeper: RELEASES (refunds) reservations stranded by a crash between hold and settle.
	// The normal path settles PROMPTLY at the post-serve seam; this is for crashes ONLY and never auto-settles
	// (a stranded serve's outcome is unknown, so the safe money move is to refund). Threshold >> any request.
	if cfg.LXCReservationEnabled {
		go func() {
			t := time.NewTicker(2 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if n, err := dualToken.ReleaseStrandedReservations(ctx, 15*time.Minute); err != nil {
						slog.Warn("billing: stranded-reservation sweep failed", slog.String("err", err.Error()))
					} else if n > 0 {
						slog.Info("billing: swept stranded reservations (crash refund)", slog.Int("released", n))
					}
				}
			}
		}()
	}
	// B19.8: scheduled agent payments and automatic top-ups. Every tick runs once however many Lens
	// processes run it (the schedule's row is held while its tick is paid and advanced).
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				if res, err := dualToken.RunAgentSchedules(ctx, now); err != nil {
					slog.Warn("agents: schedule run failed", slog.String("err", err.Error()))
				} else if res != (economy.ScheduleRunResult{}) {
					slog.Info("agents: schedules run", slog.Int("paid", res.Paid), slog.Int("refused", res.Refused), slog.Int("topped_up", res.ToppedUp),
						slog.Int("licences_renewed", res.Renewed), slog.Int("licences_unpaid", res.Unpaid))
				}
				// B22.5: loan instalments due, taken from the borrowers' agents.
				if res, err := dualToken.RunLoanRepayments(ctx, now); err != nil {
					slog.Warn("agents: loan instalments failed", slog.String("err", err.Error()))
				} else if res != (economy.LoanRunResult{}) {
					slog.Info("agents: loan instalments", slog.Int("paid", res.Paid), slog.Int("missed", res.Missed), slog.Int("defaulted", res.Defaulted))
				}
				// B22.6: escrows whose deadline passed without a dispute, released to their payees.
				if n, err := dualToken.RunEscrowDeadlines(ctx, now); err != nil {
					slog.Warn("agents: escrow deadlines failed", slog.String("err", err.Error()))
				} else if n > 0 {
					slog.Info("agents: escrows released at their deadline", slog.Int("released", n))
				}
				// B22.8: simulated limit orders the latest ECB quotes now cross.
				if res, err := dualToken.RunSimulatedOrders(ctx); err != nil {
					slog.Warn("agents: simulated orders failed", slog.String("err", err.Error()))
				} else if res != (economy.SimRunResult{}) {
					slog.Info("agents: simulated orders", slog.Int("filled", res.Filled), slog.Int("rejected", res.Rejected))
				}
				// B22.9: cash-outs handed to the partner, and its answers recorded.
				if res, err := dualToken.RunCashOuts(ctx); err != nil {
					slog.Warn("agents: cash-outs failed", slog.String("err", err.Error()))
				} else if res != (economy.CashOutRunResult{}) {
					slog.Info("agents: cash-outs", slog.Int("submitted", res.Submitted), slog.Int("paid", res.Paid), slog.Int("failed", res.Failed))
				}
			}
		}
	}()
	// B28.125: prompts scheduled from Chat, asked at their time as the paying agent's own key. Its own tick, so a slow
	// model never holds up the payments above; each time is claimed before it is asked, so it is asked once.
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				if answered, refused, err := dualToken.RunPromptSchedules(ctx, now, p); err != nil {
					slog.Warn("agents: scheduled prompts failed", slog.String("err", err.Error()))
				} else if answered+refused > 0 {
					slog.Info("agents: scheduled prompts asked", slog.Int("answered", answered), slog.Int("refused", refused))
				}
			}
		}
	}()
	// B19.12: agent card purchases are priced at the day's ECB reference rate, and B22.8's simulated trading
	// quotes every instrument from it, so the rates are fetched at start and every three hours (the ECB
	// publishes once a working day).
	ecbRates := ecbrate.New(pool, ecbrate.DailyURL)
	// B30.7 — transaction monitoring: its rules judge each payment in or out through a partner once it has moved, and
	// nightly over the last day's; a hit opens or extends the workspace's monitoring case, which the operator sees at
	// /v1/admin/screening, with its alerts at /v1/admin/compliance/alerts. It never moves or stops money.
	monitorSettings, err := monitoring.LoadEnv()
	if err != nil {
		return err
	}
	moneyMonitor := monitoring.New(pool, monitorSettings, ecbRates)
	dualToken.SetMonitor(moneyMonitor)
	// B41.103: an approval card flags an amount far above the payee's usual by monitoring's own thresholds.
	dualToken.SetUnusualAmount(monitorSettings.NewPayeeLarge.TimesUsual, monitorSettings.NewPayeeLarge.FromPayments, monitorSettings.HistoryDays)
	go haComps.leader.Run(ctx, "transaction-monitoring", 30*time.Second, func(lctx context.Context) {
		sweepTransactionMonitoring(lctx, moneyMonitor)
	})
	// B30.11 — each day yesterday's money through partner accounts is reconciled with the account partner's statements;
	// any break is sent to the operator's alert sink, and the runs and the safeguarding view are at
	// /v1/admin/reconciliation and /v1/admin/safeguarding.
	go haComps.leader.Run(ctx, "reconciliation", 30*time.Second, func(lctx context.Context) {
		reconcileDaily(lctx, dualToken, partnerRegistry, sanctionsAlert.sink)
	})
	// B30.8 — the compliance case file: the operator reviews a case, freezes or unfreezes the workspace's money
	// capabilities, closes it with a reason and exports a report draft; every action behind step-up (a code from the
	// authenticator app holding LENS_OPERATOR_STEP_UP_SECRET, once on any replica; unset, the actions answer 503) and in
	// the audit trail.
	caseFile := compliance.New(pool, moneyMonitor)
	stepUp, err := stepup.New(os.Getenv("LENS_OPERATOR_STEP_UP_SECRET"), stepup.NewPostgresClaims(pool))
	if err != nil {
		return err
	}
	if !stepUp.Configured() {
		slog.Warn("compliance: LENS_OPERATOR_STEP_UP_SECRET is not set; the compliance case actions and screening decisions answer 503 until it is")
	}
	// B32.40 — every paid marketplace bill gets Talyvor's receipt, as the supplier: the buyer as its tax profile names
	// it, and the tax also in the buyer's currency at the ECB's reference rate; a test bill's invoice links it on the app's
	// origin (B28.140).
	marketStore.SetReceipts(market.Receipts{Supplier: market.Supplier{LegalName: cfg.SupplierLegalName, Address: cfg.SupplierAddress,
		VATNumber: cfg.SupplierVATNumber}, Buyers: taxProfiles, Rates: ecbRates, AppURL: cfg.WebAuthnOrigins[0]})
	marketStore.SetDisplayRates(ecbRates) // B32.51: listing reads show each offer in the buyer's currency
	// B32.43 — to a seller who agreed to self-billing each weekly payout is also a self-billed invoice from them to
	// Talyvor, with the VAT on their supply worked out by the tax partner and paid on top — at zero, "under review",
	// until LENS_SELF_BILLING_VAT is set.
	marketStore.SetSelfBilling(market.SelfBilling{Partners: partnerRegistry, Customer: market.Supplier{LegalName: cfg.SupplierLegalName,
		Address: cfg.SupplierAddress, VATNumber: cfg.SupplierVATNumber}, VAT: cfg.SelfBillingVAT})
	go func() {
		t := time.NewTicker(3 * time.Hour)
		defer t.Stop()
		for {
			if err := ecbRates.Refresh(ctx); err != nil {
				slog.Warn("ECB reference rates not refreshed", slog.String("err", err.Error()))
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	// Phase-3 routing-pattern capture — post-serve, void, mint-free producer
	// for the routing Advisor. Default off; persists observations for opted-in
	// workspaces only (SQL gate). NEVER reaches ledger.Credit (earning is a
	// separate later stage).
	p.SetPatternCapture(patternMiner, func() bool { return cfg.PatternCaptureEnabled })
	p.SetObservationalLimiter(obsLimiter)
	// W4.9 cross-tenant pooling SHADOW LOG — post-serve, void, fingerprint-only.
	// Default off. It is handed a *poolshadow.Recorder, which holds a pool and
	// nothing else: no cache client (so it cannot read or write the pooled
	// keyspace) and no ledger handle (so it cannot mint). Read back offline with
	// poolshadow.Recorder.Rate; see migrations/0125.
	p.SetPoolShadowLog(poolshadow.New(pool), func() bool { return cfg.PoolShadowLogEnabled })
	// WorkTier descriptive classifier (default-off capability flag). Post-serve,
	// NON-CONTENT, mint-free; shares the obsLimiter budget. The ANALYTICS consumer
	// (GET /v1/admin/worktier/distribution, registered below) reads this same store via
	// Aggregate; the routing Advisor tier-conditioning is still a future PR.
	worktierStore := worktier.NewStore(pool)
	p.SetWorkTier(worktierStore, func() bool { return cfg.WorkTierEnabled })

	// Routing Brain (H8.1) — DEFAULT-OFF capability flag, MINT-FREE. It learns OFFLINE
	// from verified outcomes (H2 capability curves + Keel drift, keyed by the H1/H2
	// work-tier difficulty) and writes a per-(workspace, cohort) recommendation; the
	// serve path reads the latest via a cheap in-memory lookup. Off → the offline job
	// never runs and the serve path reads nothing (routing byte-identical). Advisory is
	// the per-workspace default; autonomous is an explicit per-workspace opt-in.
	brainCost := func(m string) float64 { return alerts.CostUSD(m, 1000, 1000) } // blended per-model price for the hard floor
	brainStore := routingbrain.NewStore(pool)
	routingBrain := routingbrain.New(brainStore, brainCost, routingbrain.Config{Enabled: cfg.RoutingBrainEnabled})
	routingBrain.StartRefresh(ctx) // no-op while disabled
	p.SetRoutingBrain(routingBrain)
	if cfg.RoutingBrainEnabled {
		brainJob := routingbrain.NewJob(
			modelcapability.NewStore(pool),  // H2 capability curves (primary — the store is read-write)
			keel.NewReader(pool),            // Keel verified drift; primary keeps the brain's DB access uniform (its H2 + recommendation stores are read-write, so primary-only)
			brainWorkspaceSource{wsManager}, // workspaces + allow-lists (in-memory)
			brainStore, brainCost)
		go haComps.leader.Run(ctx, "routing-brain-compute", 30*time.Second, func(lctx context.Context) {
			brainJob.StartScheduler(lctx, 5*time.Minute) // recompute recommendations off the hot path
		})
		logger.Info("routing brain: ENABLED — offline compute leader-scheduled; advisory default, autonomous per-workspace opt-in")
	}

	// Route-decision capture — DESCRIPTIVE, MINT-FREE, default-ON. The go/no-go substrate for a
	// corpus-contribution mint: records baseline vs served model + estimated cost off-path, post-serve.
	routeDecisionWriter := routedecision.NewWriter(pool)
	p.SetRouteDecision(routeDecisionWriter, func() bool { return cfg.RoutingDecisionCaptureEnabled })
	routeDecisionReader := routedecision.NewReader(pool)

	// Routing-PREDICTION LIVE emit — the production emit that REPLACES the lens-routeseed CLI. When the live
	// cohort override fires (the farm gate), the auto-route path records a contributor prediction the offline
	// scorer (wired above) grades and the routing-prediction minter pays on. DEFAULT-OFF: gated by the same
	// LENS_ROUTING_PREDICTION_ENABLED capability flag as CLI submission (a prediction records nothing that
	// mints — minting stays behind RoutingPredictionMintingEnabled + ProofOfImprovementEnabled + a rate).
	routingPredictStore := routingpredict.NewStore(pool, func() bool { return cfg.RoutingPredictionEnabled })
	p.SetRoutingPredictEmit(routingPredictStore, func() bool { return cfg.RoutingPredictionEnabled })

	// K4 intrinsic output verifier — DEFAULT-OFF (LENS_K4_VERIFIER_ENABLED). Post-serve, off-path,
	// mint-free (import-guarded); writes verdicts to the PRIMARY pool. Reads (admin + workspace-scoped,
	// below) also use the PRIMARY, so this adds NO replica reader and the U8/U9 ExactlySix invariant is
	// unchanged. Moves no money; produces a verdict, nothing else.
	outputVerdictWriter := outputverify.NewWriter(pool)
	p.SetOutputVerifier(outputVerdictWriter, func() bool { return cfg.K4VerifierEnabled })
	// H5.β PROVENANCE BONDS — THE MONEY PATH. Constructed on the PRIMARY pool + the integer-µLENS ledger
	// (provenance.NewBondManager is on the moneyAuthzTx list in readrouting_invariant_test.go, so a future
	// edit can never route it to a replica). Endpoints are registered ONLY when LENS_H5_BONDS_ENABLED — off =
	// no bond surface, no locks, no slashes, zero behavior change. 0/0 → default 72h appeal window, 100% slash.
	bondManager := provenance.NewBondManager(pool, tokenLedger, 0, 0)
	// Bond read-back (query-only). PRIMARY pool: bond reads are money-adjacent (collateral + appeal
	// windows) and must not observe a lagging replica — never wire this through dbrouting.ReadPool.
	bondReader := provenance.NewBondReader(pool)
	// STEP 3 — the attested-slash PRODUCER. Talyvor's OWN sandboxed compile (buildverify) reproduces a bonded
	// output's build; attest records ONLY a trustworthy compile verdict as talyvor_verified (admin-only,
	// PRIMARY pool). Default-off (LENS_H5_ATTEST_ENABLED). See attest.NewAttestor in the moneyAuthzTx list.
	buildVerifier := buildverify.NewVerifier(cfg.H5AttestEnabled)
	attestor := attest.NewAttestor(pool, buildVerifier, cfg.H5AttestEnabled)
	outputVerdictReader := outputverify.NewReader(pool)
	// K4 CODE LOOP — the ownership-bound mechanical report-back writer (reads k4_output_verdicts to verify the
	// caller produced the output_id, writes k4_mechanical_verdicts). PRIMARY pool — no replica reader.
	mechanicalVerdictWriter := outputverify.NewMechanicalWriter(pool)
	attributionWriter := outputverify.NewAttributionWriter(pool)
	// Attribution read-back (query-only), PRIMARY pool — consistent with the attribution writer.
	attributionReader := outputverify.NewAttributionReader(pool)

	// P3 #6: the DESCRIPTIVE node-latency capture sink (default-off, mint-free — writes the aggregate the
	// future latency mint reads; RunOnce/capture no-ops while the flag is off).
	nodeLatencyStore := nodelatency.NewStore(pool)
	p.SetNodeLatencySink(nodeLatencyStore, func() bool { return cfg.NodeLatencyCaptureEnabled })

	// W6.1 step 2: the compression measurement sink — the SAME store the
	// /compression/savings route reads, so the writer and the reader cannot
	// drift onto different tables. No enable flag beside it: it fires only when
	// the 0117 gate opens, and a flag here would let the rewriter run unobserved
	// again (see proxy.SetCompressionSink).
	p.SetCompressionSink(compressionStore)

	// Proof-of-Confidential-Compute step (b): the gateway attestation VERIFY sweep. INERT unless the
	// capability flag + the pinned NVIDIA root CA are configured; records a verified hardware class to
	// node_attestations, mints nothing (the mint is step c). Flag-off ⇒ no scheduler, no dials, no writes.
	startAttestationVerify(ctx, cfg, pool, lensChallengePriv, haComps.leader)
	// S4 routing-pattern EARNING wire-up — the same miner, separate sink + flag.
	// DEFAULT ON (the default-on loop in Load; force-off'd with EconomyEnabled). ⚠ SO ON A
	// FRESH DEPLOYMENT AN OPTED-IN WORKSPACE'S SERVED REQUEST ROUTES INTO RecordPattern AND
	// CREDITS LENS. Only with the flag off is the serve path byte-identical to capture-only.
	p.SetPatternEarn(patternMiner, func() bool { return cfg.PatternEarningEnabled })

	r := chi.NewRouter()
	// U3 master economy kill-switch: the single chokepoint for economy-route
	// registration. econ.{get,post,del} register only when cfg.EconomyEnabled;
	// when off the routes are never mounted → chi-native 404 (#152). See
	// economy_routes.go.
	econ := econReg{on: cfg.EconomyEnabled}
	// The unbilled batch lane — off unless consciously enabled. See batch_routes.go.
	// false = no settle hook is wired anywhere in this binary (SetSettleHook has no production
	// caller). The gate refuses to open on the flag alone — see newBatchReg.
	batchGate := newBatchReg(cfg.BatchEnabled, false)
	// OTel HTTP middleware runs FIRST so every route — authenticated or
	// not — is traced and any incoming W3C traceparent header is extracted
	// into the request context before downstream middleware sees it.
	r.Use(otelhttp.NewMiddleware("talyvor-lens",
		otelhttp.WithTracerProvider(tp),
		otelhttp.WithPropagators(otel.GetTextMapPropagator()),
	))
	r.Use(middleware.RequestID)
	r.Use(clientIPMiddleware())
	r.Use(middleware.Recoverer)
	// B27.11: every request is in /healthz's "requests" section while it runs and for five minutes
	// after, by route template and phase; one past ten seconds is logged as it crosses and as it ends.
	requestTracker := reqtrack.New(10*time.Second, logger)
	r.Use(requestTracker.Middleware(r))
	r.Use(middleware.Timeout(60 * time.Second))

	// Production-API middlewares — must run *after* the chi RequestID
	// middleware so our X-Request-ID handler can honour an incoming
	// header without colliding with chi's internal request-id field.
	r.Use((&api.RequestIDMiddleware{}).Handler)
	r.Use(api.APIVersionMiddleware)
	r.Use(api.GzipMiddleware)
	r.Use(api.RateLimitHeadersMiddleware)
	// Security headers on every response: X-Content-Type-Options, X-Frame-Options,
	// Referrer-Policy, and Content-Security-Policy (tuned for the dashboard's
	// Google Fonts dependency; all JS/CSS is inline).
	r.Use(api.SecurityHeadersMiddleware)
	// CORS: no-op when LENS_CORS_ALLOWED_ORIGINS is unset (default).
	// Set to a comma-separated list of origins or "*" for public-API mode.
	// Preflight OPTIONS requests are absorbed here, before auth middleware.
	r.Use(api.CORSMiddleware(cfg.CORSAllowedOrigins))
	// HTTP request metrics (Upgrade 11): request count, latency histogram, and
	// in-flight gauge — labelled by chi route pattern (bounded cardinality).
	r.Use(metrics.HTTPMiddleware)

	// Detailed /healthz pings DB + Redis + the multi-endpoint local
	// router. Each checker has a 100ms budget so the rollup stays fast.
	healthHandler := api.NewHealthHandler(lensVersion, map[string]api.HealthChecker{
		"database": api.HealthCheckFunc(func(ctx context.Context) (bool, int64, string) {
			if pool == nil {
				return false, 0, "no pool configured"
			}
			start := time.Now()
			if err := pool.Ping(ctx); err != nil {
				return false, time.Since(start).Milliseconds(), err.Error()
			}
			return true, time.Since(start).Milliseconds(), ""
		}),
		"redis": api.HealthCheckFunc(func(ctx context.Context) (bool, int64, string) {
			if redisClient == nil {
				return false, 0, "no redis client"
			}
			start := time.Now()
			if err := redisClient.Ping(ctx).Err(); err != nil {
				return false, time.Since(start).Milliseconds(), err.Error()
			}
			return true, time.Since(start).Milliseconds(), ""
		}),
		"local_models": api.HealthCheckFunc(func(_ context.Context) (bool, int64, string) {
			eps := localRouterMulti.List()
			if len(eps) == 0 {
				return true, 0, ""
			}
			healthy := 0
			for _, e := range eps {
				if e.Healthy {
					healthy++
				}
			}
			if healthy == 0 {
				return false, 0, "0/" + strconv.Itoa(len(eps)) + " endpoints healthy"
			}
			if healthy < len(eps) {
				return true, 0, strconv.Itoa(len(eps)-healthy) + "/" + strconv.Itoa(len(eps)) + " endpoints unhealthy"
			}
			return true, 0, ""
		}),
		// U8/U9: replica reachability + replay lag. Healthy no-op when no
		// replica is configured (the feature is off, not broken).
		"read_replica": replicaLagMonitor,
	}).AddSection("database_pool", func(context.Context) any {
		if pool == nil {
			return nil
		}
		return reqtrack.PoolStats(pool)
	}).AddSection("requests", func(ctx context.Context) any {
		return requestTracker.Snapshot(ctx)
	}).ForOperator(func(r *http.Request) bool { return isAdmin(authManager, r) })
	r.Get("/healthz", healthHandler.ServeHTTP)

	// HA endpoints (Upgrade 7). The existing /healthz above is intentionally
	// left unchanged for backward-compat; these are additive:
	//   /livez     — pure liveness, always 200 while the process serves
	//   /readyz    — drain-aware readiness; 503 while draining or a dep is down
	//   /ha/status — cluster view (this instance + peers) for ops + dashboard
	r.Get("/livez", haComps.health.Live)
	r.Get("/readyz", haComps.health.Ready)
	// /ha/status exposes cluster peer list + instance health — admin-only
	// (ISO 27001 A.9). /livez and /readyz stay public for load balancers.
	r.Get("/ha/status", requireAdmin(authManager, http.HandlerFunc(haComps.health.Status)))

	// OpenAPI spec — public, no auth, no version path prefix.
	r.Get("/openapi.json", api.ServeOpenAPI)

	// Public read-only API endpoints — no auth required, but rate-limited to
	// prevent enumeration and DoS on the public economic surface.
	r.Group(func(pub chi.Router) {
		pub.Use(ratelimit.RateLimitMiddleware(rateLimiter))

		// B18.1: the token exchange (/v1/marketplace/listings*, /v1/marketplace/trades) is no longer
		// registered — see the note where its authenticated routes were.

		// Discovery: embedding nodes available for a model. UNAUTHENTICATED —
		// see public_node_discovery.go for what an anonymous caller may learn.
		pub.Get("/v1/embedding-nodes/available", newPublicAvailableEmbeddingNodesHandler(embeddingMiner))

		// Discovery: GPU nodes available for a model. UNAUTHENTICATED —
		// see public_node_discovery.go for what an anonymous caller may learn.
		pub.Get("/v1/nodes/available", newPublicAvailableNodesHandler(computeMiner))

		// JWKS — public EC P-256 verification key for the ES256 JWTs
		// Lens mints. External services fetch this once (or on cache miss)
		// to verify tokens without holding the private key.
		// Standard path per RFC 8414 discovery conventions.
		pub.Get("/v1/auth/jwks", func(w http.ResponseWriter, _ *http.Request) {
			pub := authManager.PublicKey()
			if pub == nil {
				writeJSONErr(w, http.StatusServiceUnavailable, "JWT signing not configured")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "public, max-age=3600")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"keys": []any{auth.PublicKeyToJWK(pub, auth.JWTKid)},
			})
		})

		// B30.5: Know Your Agent — the keys agent credentials are signed with, the revocation list, and verify.
		mountKYAPublicRoutes(pub, kyaSvc)
		// B45.23: Agent Cards — Talyvor's own, each agent's, their keys, and verify.
		mountAgentCardPublicRoutes(pub, agentCards)
		// B45.28: the key AP2 mandates and agents' approval evidence are signed with.
		mountAP2PublicRoutes(pub, mandateSigner)
		// B30.20: an invoice's pay link — the page, and its card button.
		mountInvoicePayRoutes(pub, dualToken, invoiceCards)
	})

	// /metrics exposes Prometheus telemetry — gated to admin key so internal
	// server stats are not visible to unauthenticated callers (ISO 27001 A.9).
	// Prometheus scrapers must send: Authorization: Bearer <LENS_API_KEY>.
	r.Handle("/metrics", requireAdmin(authManager, metrics.Handler()))

	// Keel (U25) drift findings — requireAdmin forensic read (a tenant must NEVER read another tenant's
	// drift attribution). Rows name only a self workspace + cohort aggregates; no counterparty raw value.
	// One keel reader feeds both the admin (all-workspace) and the tenant (self-scoped) findings reads.
	keelFindingsReader := keel.NewReader(dbrouting.ReadPool(pool, replicaPool))
	// ECONOMY FLAG READOUT — the LIVE value of every economy/minting flag as this process holds
	// it. requireAdmin because it describes the money path.
	//
	// `cfg` here is the same *config.Config every gate above reads, so this is observation, not
	// a re-read of the environment and not a default. If it could not be read the payload says
	// so and carries no values — see internal/econflags.
	r.Handle("/v1/admin/economy/flags", requireAdminOrOperatorRead(authManager,
		econflags.Handler(cfg, lensVersion, poolFlagOverride(cfg, poolGate),
			batchFlagOverride(cfg, batchGate))))

	r.Handle("/v1/admin/keel/findings", requireAdminOrOperatorRead(authManager,
		newKeelFindingsHandler(keelFindingsReader)))
	// B20.4 — marketplace safety: the admin's queue of held and reported listings, keeping one up, and
	// taking one down (which refunds its uses inside the holdback). market_handler.go.
	r.Get("/v1/admin/marketplace/review", requireAdminOrModerator(authManager, moderatorKeys, newMarketReviewQueueHandler(marketStore)))
	r.Post("/v1/admin/marketplace/listings/{listingID}/approve", requireAdminOrModerator(authManager, moderatorKeys, newMarketApproveHandler(marketStore)))
	r.Post("/v1/admin/marketplace/listings/{listingID}/takedown", requireAdminOrModerator(authManager, moderatorKeys, newMarketTakedownHandler(marketStore, marketRefunder)))
	// B32.47 — IP claims: the operator's queue, and the decision (upheld takes the listing down with its refunds,
	// attributed adds a claim edge and releases the holds, rejected releases them), recorded in the operator audit trail.
	r.Get("/v1/admin/marketplace/ip-claims", requireAdminOrModerator(authManager, moderatorKeys, newMarketIPClaimQueueHandler(marketStore)))
	r.Post("/v1/admin/marketplace/ip-claims/{claimID}/decide", requireAdminOrModerator(authManager, moderatorKeys, newMarketIPClaimDecideHandler(marketStore, marketRefunder)))
	// B32.50: the operator marks a public collection featured, listed first.
	r.Post("/v1/admin/marketplace/collections/{collectionID}/feature", requireAdminOrModerator(authManager, moderatorKeys, newMarketFeatureCollectionHandler(marketStore)))
	// B26.3 — the billed uses Stripe refused too often to keep retrying, with its reason. market_handler.go.
	r.Get("/v1/admin/marketplace/parked-uses", requireAdminOrOperatorRead(authManager, newMarketParkedUsesHandler(marketStore)))
	// B32.38 — the buyer tax profiles whose Stripe evidence contradicts the declared country. tax_profile_handler.go.
	r.Get("/v1/admin/tax-profiles/flagged", requireAdminOrOperatorRead(authManager, newTaxProfilesFlaggedHandler(taxProfiles)))
	// B30.6 — the sanctions lists and the compliance cases screening opened; an admin releases or refuses a held one.
	r.Get("/v1/admin/screening", requireAdminOrOperatorRead(authManager, newScreeningOverviewHandler(screeningLists, screener)))
	// B30.8: a decision is behind step-up, and an operator audit row.
	r.Post("/v1/admin/screening/cases/{caseID}/release", requireStepUp(authManager, stepUp, newScreeningDecideHandler(screener, true)))
	r.Post("/v1/admin/screening/cases/{caseID}/refuse", requireStepUp(authManager, stepUp, newScreeningDecideHandler(screener, false)))
	// B30.7 — the alerts transaction monitoring raised on its compliance cases, newest first.
	r.Get("/v1/admin/compliance/alerts", requireAdminOrOperatorRead(authManager, newMonitoringAlertsHandler(moneyMonitor)))
	// B30.11 — the daily reconciliation runs with their breaks, and the safeguarding view. reconciliation_handler.go.
	r.Get("/v1/admin/reconciliation", requireAdminOrOperatorRead(authManager, newReconciliationRunsHandler(dualToken)))
	r.Get("/v1/admin/safeguarding", requireAdminOrOperatorRead(authManager, newSafeguardingHandler(dualToken)))
	// B30.8 — the compliance case file: read with the admin key, every action behind step-up. compliance_handler.go.
	r.Get("/v1/admin/compliance/cases", requireAdmin(authManager, newComplianceCasesHandler(caseFile)))
	r.Get("/v1/admin/compliance/cases/{caseID}", requireAdmin(authManager, newComplianceCaseHandler(caseFile)))
	r.Post("/v1/admin/compliance/cases/{caseID}/notes", requireStepUp(authManager, stepUp, newComplianceActionHandler(caseFile, compliance.ActionNote)))
	r.Post("/v1/admin/compliance/cases/{caseID}/freeze", requireStepUp(authManager, stepUp, newComplianceActionHandler(caseFile, compliance.ActionFreeze)))
	r.Post("/v1/admin/compliance/cases/{caseID}/unfreeze", requireStepUp(authManager, stepUp, newComplianceActionHandler(caseFile, compliance.ActionUnfreeze)))
	r.Post("/v1/admin/compliance/cases/{caseID}/close", requireStepUp(authManager, stepUp, newComplianceActionHandler(caseFile, compliance.ActionClose)))
	r.Post("/v1/admin/compliance/cases/{caseID}/export", requireStepUp(authManager, stepUp, newComplianceExportHandler(caseFile)))
	// B32.44 — the annual platform-reporting export (UK reporting rules, EU DAC7): the file, with the sellers' TINs in
	// clear, and the runs recorded with their sha256 — for the global admin key only. platform_report_handler.go.
	platformReports := platformreport.New(pool, sellerTax)
	r.Post("/v1/admin/platform-reports", requireAdmin(authManager, newPlatformReportExportHandler(platformReports)))
	r.Get("/v1/admin/platform-reports", requireAdmin(authManager, newPlatformReportRunsHandler(platformReports)))
	// B27.19 — an operator retries one: the global key or a moderator key, recorded under the operator's name.
	r.Post("/v1/admin/marketplace/parked-uses/{useID}/retry", requireAdminOrModerator(authManager, moderatorKeys, newMarketParkedUseRetryHandler(marketStore)))
	// B32.10 — the operator puts a workspace on an Enterprise contract, with its fees, or ends one; each is
	// recorded in the operator audit trail in the same transaction.
	r.Get("/v1/admin/workspaces/{wsID}/contract", requireAdmin(authManager, newContractGetHandler(pool)))
	r.Put("/v1/admin/workspaces/{wsID}/contract", requireAdmin(authManager, newContractPutHandler(pool)))
	r.Delete("/v1/admin/workspaces/{wsID}/contract", requireAdmin(authManager, newContractDeleteHandler(pool)))
	// B39.9 — the operator gives one workspace its own request limits, or puts it back on the defaults.
	r.Get("/v1/admin/workspaces/{wsID}/rate-limits", requireAdmin(authManager, newRateLimitsGetHandler(workspaceRateLimits)))
	r.Put("/v1/admin/workspaces/{wsID}/rate-limits", requireAdmin(authManager, newRateLimitsPutHandler(workspaceRateLimits)))
	r.Delete("/v1/admin/workspaces/{wsID}/rate-limits", requireAdmin(authManager, newRateLimitsDeleteHandler(workspaceRateLimits)))
	// B27.28 — the operator audit trail: the web app records each operator action on its moderator key
	// (naming the operator), operators read it back filtered or as CSV. Append-only in the database (0182).
	// operator_audit_handler.go.
	operatorAudit := operatoraudit.NewStore(pool)
	r.Post("/v1/admin/operator-audit/record", requireAdminOrModerator(authManager, moderatorKeys, newOperatorAuditRecordHandler(operatorAudit)))
	r.Get("/v1/admin/operator-audit", requireAdminOrOperatorRead(authManager, newOperatorAuditListHandler(operatorAudit)))
	r.Get("/v1/admin/operator-audit/export", requireAdminOrOperatorRead(authManager, newOperatorAuditExportHandler(operatorAudit)))
	// B44.6 — every talyvor.com waitlist signup, oldest first, for the operator.
	r.Get("/v1/admin/waitlist", requireAdminOrOperatorRead(authManager, newWaitlistListHandler(pool)))
	// KE-2 observability — every APPLIED drift haircut (default-on in closed-test). Reads the PRIMARY pool
	// (non-money read of ledger metadata + keel_findings; keeps the U8/U9 ExactlySix replica-reader invariant
	// unchanged).
	r.Handle("/v1/admin/keel/haircuts", requireAdminOrOperatorRead(authManager,
		newKeelHaircutsHandler(haircutobs.NewReader(pool), time.Now)))

	// K4 output verdicts — admin forensic read (all workspaces), requireAdmin-gated. Reads the PRIMARY
	// pool (non-money read; keeps the U8/U9 ExactlySix replica-reader invariant unchanged).
	r.Handle("/v1/admin/output-verdicts", requireAdminOrOperatorRead(authManager,
		newOutputVerdictsAdminHandler(outputVerdictReader)))
	// Full tenant roster — admin forensic/ops read (requireAdmin), sibling of the admin reads here.
	// Backed by the in-memory workspace.Manager (no DB read; U8/U9 replica-reader invariant unchanged).
	r.Handle("/v1/admin/workspaces", requireAdminOrOperatorRead(authManager,
		newAdminListWorkspacesHandler(wsManager)))
	// KEEL ECONOMY go/no-go: the routing-decision summary (override rate + estimated cost delta). Admin-only,
	// descriptive; the estimate is NOT money.
	r.Handle("/v1/admin/routing-decisions/summary", requireAdminOrOperatorRead(authManager,
		newRoutingDecisionsSummaryHandler(routeDecisionReader, time.Now)))
	// B17.7 — the operator's usage and spend totals across every workspace, synthetic workspaces apart.
	// Reads the PRIMARY pool (non-money read; keeps the U8/U9 ExactlySix replica-reader invariant unchanged).
	r.Handle("/v1/admin/usage/summary", requireAdminOrOperatorRead(authManager,
		newAdminUsageSummaryHandler(opsusage.NewReader(pool), time.Now)))
	// B18.17 — the operator screen's per-workspace columns: spend, LENS held, last activity. Same pool and gate.
	opsReader := opsusage.NewReader(pool)
	r.Handle("/v1/admin/workspaces/spend", requireAdminOrOperatorRead(authManager,
		newOperatorWorkspaceReadHandler(opsReader.SpendByWorkspace)))
	r.Handle("/v1/admin/workspaces/held", requireAdminOrOperatorRead(authManager,
		newOperatorWorkspaceReadHandler(opsReader.HeldByWorkspace)))
	r.Handle("/v1/admin/workspaces/last-activity", requireAdminOrOperatorRead(authManager,
		newOperatorWorkspaceReadHandler(opsReader.ActivityByWorkspace)))

	// H5.β — settle a provenance bond (slash-or-release; deadline-guarded + CAS-safe + idempotent).
	// requireAdmin as defense-in-depth; registered ONLY when the flag is on.
	if cfg.H5BondsEnabled {
		r.Handle("/v1/admin/bonds/{bond_id}/settle", requireAdmin(authManager,
			newBondSettleHandler(bondManager)))
	}
	// STEP 3 — reproduce-before-burn: Talyvor's own sandboxed compile of a bonded output. ADMIN-ONLY; a
	// workspace can never reach it. Registered only when LENS_H5_ATTEST_ENABLED.
	if cfg.H5AttestEnabled {
		r.Handle("/v1/admin/attest/{output_id}", requireAdmin(authManager, newAttestHandler(attestor)))
	}

	apiServer := api.NewServer(
		pool, redisClient, nc, exactCache, l,
		alertManager, branchTracker, wsManager, lr,
		anomalyDetector,
		lensVersion,
	)
	// Budgets store powers the dashboard's read-only Budgets panel.
	apiServer.SetBudgetStore(budgetStore)
	// Forecaster powers the dashboard's projection columns.
	apiServer.SetForecaster(forecaster)
	// Cost-anomaly detector powers the dashboard's Cost outliers panel.
	apiServer.SetCostAnomalyDetector(costAnomalyDetector)
	// ROI reporter powers the dashboard's Executive summary panel.
	apiServer.SetROIReporter(roiReporter)
	// Routing advisor powers the dashboard's Routing intelligence panel.
	apiServer.SetRoutingAdvisor(routingAdvisor)
	// Guardrails engine powers the dashboard's Guardrails panel.
	apiServer.SetGuardrailsEngine(guardrailsEngine)
	apiServer.SetEvalPipeline(evalPipeline)
	apiServer.SetPOVIStakeManager(poviStakeManager)
	apiServer.SetPOVIChallengeStore(challengeStore)
	apiServer.SetEconomyEnabled(cfg.EconomyEnabled) // U3: gate the economy dashboard-API routes
	// Public: health probe and Prometheus passthrough never require a key.
	apiServer.MountUnauthenticated(r)

	// SEC: /mcp exposes per-workspace financial telemetry (spend, cache, sessions) via tools that read a
	// workspace_id argument — so it MUST require a verified credential. It was previously mounted bare
	// ("MCP clients bring their own auth"), letting any unauthenticated caller read any workspace's
	// financials by naming it. Gate it with the same AuthMiddleware as the authed group; the tools
	// additionally force the acted-on workspace to the verified caller (effectiveWorkspace).
	mcpServer := mcp.New(pool, l, alertManager, wsManager, sessionTracker, lensVersion)
	mcpServer.SetAgentBank(dualToken)       // B19.9: agents use their wallets with their own keys
	mcpServer.SetCredentials(kyaSvc)        // B30.5: and show their Know Your Agent credential
	mcpServer.SetMandates(dualToken)        // B40.2: get_mandates
	mcpServer.SetPaymentRequests(dualToken) // B40.3: prepare_payment, prepare_batch, request_marketplace_purchase, cancel_request, get_request_status; B41.102: prepare_bills_payment; B45.17: list_bills_due
	// B32.23: and shop the marketplace with them — search, license, use within a max price, cancel.
	mcpServer.SetMarket(marketStore, mcpMarketDeps{lens: r, meter: marketMeter, agents: dualToken})
	// B19.16: approvals signed with the owner's passkey; a web push per approval filed when a VAPID key is set.
	var approvalPusher economy.ApprovalPusher
	if cfg.VAPIDPrivateKey != "" {
		sender, err := webpush.NewSender(cfg.VAPIDPrivateKey, cfg.VAPIDSubject)
		if err != nil {
			return fmt.Errorf("LENS_VAPID_PRIVATE_KEY: %w", err)
		}
		approvalPusher = sender
	}
	dualToken.SetApprovalAuth(passkey.RelyingParty{ID: cfg.WebAuthnRPID, Origins: cfg.WebAuthnOrigins}, approvalPusher)
	dualToken.SetDeviceKeys(devicekey.Verifier{AppleAppID: cfg.DeviceAppleAppID, AllowTest: cfg.DeviceTestAttestation}) // B40.4
	// B40.15: and a native push to each phone with a push token, for whichever of APNs and FCM is configured.
	var native nativepush.Senders
	if cfg.APNsKey != "" {
		key, err := nativepush.Secret(cfg.APNsKey)
		if err == nil {
			host := nativepush.APNsProduction
			if cfg.APNsSandbox {
				host = nativepush.APNsSandbox
			}
			native.APNs, err = nativepush.NewAPNs(key, cfg.APNsKeyID, cfg.APNsTeamID, cfg.APNsTopic, host)
		}
		if err != nil {
			return fmt.Errorf("LENS_APNS_KEY: %w", err)
		}
	}
	if cfg.FCMCredentials != "" {
		creds, err := nativepush.Secret(cfg.FCMCredentials)
		if err == nil {
			native.FCM, err = nativepush.NewFCM(creds)
		}
		if err != nil {
			return fmt.Errorf("LENS_FCM_CREDENTIALS: %w", err)
		}
	}
	if native.APNs != nil || native.FCM != nil {
		dualToken.SetNativePush(native)
	}
	// B19.11: an agent's verified badge follows its owner's workspace's verification (the U6 predicate).
	dualToken.SetOwnerVerifier(earnverify.New(cfg.EarnRequireLivePurchase))
	// B22.9: cash-outs are paid through the partner-neutral interface; only the test partner exists, and it pays
	// nothing — the real one waits for a licensed partner.
	dualToken.SetCashOutPartner(economy.TestCashOutPartner{})
	mcpAuth := auth.AuthMiddleware(keyStore, authManager)
	r.With(mcpAuth).Post("/mcp", mcpServer.HandleRPC)
	r.With(mcpAuth).Get("/mcp/sse", mcpServer.HandleSSE)

	// THE BROWSER SURFACE. This used to be a 41-panel dashboard; it is now one
	// static status page, and the reason is in internal/dashboard/handler.go: ten
	// of its thirteen data panels fetched AUTHENTICATED endpoints with no
	// credential attached and no way to supply one, so on the deployed host they
	// answered 401 and rendered em-dashes. The comment that used to sit here —
	// "the live numbers come from /v1/api/* XHRs that the browser sends with the
	// user's own API key" — described a mechanism that did not exist.
	//
	// GET / is registered UNCONDITIONALLY. A visitor who types the API hostname
	// must never be met with nothing: a bare 404 is honest but useless, and a
	// redirect to app.talyvor.com assumes a suite deployment a self-hoster may
	// not have. The page is static, holds no credential, reads no database and
	// shows no per-workspace data — there is nothing about it worth gating. (It
	// no longer 302s to /dashboard: with the dashboard default-off that would
	// bounce every visitor into a 404.)
	//
	// GET /dashboard is gated on cfg.DashboardEnabled (default FALSE) via dashReg,
	// the same never-registered shape as billReg. It serves the same page, so an
	// existing link to /dashboard lands somewhere honest on deployments that
	// still want that path.
	dashHandler := dashboard.New(lensVersion)
	r.Get("/", dashHandler.ServeHTTP)
	dashReg{on: cfg.DashboardEnabled}.get(r, "/dashboard", dashHandler.ServeHTTP)

	// Public conversion-rate endpoints — no auth, no PII, but rate-limited
	// to prevent scraping / DoS.
	r.Group(func(pub chi.Router) {
		pub.Use(ratelimit.RateLimitMiddleware(rateLimiter))

		// Public conversion-rate surface (two-token split). The rate +
		// its full derivation are public economic signal; no auth.
		econ.get(pub, "/v1/economy/conversion-rate", func(w http.ResponseWriter, req *http.Request) {
			rate, err := rateEngine.CurrentRate(req.Context())
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]any{
				"rate":         rate,
				"usd_per_lxc":  economy.LXCUSDValue,
				"lens_per_lxc": rate,
			})
		})

		econ.get(pub, "/v1/economy/conversion-rate/history", func(w http.ResponseWriter, req *http.Request) {
			limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
			hist, err := rateEngine.RateHistory(req.Context(), limit)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, hist)
		})

		// B28.439 — each plan's price and included usage, for a visitor: what a new subscriber is granted.
		subs.get(pub, "/v1/billing/plans", newPlansHandler(billRoute, enterpriseFromUSDCents))

		// B32.8 — every fee Talyvor charges, for /pricing.
		pub.Get("/v1/public/fees", publicFeesHandler)

		// B32.12 — what each plan unlocks, for /pricing.
		pub.Get("/v1/public/plan-gates", publicPlanGatesHandler)
	})

	// Public status page. /status content-negotiates between HTML and
	// JSON; /status.json is the unconditional-JSON convenience route
	// uptime monitors and CI scripts use.
	r.Get("/status", statusPage.ServeHTTP)
	r.Get("/status.json", statusPage.ServeJSON)

	// Everything else sits behind the API-key middleware. chi.Group inherits
	// middleware only for routes registered inside its closure.
	heliconeCompat := compat.NewHeliconeCompat(keyStore)

	// U18b billing webhook — PUBLIC (Stripe-signed, no auth), gated on
	// BillingEnabled (unregistered ⇒ 404). Registered on the bare router with no
	// auth/rate-limit middleware; the handler reads the RAW body itself (Stripe
	// signs raw bytes) before any JSON.
	bill.post(r, "/v1/billing/webhook", billingSvc.HandleWebhook)
	// B25.2 — the Stripe test-mode endpoint, signed with its own secret; it credits test workspaces only.
	if testBillingSvc != nil {
		r.Post("/v1/billing/webhook/test", testBillingSvc.HandleWebhook)
	}

	// B19.12 — Stripe's real-time authorisation of every agent card purchase — PUBLIC (Stripe-signed with
	// its own secret, no auth), on the bare router like the billing webhook. Unset secret ⇒ unregistered.
	if cfg.StripeIssuingWebhookSecret != "" {
		r.Post("/v1/agent-cards/authorizations", agentcard.NewHandler(cfg.StripeIssuingWebhookSecret, dualToken, ecbRates).ServeHTTP)
	}

	// B46.3 — each money partner's webhooks — PUBLIC (signed by the partner, no auth), on the bare router like the billing
	// webhook; the partner's adapter verifies the signature and a replayed id is refused.
	r.Post("/v1/money/webhooks/{partner}", moneyLayer.ServeWebhook)

	// B25.7 and B32.99 — the synthetic routes' slow money (mounted below with the synthetic routes).
	syntheticDue := syntheticDueDeps{db: pool, loans: dualToken, bills: marketStore, payouts: marketStore, sellerTax: sellerTax, cards: dualToken,
		authorizer: agentcard.NewHandler(cfg.StripeIssuingWebhookSecret, dualToken, ecbRates)}

	// U-signup provisioning — turns an authenticated identity into a tenant. Mounted on the BARE
	// router, OUTSIDE the authed group, because it carries its own credential
	// (LENS_PROVISION_SECRET) rather than an API key: its whole purpose is to serve a caller that
	// does not yet have a workspace, and therefore cannot have a workspace key.
	//
	// FAIL CLOSED: mountProvisionRoute registers nothing when the secret is unset, so an
	// unconfigured deployment 404s here instead of exposing an unauthenticated provisioning
	// surface. That is the reason the emptiness check lives inside the mount helper and not in
	// the handler — a handler-level check would still leave the route reachable.
	//
	// AND SAY SO WHEN IT IS OFF. Failing closed and failing silently are the same thing when
	// nobody looks: a Lens paired with a suite BFF that expects provisioning boots clean, passes
	// its healthcheck, serves every other route, and 404s every login with nothing anywhere
	// explaining it. Deliberately a WARN and not a boot refusal — most deployments run Lens with
	// no BFF and never want provisioning, so a hard requirement would break them for a capability
	// they never asked for. Deliberately not a healthcheck failure either: healthchecks drive
	// restarts and load-balancer removal, and "an optional capability is off" is not "this process
	// cannot serve traffic". A boot log is the surface a person is actually reading at the one
	// moment this matters — during the deploy.
	if cfg.ProvisionSecret == "" {
		logger.Warn("provisioning DISABLED: LENS_PROVISION_SECRET is unset, so POST /v1/provision is not registered",
			slog.String("effect", "any gateway that provisions per-user workspaces will get 404 on every login"),
			slog.String("remedy", "set LENS_PROVISION_SECRET here and to the SAME value on the gateway; leave unset if this deployment has no such gateway"))
	}
	// B17.1 — synthetic test workspaces, only when LENS_SYNTHETIC_KEY is set.
	syntheticAnswers := storedanswers.New(pool, redisClient)
	mountSyntheticRoutes(r, cfg.SyntheticKey, syntheticDeps{
		workspaces: wsManager,
		credits:    dualToken,
		answers:    syntheticAnswers,
		audit:      pool,
		mint: func(workspaceID, userID string, scopes []string, ttl time.Duration) (string, error) {
			return auth.GenerateToken(workspaceID, userID, scopes, authManager.PrivateKey(), ttl)
		},
		// B35.1 — a test workspace on any plan.
		plans: pool,
		// B25.7 — a test user's loan instalment, bill, refund and card purchase brought due now.
		due: syntheticDue,
	})
	mountProvisionRoute(r, cfg.ProvisionSecret, wsManager,
		newWorkspaceStarter(dualToken, cfg.StarterTestMoneyULXC),
		func(workspaceID, userID string, scopes []string, ttl time.Duration) (string, error) {
			return auth.GenerateToken(workspaceID, userID, scopes, authManager.PrivateKey(), ttl)
		})
	// B44.6 — the talyvor.com waitlist, written by the suite BFF on the same secret.
	mountWaitlistRoute(r, cfg.ProvisionSecret, pool)
	// B26.1 — synthetic workspaces older than seven days are deleted, by the leader alone.
	if cfg.SyntheticKey != "" {
		go haComps.leader.Run(ctx, "synthetic-purge", 30*time.Second, func(lctx context.Context) {
			runSyntheticPurge(lctx, wsManager, syntheticAnswers)
		})
	}
	// B30.19 — a mandate's payee pulls: an outside business with its mandate's key, a Talyvor company with its own,
	// through the authed group's auth and rate limit.
	mountMandatePullRoute(r, dualToken, func(h http.Handler) http.Handler {
		return auth.AuthMiddleware(keyStore, authManager)(ratelimit.RateLimitMiddleware(rateLimiter)(h))
	})

	r.Group(func(authed chi.Router) {
		// Helicone-compat translates Helicone-* headers and rewrites
		// /oai/* and /anthropic/* paths BEFORE auth runs. That order
		// matters: Helicone-Auth becomes Authorization, which is what
		// AuthMiddleware then validates.
		authed.Use(heliconeCompat.Middleware())
		// Auth must run before the rate-limiter so the limiter sees the
		// key/workspace that AuthMiddleware just stamped onto the request.
		authed.Use(auth.AuthMiddleware(keyStore, authManager))
		authed.Use(ratelimit.RateLimitMiddleware(rateLimiter))
		// Tenant isolation: a /v1/workspaces/{wsID}/... route may only be driven
		// by a credential belonging to that workspace (the global admin key
		// bypasses). Registered after AuthMiddleware so the resolved identity is
		// already on the request context. Closes a cross-tenant BOLA/IDOR where
		// any valid workspace key could act on any {wsID} in the path — read or
		// rewrite another tenant's config/budgets and mint API keys for it.
		authed.Use(workspaceIsolationMiddleware)

		apiServer.MountAuthenticated(authed)

		// Proxy scope: the LLM serving surface (every /v1/proxy/*, /oai/*,
		// /anthropic/* route) requires the "proxy" scope. RequireScope
		// grandfathers empty-scope credentials and fast-path api_keys (which
		// have no scope field), so no existing key loses access — only a key
		// created with an explicit, non-proxy scope set (e.g. scopes:["analytics"])
		// is refused here (403). Admin (the global key) bypasses via IsAdmin.
		proxyScope := auth.RequireScope(auth.ScopeProxy)
		// B28.186: a Chat request naming a marketplace listing (X-Talyvor-Listing) is asked through it, as one use of it.
		// Wrapping the handler, not a With(...): the route censuses read each registration's With(proxyScope) as written.
		listingOpenAI := chatListing(marketStore, marketMeter, dualToken, chatShapeOpenAI)
		listingAnthropic := chatListing(marketStore, marketMeter, dualToken, chatShapeAnthropic)
		listingNone := chatListing(marketStore, marketMeter, dualToken, "")

		authed.With(proxyScope).Post("/v1/proxy/openai/*", listingOpenAI(p.HandleOpenAI))
		authed.With(proxyScope).Post("/v1/proxy/anthropic/*", listingAnthropic(p.HandleAnthropic))
		authed.With(proxyScope).Post("/v1/proxy/google/*", listingNone(p.HandleGoogle))
		authed.With(proxyScope).Post("/v1/proxy/bedrock/*", listingNone(p.HandleBedrock))
		authed.With(proxyScope).Post("/v1/documents", documents.UploadHandler(docStore)) // B18.13
		authed.With(proxyScope).Post("/v1/audio/transcriptions", p.HandleTranscription)  // B40.10
		authed.With(proxyScope).Post("/v1/audio/speech", p.HandleSpeech)                 // B40.10
		// K4 output verdicts — WORKSPACE-SCOPED read: a tenant sees ONLY its OWN verdicts (scoped to the
		// authenticated WorkspaceID; never another's). Intra-tenant by construction.
		authed.Get("/v1/output-verdicts", newOutputVerdictsWorkspaceHandler(authManager, outputVerdictReader))
		// Keel drift findings — WORKSPACE-SCOPED read: a tenant sees ONLY its OWN drift attribution (scoped to
		// the authenticated WorkspaceID; a caller-supplied workspace_id is ignored). Gives the feature a user.
		authed.Get("/v1/keel/findings", newKeelFindingsWorkspaceHandler(authManager, keelFindingsReader))
		// K4 CODE LOOP — the producing workspace self-reports a MECHANICAL verdict (compiled/tests) for an
		// output it produced. Ownership-bound (only the producer, per k4_output_verdicts) + append-only.
		authed.Post("/v1/output-verdicts/{output_id}/mechanical", newMechanicalVerdictHandler(authManager, mechanicalVerdictWriter))
		// K4 ATTRIBUTION — the producing workspace attributes an output it OWNS to a PR or spec
		// (opaque target_ref). Ownership-bound (only the producer, per k4_output_verdicts) + append-only,
		// first-wins per kind (409 on a conflicting re-attribution). Attribution ≠ settlement: no amount.
		authed.Post("/v1/outputs/{output_id}/attribution", newAttributionHandler(authManager, attributionWriter))
		// Attribution READ-BACK (self-scoped; NOT flag-gated, matching the write sibling above). By-output is
		// owner-scoped (a foreign output → 404, no oracle); /v1/attributions lists the caller's own.
		authed.Get("/v1/outputs/{output_id}/attribution", newAttributionByOutputHandler(authManager, attributionReader))
		authed.Get("/v1/attributions", newAttributionListHandler(authManager, attributionReader))
		// H5 OPT-IN buildable-artifact commitment — the producing workspace commits, bound to an output it
		// produced, the manifest hash of its buildable module (output slot folded from the captured
		// output_content_sha256 — the canonical served content a buildable tree can actually carry).
		// Owner-bound + append-once. Registered ONLY when LENS_H5_ARTIFACT_ENABLED; until then no
		// artifact_sha256 is ever populated and the attestor's opt-in binding stays dormant.
		if cfg.H5ArtifactEnabled {
			authed.Post("/v1/outputs/{output_id}/artifact", newArtifactCommitHandler(authManager, outputverify.NewArtifactCommitter(pool)))
		}
		// H5.β — the workspace stakes a provenance bond on an output it produced (locks µLENS collateral).
		// Scoped to the caller's workspace; registered ONLY when LENS_H5_BONDS_ENABLED.
		if cfg.H5BondsEnabled {
			authed.Post("/v1/bonds", newBondCreateHandler(authManager, bondManager))
		}
		// Bond READ-BACK (list + owner-scoped get) — gated on the SAME flag as the write siblings, so
		// flag-off ⇒ the routes are never registered (chi 404).
		registerBondReadRoutes(authed, cfg.H5BondsEnabled, authManager, bondReader)
		authed.With(proxyScope).Post("/v1/proxy/mistral/*", listingOpenAI(p.HandleExtraProvider("mistral")))
		authed.With(proxyScope).Post("/v1/proxy/groq/*", listingOpenAI(p.HandleExtraProvider("groq")))
		authed.With(proxyScope).Post("/v1/proxy/vllm/*", listingOpenAI(p.HandleExtraProvider("vllm")))

		// Helicone-shape URL prefixes. First-class routes (not a
		// deprecated alias) — migrating teams can keep these URLs
		// indefinitely. The compat middleware above strips the
		// Helicone-Auth / Helicone-Property-* headers before the
		// proxy handler runs.
		authed.With(proxyScope).Post("/oai/*", listingOpenAI(p.HandleOpenAI))
		authed.With(proxyScope).Post("/anthropic/*", listingAnthropic(p.HandleAnthropic))

		authed.Post("/v1/api/keys", newCreateAPIKeyHandler(keyStore))

		authed.Post("/v1/api/injection/patterns", requireAdmin(authManager, http.HandlerFunc(newInjectionPatternAddHandler(injectionDetector))))

		authed.Delete("/v1/api/keys/{keyID}", newRevokeAPIKeyHandler(keyStore))

		// ─── JWT auth endpoints ────────────────────────
		// /v1/auth/token mints a JWT — admin-only because issuing
		// a token for an arbitrary workspace/user is a privileged
		// op. /refresh accepts any valid JWT and extends it.
		// /me echoes the resolved AuthContext for the caller.
		// Extracted to newAuthTokenMintHandler (auth_token_mint_handler.go) so the
		// admin-scoping invariant is provable over HTTP; behavior is unchanged.
		authed.Post("/v1/auth/token", newAuthTokenMintHandler(authManager))

		// W4.6.1 step 4 — session-scoped keys. Registered ONLY when configured, so an
		// unconfigured deployment 404s these paths rather than serving a route that refuses.
		if sessionKeyStore != nil {
			mountSessionKeyRoutes(authed, sessionKeyStore, cfg.SessionKeyTTL)
		}

		// Admin-only: approve the LENS->LXC conversion rate. The
		// admin can ONLY approve the algorithm's output (within
		// band + floor) — there is no way to set an arbitrary rate.
		// The full computation (all inputs) is returned + persisted.
		econ.post(authed, "/v1/admin/conversion-rate/approve", func(w http.ResponseWriter, req *http.Request) {
			actx, err := authManager.Authenticate(req)
			if err != nil || !actx.IsAdmin {
				writeJSONErr(w, http.StatusForbidden, "admin credentials required")
				return
			}
			approvedBy := actx.UserID
			if approvedBy == "" {
				approvedBy = "global_key"
			}
			comp, err := rateEngine.ApproveRate(req.Context(), approvedBy)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, comp)
		})

		// Admin-only DISTILL preview: a DRY RUN that converts an uploaded
		// document to Markdown through the KILLABLE subprocess (ProcessIsolator,
		// using the proven-sufficient 512 MiB default) — never the in-process
		// path. No model call, no token_events, no spend. A text-less/scanned
		// doc returns needs_vision honestly (no vision model runs here).
		distillPreview := &distillpreview.Handler{
			Converter: &distill.ProcessIsolator{WorkerBin: cfg.DistillWorkerBin},
			IsAdmin: func(req *http.Request) bool {
				// Mirror the existing admin route's error-first, fail-closed check
				// (deny on auth error OR non-admin) exactly.
				actx, err := authManager.Authenticate(req)
				if err != nil || !actx.IsAdmin {
					return false
				}
				return true
			},
		}
		authed.Post("/v1/admin/distill/preview", distillPreview.ServeHTTP)

		// B11.4 — the Try-it previews, workspace-scoped: Tare on a paste, conversion on an upload. No model
		// call, no charge, no token_events row.
		mountPreviewRoutes(authed, &distill.ProcessIsolator{WorkerBin: cfg.DistillWorkerBin}, func(wsID string) *tare.Reducer {
			if !wsManager.GetTareModel(wsID) {
				return nil
			}
			return &tare.Reducer{Kind: tare.KindProse, New: tareModel.Reduction}
		})

		// B39.3: the token-economy reads need a credential; the two that pool
		// across workspaces (routing insights, the oracle queue) need the admin key.
		// Token earning rates.
		econ.get(authed, "/v1/tokens/rates", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				// "cache" retired (Phase-4a Item 0 — superseded by pool-royalty).
				"compute": map[string]any{
					"base_per_1k_tokens_ulens": mining.ComputeMineBaseRate, // µLENS (SEC-2)
					"multiplier_cpu":           mining.GPUMultiplierCPU,
					"multiplier_rtx4090":       mining.GPUMultiplierRTX4090,
					"multiplier_a100":          mining.GPUMultiplierA100,
					"multiplier_h100":          mining.GPUMultiplierH100,
				},
				"embedding":  mining.EmbeddingRates(),
				"annotation": mining.AnnotationRates(),
				"pattern":    mining.PatternRates(),
			})
		})

		// Economy stats (Batch 3 Phase 1).
		econ.get(authed, "/v1/economy/stats", func(w http.ResponseWriter, req *http.Request) {
			stats, err := marketplace.GetEconomyStats(req.Context())
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		// Aggregated routing patterns across opted-in workspaces.
		econ.get(authed, "/v1/insights/routing", requireAdmin(authManager, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			q := req.URL.Query()
			insights, err := patternMiner.GetInsights(req.Context(),
				q.Get("model"), q.Get("provider"), q.Get("feature"))
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, insights)
		})))

		econ.get(authed, "/v1/oracle/stats", requireAdmin(authManager, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			stats, err := oracleEngine.GetOracleStats(req.Context())
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})))

		// ADMIN-ONLY distill attribution read (S1 read-surface commitment).
		// requireAdmin-gated: content_hash + counterparty workspace ids are
		// exposed here and must never be tenant-reachable. Default returns raw
		// rows; ?view=pairs returns the condition-(b) materiality aggregate. The
		// Reader is Query-only (no write capability — see distillattrib.Reader).
		econ.get(authed, "/v1/admin/distill/attribution",
			requireAdminOrOperatorRead(authManager, http.HandlerFunc(newDistillAttributionAdminHandler(distillattrib.NewReader(dbrouting.ReadPool(pool, replicaPool))))))

		// Stage-3 pool-mint adjudication gate — the Revoker's FIRST and ONLY
		// production caller. Admin-gated (mirrors ApproveRate); the operator
		// passes an explicitly-chosen subset of held request_ids to revoke. The
		// writer records the decision BEFORE the burn (record-before-revoke), so
		// no production revoke can happen without a preceding audit row. Doubly
		// inert in the current config: needs an admin AND
		// LENS_POOL_ROYALTY_MINTING_ENABLED (no held rows exist otherwise). Never
		// a loop — operator-initiated only.
		royaltyRevoker := poolroyalty.NewRevoker(pool, tokenLedger)
		royaltyAdjudicator := poolroyalty.NewAdjudicationWriter(pool, royaltyRevoker)
		econ.post(authed, "/v1/admin/pool-royalty/adjudicate", newAdjudicateHandler(authManager, royaltyAdjudicator))

		// PR3 distill anti-gaming — the SAME record-before-revoke gate over the
		// distill_royalty_mints / distill_royalty_adjudications tables (the Revoker +
		// AdjudicationWriter parameterized by table; RevokeHeldTx is already generic).
		// Same admin gate, same inertness (held distill rows exist only under the mint flag).
		distillRoyaltyRevoker := poolroyalty.NewRevokerForTable(pool, tokenLedger, "distill_royalty_mints")
		distillRoyaltyAdjudicator := poolroyalty.NewAdjudicationWriterForTable(pool, distillRoyaltyRevoker, "distill_royalty_adjudications")
		econ.post(authed, "/v1/admin/distill-royalty/adjudicate", newAdjudicateHandler(authManager, distillRoyaltyAdjudicator))

		// Phase-3 Item 2: manual clawback for the FOUR single-party held tables Phase-2
		// left unreachable (eval-contribution / routing-prediction / node-latency /
		// confidential-compute). The generic Revoker reaches them unchanged (proven on
		// the real DOUBLE-PRECISION schema in held_clawback_realschema_integration_test)
		// — the gap was purely that none was constructed. Same admin-gated, record-
		// before-revoke AdjudicationWriter, over the shared held_mint_adjudications
		// audit (0091, target table named in flag_type). Doubly inert today (needs an
		// admin AND the P-o-I mint rate>0 — no held rows exist otherwise).
		for _, mt := range []string{"eval_contribution_mints", "routing_prediction_mints", "node_latency_mints", "confidential_compute_mints"} {
			adj := poolroyalty.NewAdjudicationWriterForTable(pool, poolroyalty.NewRevokerForTable(pool, tokenLedger, mt), "held_mint_adjudications")
			econ.post(authed, "/v1/admin/held-mints/"+mt+"/adjudicate", newAdjudicateHandler(authManager, adj))
		}

		// Phase-3 Item 2: manual clawback for traffic_mint_holds (the cache/compute/
		// embedding NODE mints). The generic Revoker is column-incompatible here
		// (composite key + workspace_id), so this uses the purpose-built TrafficRevoker
		// (mining), record-before-revoke over the same shared audit. Keys are composite
		// "request_id|workspace_id|mint_type". Admin-gated; inert until the node network
		// produces held rows.
		trafficRevoker := mining.NewTrafficRevoker(pool, tokenLedger)
		econ.post(authed, "/v1/admin/held-mints/traffic/adjudicate", newTrafficRevokeHandler(authManager, pool, trafficRevoker))

		// Pool-B royalty OBSERVABILITY (read-only, admin-gated, NOT economy-gated).
		// Forensic infra must survive the kill-switch — the economy is likeliest OFF
		// during a security event, exactly when detection is needed. The readers use
		// Query-only db seams (no Exec/Begin reachable): NO mint/burn/balance/held row
		// is touched, and the adjudicate/clawback path above is untouched. requireAdmin
		// → 401 (the existing admin-reader gate); registered on `authed` directly (NOT
		// econ.get), so they resolve regardless of LENS_ECONOMY_ENABLED. Only the
		// MUTATION endpoint (adjudicate, above) stays economy-gated.
		poolRoyaltyDetector := poolroyalty.NewDetectorReader(pool, thresholdsFromConfig(cfg))
		poolRoyaltyResolver := poolroyalty.NewResolver(pool)
		poolRoyaltyMargin := poolroyalty.NewMarginReader(pool)
		authed.Get("/v1/admin/pool-royalty/detect", requireAdminOrOperatorRead(authManager, http.HandlerFunc(newPoolRoyaltyDetectHandler(poolRoyaltyDetector))))
		authed.Get("/v1/admin/pool-royalty/resolve", requireAdminOrOperatorRead(authManager, http.HandlerFunc(newPoolRoyaltyResolveHandler(poolRoyaltyResolver))))
		authed.Get("/v1/admin/pool-royalty/margin", requireAdminOrOperatorRead(authManager, http.HandlerFunc(newPoolRoyaltyMarginHandler(poolRoyaltyMargin))))

		// WorkTier ANALYTICS (read-only, admin-gated, NOT economy-gated) — surfaces ONE
		// workspace's descriptive tier distribution from worktier.Store.Aggregate (Query-only;
		// no Exec/Begin reachable, no mint path). workspace_id is REQUIRED (no cross-tenant
		// mode). Registered on `authed` (NOT econ.get) to match the WorkTier capture's
		// kill-switch-EXEMPT posture — the read survives LENS_ECONOMY_ENABLED, like the capture
		// it surfaces. MONEY-DECOUPLED: WorkTier never feeds mint/earn/billing.
		authed.Get("/v1/admin/worktier/distribution", requireAdminOrOperatorRead(authManager, http.HandlerFunc(newWorkTierDistributionHandler(worktierStore))))

		// Annotation reputation ADMIN RE-ENTRY (PR2) — un-bench a sub-floor annotator by appending
		// an admin_reset event (append-only; restores to baseline). Admin-gated (requireAdmin →
		// 401), NOT economy-gated (an operator action, like the observability reads). MONEY-DECOUPLED:
		// it moves a reputation score, never the ledger. reputationStore is the same one the
		// resolution job uses.
		authed.Post("/v1/admin/annotation-reputation/reset", requireAdmin(authManager, http.HandlerFunc(newReputationResetHandler(authManager, reputationStore))))

		// Distill royalty OBSERVABILITY — the distill mirror of the Pool-B block above,
		// over distill_royalty_mints / distill_royalty_margin. Same read-only admin-gated
		// (NOT economy-gated) shape; NO similarity detector + NO resolver (distill has
		// neither, by design). Read-only seams; the distill adjudicate/clawback path + any
		// sweeper are untouched.
		distillRoyaltyDetector := poolroyalty.NewDistillDetectorReader(pool, thresholdsFromConfig(cfg))
		distillRoyaltyMargin := poolroyalty.NewDistillMarginReader(pool)
		distillRoyaltyResolver := poolroyalty.NewDistillResolver(pool)
		authed.Get("/v1/admin/distill-royalty/detect", requireAdminOrOperatorRead(authManager, http.HandlerFunc(newDistillRoyaltyDetectHandler(distillRoyaltyDetector))))
		authed.Get("/v1/admin/distill-royalty/resolve", requireAdminOrOperatorRead(authManager, http.HandlerFunc(newDistillRoyaltyResolveHandler(distillRoyaltyResolver))))
		authed.Get("/v1/admin/distill-royalty/margin", requireAdminOrOperatorRead(authManager, http.HandlerFunc(newDistillRoyaltyMarginHandler(distillRoyaltyMargin))))

		authed.Post("/v1/auth/refresh", func(w http.ResponseWriter, req *http.Request) {
			if authManager.PrivateKey() == nil {
				writeJSONErr(w, http.StatusServiceUnavailable, "JWT signing not available")
				return
			}
			actx, err := authManager.Authenticate(req)
			if err != nil || actx.AuthMethod != auth.MethodJWT {
				writeJSONErr(w, http.StatusUnauthorized, "valid JWT required")
				return
			}
			ttl := auth.ClampTTL(cfg.TokenTTL)
			tok, err := auth.GenerateToken(actx.WorkspaceID, actx.UserID, actx.Scopes, authManager.PrivateKey(), ttl)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]any{
				"token":      tok,
				"expires_at": time.Now().Add(ttl).UTC().Format(time.RFC3339),
			})
		})

		authed.Get("/v1/auth/me", func(w http.ResponseWriter, req *http.Request) {
			actx, err := authManager.Authenticate(req)
			if err != nil {
				writeJSONErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeJSONOK(w, http.StatusOK, actx)
		})

		// ─── Key rotation + usage ──────────────────────
		authed.Post("/v1/workspaces/{wsID}/api-keys/{keyID}/rotate", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			keyID := chi.URLParam(req, "keyID")
			// RotateAPIKey is a single atomic transaction: SELECT FOR UPDATE on the
			// old key → INSERT new key → DELETE old key. Concurrent rotate calls for
			// the same keyID are serialised by the row-level lock; the second call
			// finds the row gone and gets a 404. This prevents the previous race
			// where two concurrent calls could produce two simultaneously active keys.
			raw, fresh, err := tenantStore.RotateAPIKey(req.Context(), wsID, keyID)
			if errors.Is(err, tenant.ErrKeyNotFound) {
				writeJSONErr(w, http.StatusNotFound, "key not found")
				return
			}
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			logger.Info("auth: key rotated",
				slog.String("workspace_id", wsID),
				slog.String("old_key_id", keyID),
				slog.String("new_key_id", fresh.ID),
			)
			writeJSONOK(w, http.StatusCreated, map[string]any{
				"key":     raw,
				"id":      fresh.ID,
				"prefix":  fresh.KeyPrefix,
				"warning": "Old key revoked. Store this new key securely — it will not be shown again.",
			})
		})

		// ─── W1.9.1: rotation for a key a RUNNING SERVICE holds ──────────
		//
		// The route above is an ATOMIC REPLACE and stays: it is the right primitive for a key a
		// person can paste immediately, and for a leaked key that must die now with downtime as
		// the lesser harm. It is the wrong one for a key a PROCESS holds, because both rows change
		// visibility at commit and the holder never has a moment in which both credentials work
		// (internal/tenant/rotation_overlap_integration_test.go pins that against real Postgres).
		//
		// These three are the other path. The order is the design and must not invert: mint →
		// BOTH VALID → the holder switches → the new key authenticates a real request → only then
		// is the old one revoked. Completion is REFUSED until that request has happened, which is
		// the one thing CreateAPIKey + RevokeAPIKey cannot enforce on their own.
		authed.Post("/v1/workspaces/{wsID}/api-keys/{keyID}/rotate/begin", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			keyID := chi.URLParam(req, "keyID")
			raw, fresh, rot, err := tenantStore.BeginRotation(req.Context(), wsID, keyID)
			switch {
			case errors.Is(err, tenant.ErrKeyNotFound):
				writeJSONErr(w, http.StatusNotFound, "key not found")
				return
			case errors.Is(err, tenant.ErrRotationOpen):
				writeJSONErr(w, http.StatusConflict, err.Error()+
					" — complete or abandon it before starting another, or the holder cannot be told which key to take")
				return
			case err != nil:
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			logger.Info("auth: key rotation begun",
				slog.String("workspace_id", wsID),
				slog.String("rotation_id", rot.ID),
				slog.String("old_key_id", keyID),
				slog.String("new_key_id", fresh.ID),
			)
			writeJSONOK(w, http.StatusCreated, map[string]any{
				"rotation_id": rot.ID,
				"key":         raw,
				"id":          fresh.ID,
				"prefix":      fresh.KeyPrefix,
				"started_at":  rot.StartedAt,
				// ⚠ SAID ON THE RESPONSE, not only in a runbook. The old key is still live and the
				// operator has to know that, or they will assume this call finished the job.
				"old_key_still_valid": true,
				"next_step": "put this key in front of the holder and let it make one real request, then POST " +
					"/v1/workspaces/" + wsID + "/key-rotations/" + rot.ID + "/complete. The old key keeps working until you do.",
			})
		})

		authed.Get("/v1/workspaces/{wsID}/key-rotations/{rotationID}", func(w http.ResponseWriter, req *http.Request) {
			st, err := tenantStore.RotationStatus(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "rotationID"))
			if errors.Is(err, tenant.ErrRotationNotFound) {
				writeJSONErr(w, http.StatusNotFound, "rotation not found")
				return
			}
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, st)
		})

		authed.Post("/v1/workspaces/{wsID}/key-rotations/{rotationID}/complete", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			rotID := chi.URLParam(req, "rotationID")
			err := tenantStore.CompleteRotation(req.Context(), wsID, rotID)
			switch {
			case errors.Is(err, tenant.ErrRotationNotFound):
				writeJSONErr(w, http.StatusNotFound, "rotation not found")
				return
			case errors.Is(err, tenant.ErrRotationClosed):
				writeJSONErr(w, http.StatusConflict, err.Error())
				return
			case errors.Is(err, tenant.ErrNewKeyUnused):
				// 412 and not 400: the request is well-formed and the SERVER's precondition is
				// unmet. Nothing was changed — both keys are still live — and the message says
				// what makes it true rather than only that it is false.
				writeJSONErr(w, http.StatusPreconditionFailed, err.Error()+
					" — nothing was revoked. Point the holder at the new key and let it make one real "+
					"authenticated request; a health check does not count, because a health route answers "+
					"200 with no credential at all.")
				return
			case err != nil:
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			logger.Info("auth: key rotation completed",
				slog.String("workspace_id", wsID), slog.String("rotation_id", rotID))
			writeJSONOK(w, http.StatusOK, map[string]any{
				"rotation_id": rotID,
				"outcome":     "completed",
				"detail":      "the old key is revoked; the new key has already proven it authenticates",
			})
		})

		authed.Post("/v1/workspaces/{wsID}/key-rotations/{rotationID}/abandon", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			rotID := chi.URLParam(req, "rotationID")
			err := tenantStore.AbandonRotation(req.Context(), wsID, rotID)
			switch {
			case errors.Is(err, tenant.ErrRotationNotFound):
				writeJSONErr(w, http.StatusNotFound, "rotation not found")
				return
			case errors.Is(err, tenant.ErrRotationClosed):
				writeJSONErr(w, http.StatusConflict, err.Error())
				return
			case err != nil:
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			logger.Info("auth: key rotation abandoned",
				slog.String("workspace_id", wsID), slog.String("rotation_id", rotID))
			writeJSONOK(w, http.StatusOK, map[string]any{
				"rotation_id": rotID,
				"outcome":     "abandoned",
				"detail":      "the NEW key is revoked; the key the holder is using is untouched",
			})
		})

		authed.Get("/v1/workspaces/{wsID}/api-keys/{keyID}/usage", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			keyID := chi.URLParam(req, "keyID")
			keys, err := tenantStore.ListAPIKeys(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			for _, k := range keys {
				if k.ID == keyID {
					requests, chargedULXC, err := tenantStore.KeyUsage(req.Context(), wsID, keyID)
					if err != nil {
						writeJSONErr(w, http.StatusInternalServerError, err.Error())
						return
					}
					writeJSONOK(w, http.StatusOK, map[string]any{
						"last_used_at":   k.LastUsedAt,
						"total_requests": requests,
						"total_cost":     float64(chargedULXC) / 1e6 * economy.LXCUSDValue, // USD, as before
					})
					return
				}
			}
			writeJSONErr(w, http.StatusNotFound, "key not found")
		})

		// ADMIN-ONLY (cross-tenant IDOR fix): RegisterWorkspace is a blind upsert on the body-supplied
		// id, and this route has no {wsID} segment so workspaceIsolationMiddleware skips it — ungated,
		// any authenticated non-admin key could overwrite ANOTHER tenant's config (spend cap / cache+
		// distill pooling / logging policy). Provisioning is privileged; gate it like the ~30 sibling
		// control-plane routes. Handler body extracted verbatim to newRegisterWorkspaceHandler.
		authed.Post("/v1/workspaces", requireAdmin(authManager, newRegisterWorkspaceHandler(wsManager)))

		// ADMIN-ONLY comped-LXC grant (default-off behind LENS_ADMIN_LXC_GRANT_ENABLED). Bootstrap
		// escape hatch for a closed trial: a fresh workspace has 0 LXC and no funding path without
		// Stripe, so it cannot transact. This funds it through economy.GrantLXC (the atomic ledger+
		// balance move, recorded under LXCTypeGrant so a comp is never mistaken for a paid purchase).
		// Off ⇒ the route is never registered (chi 404). Body-param target, no {wsID} — requireAdmin
		// is the sole authority (matching the workspace-register admin route above).
		if cfg.AdminLXCGrantEnabled {
			authed.Post("/v1/admin/lxc/grant", requireAdmin(authManager, newAdminLXCGrantHandler(dualToken)))
		}

		// Self-scoped list of the workspaces the caller's credential can act on. No {wsID} segment, so
		// workspaceIsolationMiddleware passes through and the handler scopes to the authenticated
		// WorkspaceID. ALWAYS a JSON array (one element today) so a future multi-workspace token widens
		// it with no contract change — a workspace switcher is buildable now.
		authed.Get("/v1/workspaces", newListMyWorkspacesHandler(authManager, wsManager))

		authed.Get("/v1/workspaces/{wsID}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			ws, ok := wsManager.GetWorkspace(wsID)
			if !ok {
				writeJSONErr(w, http.StatusNotFound, "workspace not found")
				return
			}
			writeJSONOK(w, http.StatusOK, ws)
		})

		// B46.15: the workspace's owner names the workspace.
		mountWorkspaceNameRoutes(authed, pool, wsManager)

		// ─── Workspace tenant config + keys ─────────────────
		// Per-workspace policy bundle (spend cap, model + provider
		// allowlists, retention) and workspace-scoped API keys.
		// These sit alongside the Lens-admin key routes above —
		// admin keys authenticate the call, then operate on
		// workspace-scoped credentials.
		authed.Get("/v1/workspaces/{wsID}/config", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			cfg, err := tenantStore.GetConfig(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if cfg == nil {
				writeJSONErr(w, http.StatusNotFound, "no workspace config")
				return
			}
			writeJSONOK(w, http.StatusOK, cfg)
		})

		authed.Put("/v1/workspaces/{wsID}/config", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in tenant.WorkspaceConfig
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			in.ID = wsID
			if err := tenantStore.UpsertConfig(req.Context(), in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			// Drop any cached spend snapshot so the new cap kicks
			// in on the next request instead of waiting for the TTL.
			spendTracker.InvalidateCache(wsID)
			p.InvalidateWorkspaceLimits(wsID)
			writeJSONOK(w, http.StatusOK, in)
		})

		authed.Post("/v1/workspaces/{wsID}/api-keys", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				Name      string     `json:"name"`
				Scopes    []string   `json:"scopes"`
				ExpiresAt *time.Time `json:"expires_at,omitempty"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			raw, key, err := tenantStore.CreateAPIKey(req.Context(), wsID, in.Name, in.Scopes, in.ExpiresAt)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, map[string]any{
				"key":     raw,
				"id":      key.ID,
				"prefix":  key.KeyPrefix,
				"name":    key.Name,
				"scopes":  key.Scopes,
				"warning": "Store this key securely. It will not be shown again.",
			})
		})

		authed.Get("/v1/workspaces/{wsID}/api-keys", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			keys, err := tenantStore.ListAPIKeys(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, keys)
		})

		authed.Delete("/v1/workspaces/{wsID}/api-keys/{keyID}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			keyID := chi.URLParam(req, "keyID")
			// workspaceIsolationMiddleware has already proven the caller owns
			// {wsID}. RevokeAPIKey operates by keyID alone, so confirm the key
			// actually belongs to {wsID} before revoking — otherwise a caller
			// could revoke another tenant's key by passing its ID under their
			// own workspace path.
			keys, err := tenantStore.ListAPIKeys(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			owned := false
			for _, k := range keys {
				if k.ID == keyID {
					owned = true
					break
				}
			}
			if !owned {
				writeJSONErr(w, http.StatusNotFound, "key not found")
				return
			}
			if err := tenantStore.RevokeAPIKey(req.Context(), keyID); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		// ─── per-team / per-sprint budgets (Upgrade 19) ───
		// CRUD + a live status endpoint. Every mutation reloads the budget
		// service's in-memory snapshot so edits take effect on the next
		// request without waiting for the periodic refresh.

		authed.Post("/v1/workspaces/{wsID}/budgets", budgetCreateHandler(budgetStore, budgetService.Reload))

		authed.Get("/v1/workspaces/{wsID}/budgets", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			list, err := budgetStore.List(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, list)
		})

		// status MUST be registered before the {id} route so the literal
		// segment wins; chi matches static paths ahead of wildcards anyway,
		// but keeping them adjacent documents the intent.
		authed.Get("/v1/workspaces/{wsID}/budgets/status", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			team := req.URL.Query().Get("team")
			sprint := req.URL.Query().Get("sprint")
			writeJSONOK(w, http.StatusOK, budgetService.Status(wsID, team, sprint))
		})

		authed.Get("/v1/workspaces/{wsID}/budgets/{id}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			id := chi.URLParam(req, "id")
			b, err := budgetStore.Get(req.Context(), wsID, id)
			if errors.Is(err, budgets.ErrNotFound) {
				writeJSONErr(w, http.StatusNotFound, "budget not found")
				return
			}
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, b)
		})

		authed.Patch("/v1/workspaces/{wsID}/budgets/{id}", budgetUpdateHandler(budgetStore, budgetService.Reload))

		authed.Delete("/v1/workspaces/{wsID}/budgets/{id}", budgetDeleteHandler(budgetStore, budgetService.Reload))

		// ─── cost forecasting (Upgrade 20) ───
		// Read-only, cached projections. scope defaults to workspace,
		// scope_id to the workspace id, period to monthly.
		authed.Get("/v1/workspaces/{wsID}/forecast", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			q := req.URL.Query()
			scope := budgets.Scope(q.Get("scope"))
			if scope == "" {
				scope = budgets.ScopeWorkspace
			}
			fc, err := forecaster.ProjectScope(req.Context(), wsID, scope, q.Get("scope_id"), q.Get("period"))
			// An unsupported ?scope= is the CALLER's mistake, not the server's:
			// 400 with the supported set, never 500. (It used to be neither —
			// an unknown scope was projected as the whole workspace's spend.)
			if errors.Is(err, budgets.ErrUnknownScope) {
				writeJSONErr(w, http.StatusBadRequest, "unknown scope: must be one of workspace, team, sprint")
				return
			}
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, fc)
		})

		authed.Get("/v1/workspaces/{wsID}/forecast/summary", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			list, err := forecaster.SummarizeWorkspace(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, list)
		})

		// ─── cost anomaly detection (Upgrade 21) ───
		// Read-only, cached cross-sectional outlier flags. scope is the unit
		// kind (issue/team/sprint); default issue. These are statistical
		// flags, not judgments.
		authed.Get("/v1/workspaces/{wsID}/anomalies", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			scope := req.URL.Query().Get("scope")
			if scope == "" {
				scope = costanomaly.UnitIssue
			}
			res, err := costAnomalyDetector.ScanScope(req.Context(), wsID, scope)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, res)
		})

		authed.Get("/v1/workspaces/{wsID}/anomalies/issue/{issueID}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			issueID := chi.URLParam(req, "issueID")
			a, err := costAnomalyDetector.CheckIssue(req.Context(), wsID, issueID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, a)
		})

		// ─── executive ROI reporting (Upgrade 24) ───
		// Read-only, cached. format=json (default) | html | md. The HTML is
		// self-contained + print-to-PDF-able; emailing it is Track's job.
		authed.Get("/v1/workspaces/{wsID}/roi/report", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			q := req.URL.Query()
			rep, err := roiReporter.GenerateReport(req.Context(), wsID, q.Get("period"))
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			switch q.Get("format") {
			case "html":
				html, err := roi.RenderHTML(rep)
				if err != nil {
					writeJSONErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte(html)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- roi.RenderHTML uses html/template, which escapes every value
			case "md", "markdown":
				w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
				// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- text/markdown, not HTML; nosniff is set on every response
				_, _ = w.Write([]byte(roi.RenderMarkdown(rep)))
			default:
				writeJSONOK(w, http.StatusOK, rep)
			}
		})

		authed.Get("/v1/workspaces/{wsID}/roi/report/summary", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			summary, err := roiReporter.GenerateSummary(req.Context(), wsID, req.URL.Query().Get("period"))
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, summary)
		})

		// ─── routing intelligence introspection (Upgrade 22) ───
		// Read-only: lets users SEE what the advisor would pick (basis +
		// numbers) so the intelligence isn't a black box.
		authed.Get("/v1/workspaces/{wsID}/routing/recommendation", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			q := req.URL.Query()
			inputRange := q.Get("input_range")
			if inputRange == "" {
				inputRange = mining.InputBucketMedium
			}
			provider := q.Get("provider")
			if provider == "" {
				provider = "openai"
			}
			var allowedModels, allowedProviders []string
			if ws, ok := wsManager.GetWorkspace(wsID); ok {
				allowedModels, allowedProviders = ws.AllowedModels, ws.AllowedProviders
			}
			rec := routingAdvisor.RecommendByRange(req.Context(), wsID, q.Get("feature"), inputRange, provider, allowedModels, allowedProviders)
			writeJSONOK(w, http.StatusOK, rec)
		})

		authed.Get("/v1/routing/intelligence/status", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, map[string]any{
				"status":  routingAdvisor.Status(),
				"cohorts": routingAdvisor.Overview(),
			})
		})

		// ─── model capabilities (Upgrade 15) ───
		// Which models support which modalities — so capability-aware
		// routing isn't a black box.
		authed.Get("/v1/models/capabilities", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, modality.CapabilityMap())
		})

		// ─── model catalog (Upgrade 16) ───
		// The single source of truth for models — provider, pricing,
		// capabilities, context. Read-only introspection so pricing/caps are
		// transparent, not hidden.
		authed.Get("/v1/catalog/models", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, catalog.All())
		})
		// B10.5 — models a provider lists that wait for a price, and the admin act that prices one.
		authed.Get("/v1/catalog/discovered", newCatalogDiscoveredHandler(watcher))
		authed.Put("/v1/admin/catalog/models/{id}/price", requireAdmin(authManager, newCatalogPriceHandler(watcher)))
		authed.Get("/v1/catalog/models/{id}", func(w http.ResponseWriter, req *http.Request) {
			m, ok := catalog.Get(chi.URLParam(req, "id"))
			if !ok {
				writeJSONErr(w, http.StatusNotFound, "model not in catalog")
				return
			}
			writeJSONOK(w, http.StatusOK, m)
		})

		// ─── Local endpoint registry ───────────────────
		// Admin surface for the multi-endpoint Router. Listing
		// is read-only; the other three routes mutate the
		// in-process registry (no persistence — restarts re-read
		// LENS_LOCAL_ENDPOINTS).
		authed.Get("/v1/local/endpoints", requireAdmin(authManager, newLocalEndpointsListHandler(localRouterMulti)))

		authed.Post("/v1/local/endpoints", requireAdmin(authManager, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			var in localrouter.LocalEndpoint
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.URL == "" || in.Provider == "" {
				writeJSONErr(w, http.StatusBadRequest, "url and provider required")
				return
			}
			localRouterMulti.Register(&in)
			// Fire an immediate health check so the new endpoint
			// is usable on the next request rather than after the
			// 30s tick.
			go localRouterMulti.CheckHealth(ctx, &in)
			writeJSONOK(w, http.StatusCreated, &in)
		})))

		authed.Delete("/v1/local/endpoints/{id}", requireAdmin(authManager, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			if !localRouterMulti.Remove(id) {
				writeJSONErr(w, http.StatusNotFound, "endpoint not found")
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})))

		authed.Post("/v1/local/endpoints/{id}/check", requireAdmin(authManager, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			ep, err := localRouterMulti.CheckHealthByID(req.Context(), id)
			if err != nil {
				writeJSONErr(w, http.StatusNotFound, "endpoint not found")
				return
			}
			writeJSONOK(w, http.StatusOK, ep)
		})))

		// ─── LENS token endpoints (Batch 2 Item 1) ─────
		econ.get(authed, "/v1/workspaces/{wsID}/tokens/balance", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			snap, err := tokenLedger.GetSnapshot(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, snap)
		})

		// W4.6.1 step 7 — what this workspace has EARNED, as opposed to what it has been credited.
		//
		// ⚠ THIS IS NOT tokens/balance's lifetime_earned, AND THE DIFFERENCE IS THE POINT.
		// lens_token_balances.lifetime_earned is raised by EVERY credit with no filter on the ledger
		// type, so LENS a workspace was given, bought, or simply got back all count — measured at
		// 27× on a five-row fixture, and unbounded across stake/unstake round trips
		// (docs/lifetime-earned-measured.md, #472). That field is left exactly as it is, because it
		// is already served and already read by cmd/node and cmd/cachenode; this route is the
		// honest number beside it, not a replacement for it.
		//
		// Workspace-scoped by the {wsID} segment, so workspaceIsolationMiddleware applies — a caller
		// cannot read another workspace's earnings. Economy-gated like its neighbours.
		econ.get(authed, "/v1/workspaces/{wsID}/earnings", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			sum, err := earningsReader.ForWorkspace(req.Context(), wsID, earnings.Gates{
				EconomyEnabled:            cfg.EconomyEnabled,
				PoolRoyaltyMintingEnabled: cfg.PoolRoyaltyMintingEnabled,
				CachePoolableEnabled:      cfg.CachePoolableEnabled,
				DistillPoolableEnabled:    cfg.DistillPoolableEnabled,
			})
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, sum)
		})

		econ.get(authed, "/v1/workspaces/{wsID}/tokens/history", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
			offset, _ := strconv.Atoi(req.URL.Query().Get("offset"))
			entries, err := tokenLedger.GetHistory(req.Context(), wsID, limit, offset)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, entries)
		})

		// ─── LXC compute credit (two-token split) ──────
		// U18: LXC is the fiat usage credit — the balance READ is fiat (registers
		// unconditionally; survives LENS_ECONOMY_ENABLED=false). The convert route
		// below STAYS economy-gated (it burns LENS).
		authed.Get("/v1/workspaces/{wsID}/lxc/balance", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			snap, err := dualToken.GetLXCSnapshot(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, snap)
		})

		// U18: LXC ledger history — FIAT, so it registers unconditionally on authed.Get like
		// lxc/balance above (survives LENS_ECONOMY_ENABLED=false), NOT via econ.get. The {wsID}
		// segment is bound to the caller's credential by workspaceIsolationMiddleware (a foreign
		// wsID is 403, exactly like tokens/history and lxc/balance). limit/offset mirror tokens/history.
		authed.Get("/v1/workspaces/{wsID}/lxc/history", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
			offset, _ := strconv.Atoi(req.URL.Query().Get("offset"))
			entries, err := dualToken.GetLXCHistory(req.Context(), wsID, limit, offset)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, entries)
		})

		// U18b billing — FIAT, registered under BillingEnabled (independent of the
		// economy master). Checkout is workspace-scoped: workspaceIsolationMiddleware
		// binds {wsID} to the caller's credential (admin bypasses), like its siblings.
		// B25.2: a test workspace's checkout is a Stripe test-mode one (billRoute).
		bill.post(authed, "/v1/workspaces/{wsID}/billing/checkout", newCheckoutHandler(billRoute))

		// MODEL 2 (W4.6.1 step 1) — the subscription checkout. Same {wsID} binding as
		// its one-off sibling above: the caller's credential owns the workspace, admin
		// bypasses. Registered only when a Stripe Price is configured. B25.2: a test workspace subscribes to
		// a Stripe test-mode plan, and each subscription route answers from the Service that sold it.
		subs.post(authed, "/v1/workspaces/{wsID}/billing/subscribe", billRoute.onSubscriptions(true, newSubscribeHandler))

		// The read: is this workspace paying, and until when. ⚠ Registered under the
		// SAME gate as the checkout, so a deployment without subscriptions answers 404
		// rather than a confident {"subscribed": false} about a product it does not sell.
		subs.get(authed, "/v1/workspaces/{wsID}/billing/subscription", billRoute.onSubscriptions(false, func(billingSvc *billing.Service) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) {
				wsID := chi.URLParam(req, "wsID")
				st, err := billingSvc.GetSubscription(req.Context(), wsID)
				if err != nil {
					writeJSONErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				writeJSONOK(w, http.StatusOK, st)
			}
		}))

		// B32.12 — the workspace's plan and what it unlocks, and the seats check the members feature asks before
		// it adds one (plan_gates_handler.go). {wsID} is bound to the caller's credential like its siblings.
		authed.Get("/v1/workspaces/{wsID}/plan", newWorkspacePlanHandler(pool))
		authed.Get("/v1/workspaces/{wsID}/plan/seats", newSeatsCheckHandler(pool))

		// B27.26 — "Your provider keys" (provider_keys_routes.go), only while custody is armed.
		if byokStore != nil {
			authed.Get("/v1/workspaces/{wsID}/provider-keys", newProviderKeysListHandler(byokStore))
			authed.Put("/v1/workspaces/{wsID}/provider-keys/{provider}", newProviderKeyPutHandler(byokStore))
			authed.Delete("/v1/workspaces/{wsID}/provider-keys/{provider}", newProviderKeyDeleteHandler(byokStore))
		}

		// B1.6 — the subscriber's plan this period: the allowance, what has been used
		// of it, and what the workspace's answers earned back (capped at the fee).
		// {"allowance": null} when no period is granted.
		subs.get(authed, "/v1/workspaces/{wsID}/billing/allowance", billRoute.onSubscriptions(false, func(billingSvc *billing.Service) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) {
				wsID := chi.URLParam(req, "wsID")
				sum, err := billingSvc.Summary(req.Context(), wsID, time.Now())
				if err != nil {
					writeJSONErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				writeJSONOK(w, http.StatusOK, sum)
			}
		}))

		// B18.14 — change plan: {"plan":"plus"|"pro"|"max"} moves the live subscription to that plan, with
		// proration. Like cancel, it writes no row — the webhook that follows records the new price and moves
		// this period's allowance to it.
		subs.post(authed, "/v1/workspaces/{wsID}/billing/subscription/plan", billRoute.onSubscriptions(true, func(billingSvc *billing.Service) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) {
				wsID := chi.URLParam(req, "wsID")
				var body struct {
					Plan string `json:"plan"`
				}
				if err := json.NewDecoder(io.LimitReader(req.Body, 1<<10)).Decode(&body); err != nil || body.Plan == "" {
					writeJSONErr(w, http.StatusBadRequest, "body must be {\"plan\": \"<name>\"}")
					return
				}
				st, err := billingSvc.ChangePlan(req.Context(), wsID, body.Plan)
				if err != nil {
					status := http.StatusInternalServerError
					switch {
					case errors.Is(err, billing.ErrNoSubscriptionPrice):
						status = http.StatusNotImplemented
					case errors.Is(err, billing.ErrUnknownPlan):
						status = http.StatusBadRequest
					case errors.Is(err, billing.ErrNoLiveSubscription), errors.Is(err, billing.ErrSamePlan), errors.Is(err, billing.ErrBYOKPlanChange),
						errors.Is(err, billing.ErrPlanKindChange):
						status = http.StatusConflict
					}
					writeJSONErr(w, status, err.Error())
					return
				}
				writeJSONOK(w, http.StatusOK, st)
			}
		}))

		// B32.10 — BYOK, Team's add-on: POST adds it to the live Team subscription as a second item, DELETE
		// removes it, each prorated. Like a plan change it writes no row — the webhook that follows records byok.
		subs.post(authed, "/v1/workspaces/{wsID}/billing/subscription/byok", billRoute.onSubscriptions(true, func(svc *billing.Service) http.HandlerFunc {
			return newBYOKAddonHandler(svc, true)
		}))
		subs.delete(authed, "/v1/workspaces/{wsID}/billing/subscription/byok", billRoute.onSubscriptions(true, func(svc *billing.Service) http.HandlerFunc {
			return newBYOKAddonHandler(svc, false)
		}))

		// B1.5 — cancel and resume (billing_routes.go). Cancel is AT PERIOD END.
		subs.post(authed, "/v1/workspaces/{wsID}/billing/subscription/cancel", billRoute.onSubscriptions(false, func(svc *billing.Service) http.HandlerFunc {
			return newSubscriptionCancelHandler(svc, true)
		}))
		subs.post(authed, "/v1/workspaces/{wsID}/billing/subscription/resume", billRoute.onSubscriptions(false, func(svc *billing.Service) http.HandlerFunc {
			return newSubscriptionCancelHandler(svc, false)
		}))

		// Admin refund-visibility list (read-only; requireAdmin). An 'anomalous' row
		// means the customer was CHARGED and NOT credited — v1 resolution is a manual
		// refund in the Stripe dashboard.
		bill.get(authed, "/v1/admin/billing/purchases",
			requireAdminOrOperatorRead(authManager, newBillingPurchasesHandler(billingSvc)))

		// B25.2: a test workspace converts its test LENS too; the rows it writes are marked test (0173).
		econ.post(authed, "/v1/workspaces/{wsID}/lxc/convert", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				LXCAmount int64 `json:"lxc_amount_ulxc"` // µLXC (SEC-2)
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			res, err := dualToken.ConvertLENStoLXC(req.Context(), wsID, in.LXCAmount)
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, economy.ErrInsufficientLENSFor) {
					status = http.StatusPaymentRequired
				}
				writeJSONErr(w, status, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, res)
		})

		// Phase-4a Item 0: the /tokens/mining/cache stats endpoint was RETIRED with the
		// CacheMiner (it summarized a never-produced cache_mine track — always zero).

		econ.get(authed, "/v1/workspaces/{wsID}/tokens/mining/compute", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			stats, err := computeMiner.GetWorkspaceStats(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		// ─── Inference node CRUD (compute mining) ──────
		authed.Post("/v1/workspaces/{wsID}/nodes", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in mining.InferenceNode
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			in.WorkspaceID = wsID
			created, err := computeMiner.RegisterNode(req.Context(), in)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, created)
		})

		authed.Get("/v1/workspaces/{wsID}/nodes", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			nodes, err := computeMiner.ListNodes(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, nodes)
		})

		// Node heartbeat (Batch 3 Phase 2). Workspace-scoped UPDATE:
		// the SQL requires id AND workspace_id so a key from workspace-A
		// cannot keep alive workspace-B's nodes (cross-workspace hijack fix).
		authed.Post("/v1/workspaces/{wsID}/nodes/{nodeID}/heartbeat", newInferenceHeartbeatHandler(pool, cpHB))

		authed.Delete("/v1/workspaces/{wsID}/nodes/{nodeID}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			nodeID := chi.URLParam(req, "nodeID")
			if err := computeMiner.DeactivateNode(req.Context(), nodeID, wsID); err != nil {
				if errors.Is(err, mining.ErrNodeNotFound) {
					writeJSONErr(w, http.StatusNotFound, "node not found")
					return
				}
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		// ─── Cache node registry (Batch 3 Phase 3) ──────
		// Cache nodes register themselves so Lens can route
		// cache reads/writes through their Redis. Earnings flow
		// via the existing CacheMiner pipeline; this endpoint
		// pair just covers registration + heartbeat.
		authed.Post("/v1/workspaces/{wsID}/cache-nodes", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				URL        string  `json:"url"`
				MaxSizeGB  float64 `json:"max_size_gb"`
				NodeSecret string  `json:"node_secret"`
				Share      bool    `json:"share"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.URL == "" {
				writeJSONErr(w, http.StatusBadRequest, "url required")
				return
			}
			if in.MaxSizeGB <= 0 {
				in.MaxSizeGB = 10
			}
			if pool == nil {
				writeJSONErr(w, http.StatusServiceUnavailable, "DB unavailable")
				return
			}
			secretHash, err := bcrypt.GenerateFromPassword([]byte(in.NodeSecret), bcrypt.DefaultCost)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, "hash node secret: "+err.Error())
				return
			}
			// Both INSERTs run in one transaction: if the metrics
			// seed fails the node row rolls back so heartbeat
			// UPDATEs never silently no-op against a metrics-less node.
			tx, err := pool.Begin(req.Context())
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			var id string
			var createdAt time.Time
			row := tx.QueryRow(req.Context(), `
				INSERT INTO cache_nodes (workspace_id, url, max_size_gb, node_secret_hash)
				VALUES ($1, $2, $3, $4)
				RETURNING id, created_at`,
				wsID, strings.TrimRight(in.URL, "/"), in.MaxSizeGB, string(secretHash))
			if err := row.Scan(&id, &createdAt); err != nil {
				_ = tx.Rollback(req.Context())
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			// Seed metrics row so heartbeat UPDATEs have a target.
			if _, err := tx.Exec(req.Context(),
				`INSERT INTO cache_node_metrics (node_id) VALUES ($1) ON CONFLICT (node_id) DO NOTHING`, id); err != nil {
				_ = tx.Rollback(req.Context())
				writeJSONErr(w, http.StatusInternalServerError, "seed metrics: "+err.Error())
				return
			}
			if err := tx.Commit(req.Context()); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, map[string]any{
				"id":           id,
				"workspace_id": wsID,
				"url":          in.URL,
				"max_size_gb":  in.MaxSizeGB,
				"created_at":   createdAt,
			})
		})

		authed.Delete("/v1/workspaces/{wsID}/cache-nodes/{nodeID}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			nodeID := chi.URLParam(req, "nodeID")
			if pool == nil {
				writeJSONErr(w, http.StatusServiceUnavailable, "DB unavailable")
				return
			}
			// workspace_id scope: prevents cross-workspace node deletion.
			tag, err := pool.Exec(req.Context(),
				`UPDATE cache_nodes SET active = FALSE WHERE id = $1 AND workspace_id = $2`, nodeID, wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if tag.RowsAffected() == 0 {
				writeJSONErr(w, http.StatusNotFound, "cache node not found")
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		authed.Post("/v1/workspaces/{wsID}/cache-nodes/{nodeID}/heartbeat", newCacheHeartbeatHandler(pool, cpHB))

		// ─── Embedding node CRUD (embedding mining) ────
		authed.Post("/v1/workspaces/{wsID}/embedding-nodes", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			// Decode into an inline struct so we can capture node_secret
			// without exposing it on mining.EmbeddingNode's JSON shape.
			var body struct {
				mining.EmbeddingNode
				NodeSecret string `json:"node_secret"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			in := body.EmbeddingNode
			in.WorkspaceID = wsID
			// Hash the node secret and store the hash — mirrors cache-node
			// registration. An empty secret is allowed for backwards
			// compatibility with nodes that pre-date the secret field;
			// NULLIF in the INSERT stores NULL for an empty hash.
			if body.NodeSecret != "" {
				hash, err := bcrypt.GenerateFromPassword([]byte(body.NodeSecret), bcrypt.DefaultCost)
				if err != nil {
					writeJSONErr(w, http.StatusInternalServerError, "hash node secret: "+err.Error())
					return
				}
				in.NodeSecretHash = string(hash)
			}
			created, err := embeddingMiner.RegisterNode(req.Context(), in)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, created)
		})

		authed.Get("/v1/workspaces/{wsID}/embedding-nodes", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			nodes, err := embeddingMiner.ListNodes(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, nodes)
		})

		authed.Delete("/v1/workspaces/{wsID}/embedding-nodes/{nodeID}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			nodeID := chi.URLParam(req, "nodeID")
			if err := embeddingMiner.DeactivateEmbeddingNode(req.Context(), nodeID, wsID); err != nil {
				if errors.Is(err, mining.ErrNodeNotFound) {
					writeJSONErr(w, http.StatusNotFound, "node not found")
					return
				}
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		// Embedding-node heartbeat — mirrors the inference-node and cache-node
		// heartbeat endpoints.  Refreshes last_seen_at via the control-plane
		// store so the reconciler can track liveness uniformly across all node
		// types (migration 0035 added last_seen_at to embedding_nodes).
		authed.Post("/v1/workspaces/{wsID}/embedding-nodes/{nodeID}/heartbeat", newEmbeddingHeartbeatHandler(cpStore, cpHB))

		econ.get(authed, "/v1/workspaces/{wsID}/tokens/mining/embeddings", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			stats, err := embeddingMiner.GetStats(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		// ─── Annotation mining (Batch 2 Item 4) ─────────
		// Proof-of-eval-contribution LIVE submission surface (the endpoint REPLACING the benchseed CLI). A
		// workspace submits an eval (author = caller); OTHER workspaces attest its claimed answer's correctness.
		// Submission activates the eval immediately (drawable), but EARNING stays gated downstream by the eval
		// minter's correctness-consensus + usage warmup + U6 floor — an active-but-unconsensed eval mints
		// nothing. econ.post registers only when the economy is on; the eval mint flags are default-off, so this
		// is inert in production until the operator turns the economy on.
		evalSubmitStore := benchprobe.NewStore(pool)
		econ.post(authed, "/v1/evals", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				Input             string `json:"input"`
				ExpectedOutput    string `json:"expected_output"`
				EvalMethod        string `json:"eval_method"`
				FeatureCategory   string `json:"feature_category"`
				AuthorWorkspaceID string `json:"author_workspace_id"` // honored only for admin callers
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			author, _, ok := effectiveWorkspaceID(req, in.AuthorWorkspaceID)
			if !ok || author == "" {
				writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
				return
			}
			if strings.TrimSpace(in.Input) == "" || strings.TrimSpace(in.ExpectedOutput) == "" {
				writeJSONErr(w, http.StatusBadRequest, "input and expected_output are required")
				return
			}
			if len(in.Input) > 16384 || len(in.ExpectedOutput) > 16384 {
				writeJSONErr(w, http.StatusRequestEntityTooLarge, "input/expected_output too large (max 16384 bytes each)")
				return
			}
			// Derive the cohort dims the same way the serve path + scorer do, so the eval is also usable as a
			// held-eval slice item for a matching cohort (dual use).
			inRange, complexity := cohort.DeriveInputCohort(in.Input)
			id, err := evalSubmitStore.SubmitLiveEval(req.Context(), benchprobe.EvalItem{
				Input: in.Input, ExpectedOutput: in.ExpectedOutput, EvalMethod: in.EvalMethod,
				AuthorWorkspaceID: author, FeatureCategory: in.FeatureCategory,
				InputTokenRange: inRange, ComplexityBucket: complexity,
			})
			if err != nil {
				if errors.Is(err, benchprobe.ErrDuplicateItem) {
					writeJSONErr(w, http.StatusConflict, err.Error())
					return
				}
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, map[string]string{"id": id})
		})

		econ.post(authed, "/v1/evals/{id}/attest", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			var in struct {
				Agrees              bool   `json:"agrees"`
				AttesterWorkspaceID string `json:"attester_workspace_id"` // honored only for admin callers
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			attester, _, ok := effectiveWorkspaceID(req, in.AttesterWorkspaceID)
			if !ok || attester == "" {
				writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
				return
			}
			if err := evalSubmitStore.AttestCorrectness(req.Context(), id, attester, in.Agrees); err != nil {
				switch {
				case errors.Is(err, benchprobe.ErrUnknownItem):
					writeJSONErr(w, http.StatusNotFound, err.Error())
				case errors.Is(err, benchprobe.ErrSelfAttestation):
					writeJSONErr(w, http.StatusForbidden, err.Error())
				default:
					writeJSONErr(w, http.StatusBadRequest, err.Error())
				}
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		econ.post(authed, "/v1/workspaces/{wsID}/annotate/stake", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				Amount int64 `json:"amount_ulens"` // µLENS (SEC-2)
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := annotationMiner.Stake(req.Context(), wsID, in.Amount); err != nil {
				if errors.Is(err, mining.ErrInsufficientBalance) {
					writeJSONErr(w, http.StatusPaymentRequired, err.Error())
					return
				}
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			staked, _ := annotationMiner.GetStake(req.Context(), wsID)
			writeJSONOK(w, http.StatusOK, map[string]int64{"staked_ulens": staked}) // µLENS (SEC-2)
		})

		econ.del(authed, "/v1/workspaces/{wsID}/annotate/stake", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			if err := annotationMiner.Unstake(req.Context(), wsID); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		econ.get(authed, "/v1/workspaces/{wsID}/annotate/task", func(w http.ResponseWriter, req *http.Request) { // Phase-0 Item C: economy-gate (was authed.Get)
			wsID := chi.URLParam(req, "wsID")
			task, err := annotationMiner.GetPendingTask(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if task == nil {
				writeJSONErr(w, http.StatusNotFound, "no pending tasks")
				return
			}
			writeJSONOK(w, http.StatusOK, task)
		})

		econ.post(authed, "/v1/workspaces/{wsID}/annotate/task/{taskID}", func(w http.ResponseWriter, req *http.Request) { // Phase-0 Item C: economy-gate the MINT route (was authed.Post)
			wsID := chi.URLParam(req, "wsID")
			taskID := chi.URLParam(req, "taskID")
			var in struct {
				Decision    string `json:"decision"`
				Confidence  int    `json:"confidence"`
				TimeSpentMs int    `json:"time_spent_ms"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			err := annotationMiner.SubmitAnnotation(req.Context(), mining.Annotation{
				TaskID: taskID, AnnotatorID: wsID,
				Decision: in.Decision, Confidence: in.Confidence, TimeSpentMs: in.TimeSpentMs,
			})
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, mining.ErrDuplicateAnnotation) {
					status = http.StatusConflict
				} else if errors.Is(err, mining.ErrTaskExpired) {
					status = http.StatusGone
				} else if errors.Is(err, mining.ErrInsufficientStake) {
					status = http.StatusPaymentRequired
				}
				writeJSONErr(w, status, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, map[string]bool{"ok": true})
		})

		econ.get(authed, "/v1/workspaces/{wsID}/tokens/mining/annotations", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			stats, err := annotationMiner.GetAnnotatorStats(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		authed.Get("/v1/workspaces/{wsID}/annotate/stats", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			stats, err := annotationMiner.GetAnnotatorStats(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		// ─── Pattern mining (Batch 2 Item 5) ────────────
		econ.post(authed, "/v1/workspaces/{wsID}/pattern-mining/opt-in", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			if !cfg.PatternMiningEnabled {
				writeJSONErr(w, http.StatusServiceUnavailable,
					"pattern mining disabled (set LENS_PATTERN_MINING_ENABLED=true)")
				return
			}
			if err := patternMiner.OptIn(req.Context(), wsID); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"opted_in": true})
		})

		econ.get(authed, "/v1/workspaces/{wsID}/pattern-mining/opt-in", patternMiningOptInStatus(patternMiner, cfg.PatternMiningEnabled))

		econ.del(authed, "/v1/workspaces/{wsID}/pattern-mining/opt-in", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			if err := patternMiner.OptOut(req.Context(), wsID); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"opted_in": false})
		})

		econ.get(authed, "/v1/workspaces/{wsID}/tokens/mining/patterns", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			c, err := patternMiner.GetContribution(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, c)
		})

		// ─── Token transfers + marketplace + staking ────
		econ.post(authed, "/v1/workspaces/{wsID}/tokens/transfer", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				ToWorkspace string `json:"to_workspace"`
				Amount      int64  `json:"amount_ulens"` // µLENS (SEC-2)
				Description string `json:"description"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := tokenLedger.Transfer(req.Context(), wsID, in.ToWorkspace, in.Amount, in.Description); err != nil {
				if errors.Is(err, mining.ErrInsufficientBalance) {
					writeJSONErr(w, http.StatusPaymentRequired, err.Error())
					return
				}
				if errors.Is(err, workspace.ErrMoneyWall) {
					writeJSONErr(w, http.StatusForbidden, err.Error())
					return
				}
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		// ⚠ B18.1: THE TOKEN EXCHANGE AND TOKEN STAKING ARE NOT REGISTERED. Measured 27 Sep at 682da06:
		// POST /v1/marketplace/listings/{id}/buy credited the buyer, the fee and the seller's unsold
		// remainder and debited NO ONE — the buyer received LENS and paid nothing — and a stake's
		// yield ran from time.Since(StartedAt) with no ceiling at unlocks_at
		// (internal/economy/marketplace.go). Both served by default behind LENS_ECONOMY_ENABLED, which
		// also runs pooling royalties, so no switch could close them alone.
		//
		// Retire or fix is Nicolai's decision, after the lawyer's answer. Until then nothing can list,
		// buy, cancel, stake or unstake, so no LENS moves and no yield is minted (it is minted only at
		// unstake). Not one listing, position or balance was changed: production held 0 listings,
		// 0 trades and 0 stake positions. The logic stays in internal/economy (MarketplaceStore) and
		// the authz-hardened trades handler in authz_handlers.go, for whichever way it is decided.
		// POVI's /annotate/stake and /v1/povi/nodes/{nodeID}/stake are a different mechanism and stay.

		authed.Get("/v1/workspaces/{wsID}/spend/current-month", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			spent, err := spendTracker.CurrentSpend(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]float64{
				"current_month_usd": spent,
			})
		})

		// B27.32: this month's MEASURED saving — the sum, over the workspace's own token_events rows, of what
		// each request would have cost at the model it asked for with no Talyvor cache, minus what it was
		// charged. Read straight from the rows (no cache), so it is exactly what they add up to.
		authed.Get("/v1/workspaces/{wsID}/savings/current-month", func(w http.ResponseWriter, req *http.Request) {
			saving, err := alertManager.MonthSaving(req.Context(), chi.URLParam(req, "wsID"), time.Now())
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, saving)
		})

		// ─── Git attribution ────────────────────────────────
		// Per-request rollups served from request_attribution
		// (migration 0017) — now the SOLE attribution source (the
		// branch_spend double-write was retired in #157).
		authed.Get("/v1/workspaces/{wsID}/attribution/branches", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			since := parseSinceParam(req.URL.Query().Get("since"))
			limit := 20
			if v := req.URL.Query().Get("limit"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
					limit = n
				}
			}
			rows, err := attrStore.GetCostByBranch(req.Context(), wsID, since, limit)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, rows)
		})

		authed.Get("/v1/workspaces/{wsID}/attribution/branches/{branch}", newBranchStatsHandler(attrStore.GetBranchStats))

		authed.Get("/v1/workspaces/{wsID}/attribution/prs/{prNumber}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			prNumber := chi.URLParam(req, "prNumber")
			stats, err := attrStore.GetPRStats(req.Context(), wsID, prNumber)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		authed.Get("/v1/workspaces/{wsID}/attribution/summary", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			days := 30
			if v := req.URL.Query().Get("days"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
					days = n
				}
			}
			sum, err := attrStore.GetSummary(req.Context(), wsID, days)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, sum)
		})

		// Quality stats for the per-workspace dashboard. `days`
		// query param caps the rolling window (default 30).
		authed.Get("/v1/workspaces/{wsID}/quality/stats", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			days := 30
			if v := req.URL.Query().Get("days"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
					days = n
				}
			}
			stats, err := qualityScorer.StatsForWorkspace(req.Context(), wsID, days)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		// Per-workspace logging policy toggle. Applies immediately —
		// proxy.serve() reads via GetLoggingPolicy on every request and
		// there's no per-request cache of the decision.
		authed.Put("/v1/workspaces/{wsID}/logging", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				LoggingPolicy workspace.LoggingPolicy `json:"logging_policy"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := wsManager.SetLoggingPolicy(req.Context(), wsID, in.LoggingPolicy); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			ws, _ := wsManager.GetWorkspace(wsID)
			writeJSONOK(w, http.StatusOK, map[string]any{
				"ok":             true,
				"logging_policy": ws.LoggingPolicy,
			})
		})

		// Per-workspace DISTILL policy toggle (stage 3, PR #2). Applies
		// immediately — proxy.serve() reads via GetDistillPolicy on every
		// request. New workspaces get `always` (workspace.DefaultDistillPolicy,
		// migration 0134); this is where a workspace turns it off or back on.
		authed.Put("/v1/workspaces/{wsID}/distill", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				DistillPolicy workspace.DistillPolicy `json:"distill_policy"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := wsManager.SetDistillPolicy(req.Context(), wsID, in.DistillPolicy); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			ws, _ := wsManager.GetWorkspace(wsID)
			writeJSONOK(w, http.StatusOK, map[string]any{
				"ok":             true,
				"distill_policy": ws.DistillPolicy,
			})
		})

		// B18.6: the prompt rewriter is retired. This route no longer turns it on — 410 — and still
		// accepts "disabled", which is every workspace's policy. See compression_handler.go.
		authed.Put("/v1/workspaces/{wsID}/compression", newCompressionPolicyHandler(wsManager))

		// B6.5 — per-workspace TARE policy (migration 0126): disabled | opt_in | always. ALWAYS by
		// default (migration 0134); opt_in reduces only requests that also carry X-Talyvor-Tare: true. Applies on the
		// next request, streaming and buffered alike. See internal/workspace/tare_policy.go.
		authed.Put("/v1/workspaces/{wsID}/tare", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				TarePolicy workspace.TarePolicy `json:"tare_policy"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := wsManager.SetTarePolicy(req.Context(), wsID, in.TarePolicy); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			ws, _ := wsManager.GetWorkspace(wsID)
			writeJSONOK(w, http.StatusOK, map[string]any{"ok": true, "tare_policy": ws.TarePolicy})
		})

		// B27.35 — Tare phase 2a, the prose compression model (migration 0184): {"enabled": true|false}.
		// OFF unless the workspace turns it on here, because it drops words. It runs only where the Tare
		// policy above lets Tare run, and only on prose every phase-1 reducer refused.
		authed.Put("/v1/workspaces/{wsID}/tare-model", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				Enabled *bool `json:"enabled"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.Enabled == nil {
				writeJSONErr(w, http.StatusBadRequest, `body must be {"enabled": true} or {"enabled": false}`)
				return
			}
			if err := wsManager.SetTareModel(req.Context(), wsID, *in.Enabled); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]any{"ok": true, "tare_model": wsManager.GetTareModel(wsID)})
		})

		// B27.36 — whether Talyvor may train its own compressor on this workspace's prose (migration 0243):
		// {"enabled": true|false, "by": "<who>"}. OFF for every workspace until an owner turns it on here; separate
		// from Sharing and from tare-model. Lens keeps who switched it and when: the authenticated caller, and `by` —
		// the signed-in owner the app names — beside it, never instead. Switched off, collection stops and the
		// workspace's traces are deleted.
		authed.Put("/v1/workspaces/{wsID}/tare-training", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				Enabled *bool  `json:"enabled"`
				By      string `json:"by"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.Enabled == nil {
				writeJSONErr(w, http.StatusBadRequest, `body must be {"enabled": true} or {"enabled": false}`)
				return
			}
			_, caller, _ := marketCaller(req, nil, wsID)
			if actx := auth.GetAuthContext(req.Context()); caller == "" && actx != nil && actx.AuthMethod != "" {
				caller = "auth:" + actx.AuthMethod
			}
			if caller == "" {
				writeJSONErr(w, http.StatusUnauthorized, "no caller identity to record the switch under")
				return
			}
			by := strings.TrimSpace(in.By)
			at, err := wsManager.SetTareTraining(req.Context(), wsID, *in.Enabled, caller, by)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]any{"ok": true, "tare_training": wsManager.GetTareTraining(wsID),
				"changed_by": caller, "on_behalf_of": by, "changed_at": at})
		})

		// B19.1 — agent accounts, each with its own balance and keys, on a double-entry ledger.
		mountAgentAccountRoutes(authed, dualToken, tenantStore)
		mountAgentApproverRoutes(authed, dualToken, agentCards)                         // B45.28
		mountAgentCardRoutes(authed, dualToken, stripeKinds)                            // B19.12, B25.6
		mountWalletCapabilityRoutes(authed, dualToken)                                  // B22.1
		mountAgentTransferRoutes(authed, dualToken)                                     // B22.3
		mountCreditLineRoutes(authed, dualToken)                                        // B22.4
		mountCompanyLoanRoutes(authed, dualToken)                                       // B22.5
		mountAgentEscrowRoutes(authed, dualToken)                                       // B22.6
		mountAgentPotRoutes(authed, dualToken)                                          // B22.7
		mountSimTradingRoutes(authed, dualToken)                                        // B22.8
		mountCashOutRoutes(authed, dualToken)                                           // B22.9
		mountMoneyAccountRoutes(authed, dualToken)                                      // B30.13
		mountMoneyPayeeRoutes(authed, dualToken)                                        // B30.16
		mountMoneyBillRoutes(authed, dualToken, billReader)                             // B41.101
		mountMandateRoutes(authed, dualToken)                                           // B30.19
		mountPermissionMandateRoutes(authed, dualToken)                                 // B40.2
		mountPaymentRequestRoutes(authed, dualToken)                                    // B40.3
		mountDeviceKeyRoutes(authed, dualToken)                                         // B40.4
		mountMoneyPaymentRoutes(authed, dualToken)                                      // B30.17
		mountMoneyPayoutRoutes(authed, dualToken)                                       // B30.21
		mountInvoiceRoutes(authed, dualToken)                                           // B30.20
		mountMarketRoutes(authed, marketStore)                                          // B20.1
		mountMarketDiscoveryRoutes(authed, marketStore)                                 // B32.50
		mountTaxProfileRoutes(authed, taxProfiles)                                      // B32.38
		mountSellerTaxRoutes(authed, sellerTax)                                         // B32.41
		mountVerificationRoutes(authed, dualToken, partnerRegistry.Verification)        // B30.4
		mountCapabilityTermsRoutes(authed, dualToken)                                   // B30.9
		mountKYAAgentRoutes(authed, kyaSvc, dualToken)                                  // B30.5
		mountAgentCardOwnerRoutes(authed, agentCards)                                   // B45.23
		mountMarketUseRoutes(authed, marketStore, r, marketMeter, dualToken)            // B20.2
		mountMarketReceiptRoutes(authed, marketStore)                                   // B32.40
		mountMarketPayoutRoutes(authed, marketStore, stripeKinds.connectFor, dualToken, // B20.5
			marketPayoutURLs{refresh: cfg.MarketPayoutRefreshURL, ret: cfg.MarketPayoutReturnURL})
		roomStore := rooms.NewStore(pool, cfg.LineageMaxShareBPS) // B32.28
		roomStore.SetMessagesPerMinute(cfg.RoomMessagesPerMinute) // B32.30
		roomStore.SetMarket(marketStore)                          // B32.31
		roomStore.SetKeys(tenantStore)                            // B32.32: each room's wallet and its key
		dualToken.SetRoomBudgets(roomStore)                       // B32.32: a room wallet's monthly limit, within the plan
		roomStore.SetContextMessages(cfg.RoomContextMessages)     // B32.33: what the room's AI reads
		roomStore.SetReportsHide(cfg.RoomReportsHide)             // B32.52: reports that take a public room off the list
		mountRoomRoutes(authed, roomStore)
		// B32.52: the operator's queue of reported rooms, and keeping, locking, unlocking or closing one (operator audit).
		r.Get("/v1/admin/rooms/reports", requireAdminOrModerator(authManager, moderatorKeys, newRoomReportQueueHandler(roomStore)))
		r.Post("/v1/admin/rooms/{roomID}/moderate", requireAdminOrModerator(authManager, moderatorKeys, newRoomModerateHandler(roomStore)))
		// B32.33: runs in a room — a room wallet's model calls go through the proxy in process, as the wallet's key.
		roomWallet := roomWalletProxy(p.HandleOpenAI, p.HandleAnthropic)
		mountRoomRunRoutes(authed, roomStore, r, roomWallet, marketMeter, dualToken)
		// B32.36: and agents take part in rooms over MCP with their own keys, as their owners' members.
		mcpServer.SetRooms(roomStore, mcpRoomDeps{lens: r, wallet: roomWallet, meter: marketMeter, agents: dualToken})
		// B32.35: room prizes, paid as a purchase of the winning contribution; one unawarded at its deadline closes.
		mountRoomPrizeRoutes(authed, roomStore, marketMeter, dualToken)
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-t.C:
					if n, err := roomStore.ClosePrizes(ctx, "", now); err != nil {
						slog.Warn("rooms: closing prizes past their deadline failed", slog.String("err", err.Error()))
					} else if n > 0 {
						slog.Info("rooms: prizes closed at their deadline without a winner", slog.Int("closed", n))
					}
				}
			}
		}()

		// B21.3 — a workspace deletes its stored answers, or asks Talyvor to delete everything.
		// See internal/storedanswers.
		storedAnswers := storedanswers.New(pool, redisClient)
		authed.Get("/v1/workspaces/{wsID}/stored-answers", storedanswers.CountsHandler(storedAnswers))
		authed.Delete("/v1/workspaces/{wsID}/stored-answers", storedanswers.DeleteHandler(storedAnswers, wsManager))
		authed.Post("/v1/workspaces/{wsID}/deletion-requests", storedanswers.FileRequestHandler(storedAnswers))
		authed.Get("/v1/workspaces/{wsID}/deletion-requests", storedanswers.RequestStatusHandler(storedAnswers))
		authed.Get("/v1/admin/deletion-requests", requireAdmin(authManager, storedanswers.AdminListHandler(storedAnswers)))
		authed.Post("/v1/admin/deletion-requests/{id}/complete", requireAdmin(authManager, storedanswers.AdminCompleteHandler(storedAnswers)))

		// B6.4 — what Tare saved in this workspace, per work item (X-Talyvor-Issue), read from the
		// tare_* columns of token_events. work_item_id is CALLER-DECLARED — a label, not an audit.
		authed.Get("/v1/workspaces/{wsID}/tare/savings", func(w http.ResponseWriter, req *http.Request) {
			savings, err := alertManager.TareSavings(req.Context(), chi.URLParam(req, "wsID"))
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]any{"by_work_item": savings})
		})

		// COMPRESSION EVIDENCE — what the rewriter actually removed from THIS
		// workspace's wire, and how much of it reached THIS workspace's bill.
		//
		// ⚠ BYTES AND COUNTS, NEVER A PERCENTAGE. token_events.savings_pct is
		// tombstoned (migration 0114) after producing a wrong customer-facing
		// number three times, and the rewriter's own SavingsPct cannot see a
		// change that len/4 integer division swallows. The honest figures are the
		// byte totals and the DENOMINATOR beside them.
		//
		// ⚠ AND estimated_path_requests IS NOT PADDING. On a request the provider
		// reported no usage for, the spend row is len(ORIGINAL)/4 — those bytes
		// left the wire and moved no money. Reporting bytes_removed without it
		// would describe a saving the customer never received.
		//
		// Expect zeroes: no workspace has the gate open (0117 backfilled every
		// one to 'disabled'), so requests=0 is the correct answer today — and it
		// is a DIFFERENT answer from the 503 an unwired reader gives.
		// Same auth + #84 workspace-isolation middleware as the PUT above.
		authed.Get("/v1/workspaces/{wsID}/compression/savings", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			days := 30
			if v := req.URL.Query().Get("days"); v != "" {
				if n, err := strconv.Atoi(v); err == nil {
					days = n
				}
			}
			savings, err := api.ReadCompressionSavings(req.Context(), compressionStore, wsID, days, time.Now())
			if errors.Is(err, api.ErrNoCompressionSavingsStore) {
				writeJSONErr(w, http.StatusServiceUnavailable, "compression savings reader not configured")
				return
			}
			if err != nil {
				writeJSONErr(w, http.StatusBadGateway, "could not read compression savings")
				return
			}
			writeJSONOK(w, http.StatusOK, savings)
		})

		// DISTILL EVIDENCE — a workspace-scoped COUNT of documents this workspace had converted.
		//
		// ⚠ DELIBERATELY NOT /v1/api/distill/summary: those counters are PROCESS-GLOBAL, so serving
		// them to a customer would put other tenants' document counts on their screen. This reads
		// token_events filtered by workspace_id — the scoping is in the WHERE clause, not in a
		// caller's discipline. Same auth + #84 workspace-isolation middleware as the PUT above.
		//
		// A COUNT, never a saving: 'convert' rows carry the saving implicitly in their lower
		// input_tokens, and the savings metric reads 0 for every format except HTML at the tier the
		// request path uses — so there is no honest saving figure to render here.
		authed.Get("/v1/workspaces/{wsID}/distill/usage", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			days := 30
			if v := req.URL.Query().Get("days"); v != "" {
				if n, err := strconv.Atoi(v); err == nil {
					days = n
				}
			}
			usage, err := api.ReadDistillUsage(req.Context(), distillUsageStore, wsID, days, time.Now())
			if errors.Is(err, api.ErrNoDistillUsageStore) {
				// 503, never 200-with-zeroes: "nothing wired" and "converted nothing" are
				// different answers and the screen must be able to tell them apart.
				writeJSONErr(w, http.StatusServiceUnavailable, "distill usage reader not configured")
				return
			}
			if err != nil {
				writeJSONErr(w, http.StatusBadGateway, "could not read distill usage")
				return
			}
			writeJSONOK(w, http.StatusOK, usage)
		})

		// Phase-2 Stage 2.0 shared-cache governance gate (exact cache): per-tenant
		// opt-in for cross-user cache pooling. Default false (private), so the
		// request path stays inert until an admin opts a workspace in here AND the
		// global LENS_CACHE_POOLABLE_ENABLED switch is on. This route sits behind
		// the same auth + #84 workspace-isolation middleware as the /distill route.
		authed.Put("/v1/workspaces/{wsID}/cache-poolable", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				CachePoolable bool `json:"cache_poolable"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := wsManager.SetCachePoolable(req.Context(), wsID, in.CachePoolable); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			ws, _ := wsManager.GetWorkspace(wsID)
			writeJSONOK(w, http.StatusOK, map[string]any{
				"ok":             true,
				"cache_poolable": ws.CachePoolable,
			})
		})

		// Per-workspace CONSENT to cost-optimised routing on concrete (named)
		// models. Default false: an explicitly named model is HONOURED exactly —
		// never downgraded — unless the workspace opts in here (the founder's rule:
		// quality shall not be compromised without consent). The "auto" pseudo-model
		// and the X-Talyvor-Auto-Route header route regardless of this flag. Same
		// auth + workspace-isolation middleware as the /cache-poolable route.
		authed.Put("/v1/workspaces/{wsID}/cost-optimize-routing", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				CostOptimizeRouting bool `json:"cost_optimize_routing"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := wsManager.SetCostOptimizeRouting(req.Context(), wsID, in.CostOptimizeRouting); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			ws, _ := wsManager.GetWorkspace(wsID)
			writeJSONOK(w, http.StatusOK, map[string]any{
				"ok":                    true,
				"cost_optimize_routing": ws.CostOptimizeRouting,
			})
		})

		// Per-workspace opt-in for cross-tenant DISTILL-cache sharing (S0). A
		// separate consent from cache-poolable — distill artifacts are
		// document-derived. Default false (private); cross-tenant serving also
		// requires LENS_DISTILL_POOLABLE_ENABLED, and the owner opted in when it
		// shared the artifact (switching off stops new sharing only — B21.3).
		authed.Put("/v1/workspaces/{wsID}/distill-poolable", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				DistillPoolable bool `json:"distill_poolable"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := wsManager.SetDistillPoolable(req.Context(), wsID, in.DistillPoolable); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			ws, _ := wsManager.GetWorkspace(wsID)
			writeJSONOK(w, http.StatusOK, map[string]any{
				"ok":               true,
				"distill_poolable": ws.DistillPoolable,
			})
		})

		authed.Get("/v1/attribution/branch", newAttributionBranchHandler(attrStore))
		authed.Get("/v1/attribution/top", newAttributionTopHandler(attrStore))

		authed.Get("/v1/sessions/{sessionID}", newSessionGetHandler(sessionTracker))

		authed.Get("/v1/sessions/{sessionID}/summary", func(w http.ResponseWriter, req *http.Request) {
			sessionID := chi.URLParam(req, "sessionID")
			// Authz (#146 P3): same IDOR as GET /v1/sessions/{id} — own session only.
			if sess, ok := sessionTracker.GetSession(sessionID); !ok || !callerOwns(req, sess.WorkspaceID) {
				writeJSONErr(w, http.StatusNotFound, "session not found")
				return
			}
			summary := sessionTracker.SummariseSession(req.Context(), sessionID)
			if summary.TurnCount == 0 && summary.StartedAt.IsZero() {
				writeJSONErr(w, http.StatusNotFound, "session not found")
				return
			}
			writeJSONOK(w, http.StatusOK, summary)
		})

		authed.Get("/v1/sessions", newSessionsListHandler(sessionTracker))

		batchGate.post(authed, "/v1/batch/submit", newBatchSubmitHandler(batchRouter))

		batchGate.get(authed, "/v1/batch/status/{requestID}", newBatchStatusHandler(batchRouter))

		batchGate.get(authed, "/v1/batch/jobs", func(w http.ResponseWriter, req *http.Request) {
			// W6.38: scoped to the CALLER's workspace, the same way GET /v1/sessions is
			// (newSessionsListHandler). BatchJob carries Prompt and Response, so an
			// unscoped list is a cross-tenant read of customer prompt text.
			wsID, ok := applyPhase2WSID(req, req.URL.Query().Get("workspace_id"))
			if !ok {
				phase2Forbidden(w)
				return
			}
			writeJSONOK(w, http.StatusOK, batchRouter.ListJobs(wsID))
		})

		// Eval pipeline — test cases, suite runs, and history. RunSuite
		// is synchronous from the caller's perspective; up to 10 cases
		// execute concurrently inside the handler.
		authed.Post("/v1/eval/cases", newEvalCaseCreateHandler(evalPipeline))

		authed.Get("/v1/eval/cases", func(w http.ResponseWriter, req *http.Request) {
			wsID, ok := applyPhase2WSID(req, req.URL.Query().Get("workspace_id"))
			if !ok {
				phase2Forbidden(w)
				return
			}
			var tags []string
			if t := req.URL.Query().Get("tags"); t != "" {
				tags = strings.Split(t, ",")
			}
			cases, err := evalPipeline.ListTestCases(req.Context(), wsID, tags)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, cases)
		})

		authed.Post("/v1/eval/run", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				WorkspaceID string   `json:"workspace_id"`
				Tags        []string `json:"tags"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			eff, ok := applyPhase2WSID(req, in.WorkspaceID)
			if !ok {
				phase2Forbidden(w)
				return
			}
			in.WorkspaceID = eff
			summary, err := evalPipeline.RunSuite(req.Context(), in.WorkspaceID, in.Tags)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, summary)
		})

		authed.Get("/v1/eval/runs/{runID}", newEvalRunGetHandler(evalPipeline))

		authed.Get("/v1/eval/runs/{runID}/results", func(w http.ResponseWriter, req *http.Request) {
			runID := chi.URLParam(req, "runID")
			// Authz (#146 P3): same IDOR as GET /v1/eval/runs/{id} — own run only.
			if run, err := evalPipeline.GetRun(req.Context(), runID); err != nil || run == nil || !callerOwns(req, run.WorkspaceID) {
				writeJSONErr(w, http.StatusNotFound, "run not found")
				return
			}
			results, err := evalPipeline.GetResults(req.Context(), runID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, results)
		})

		authed.Get("/v1/eval/runs", newEvalRunsListHandler(evalPipeline))

		// ── Evaluation pipeline: golden datasets, regression runs, A/B
		// significance verdicts, scheduling (Upgrade 17). Workspace-scoped;
		// every path is background/on-demand, off the request hot path.
		authed.Post("/v1/workspaces/{wsID}/eval/datasets", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			ds, err := evalPipeline.CreateDataset(req.Context(), eval.Dataset{
				WorkspaceID: wsID, Name: in.Name, Description: in.Description,
			})
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, ds)
		})

		authed.Get("/v1/workspaces/{wsID}/eval/datasets", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			list, err := evalPipeline.ListDatasets(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, list)
		})

		authed.Post("/v1/workspaces/{wsID}/eval/datasets/{dsID}/cases", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			dsID := chi.URLParam(req, "dsID")
			// B-Lens: dsID is a bare id — verify the dataset belongs to the caller's authorized workspace
			// (GetDataset is workspace-scoped) before adding a case, else a member of another workspace
			// could inject cases into this dataset (poisoning its eval/regression gates). 404, no oracle.
			if _, err := evalPipeline.GetDataset(req.Context(), wsID, dsID); err != nil {
				writeJSONErr(w, http.StatusNotFound, "dataset not found")
				return
			}
			var in eval.TestCase
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			in.WorkspaceID = wsID
			created, err := evalPipeline.AddDatasetCase(req.Context(), dsID, in)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, created)
		})

		authed.Post("/v1/workspaces/{wsID}/eval/run", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				DatasetID string      `json:"dataset_id"`
				Target    eval.Target `json:"target"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			// B-Lens: dataset_id comes from the body — verify it belongs to the caller's authorized
			// workspace before running (RunEval loads its cases WHERE dataset_id only), else a member of
			// another workspace could read that dataset's prompts/expected-outputs. 404, no oracle.
			if _, err := evalPipeline.GetDataset(req.Context(), wsID, in.DatasetID); err != nil {
				writeJSONErr(w, http.StatusNotFound, "dataset not found")
				return
			}
			run, err := evalPipeline.RunEval(req.Context(), wsID, in.DatasetID, in.Target)
			if errors.Is(err, eval.ErrCostCapExceeded) {
				// Refuse the run rather than silently burn spend.
				writeJSONErr(w, http.StatusPaymentRequired, err.Error())
				return
			}
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, run)
		})

		authed.Get("/v1/workspaces/{wsID}/eval/runs", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			runs, err := evalPipeline.ListRuns(req.Context(), wsID, 20)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, runs)
		})

		authed.Post("/v1/workspaces/{wsID}/eval/schedules", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				DatasetID   string `json:"dataset_id"`
				IntervalSec int    `json:"interval_seconds"`
				Enabled     bool   `json:"enabled"`
				TargetModel string `json:"target_model"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			s, err := evalPipeline.CreateSchedule(req.Context(), eval.Schedule{
				WorkspaceID: wsID, DatasetID: in.DatasetID, IntervalSec: in.IntervalSec,
				Enabled: in.Enabled, TargetModel: in.TargetModel,
			})
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, s)
		})

		authed.Get("/v1/workspaces/{wsID}/eval/schedules", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			list, err := evalPipeline.ListSchedules(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, list)
		})

		// ── PoVI receipts (Token Economy Phase 1, Part 1). A node submits its
		// signed receipt; Lens verifies it against the node's registered
		// pubkey, records it for audit, and (only if minting is explicitly
		// enabled) provisionally mints. GET endpoints are audit + status. A
		// receipt is ATTESTATION + TAMPER-EVIDENCE, never proof of honest
		// computation.
		econ.post(authed, "/v1/workspaces/{wsID}/povi/receipts", newPoviReceiptSubmitHandler(poviProcessor, poviStakeManager))

		econ.get(authed, "/v1/povi/receipts", newPoviReceiptsHandler(poviStore))

		econ.get(authed, "/v1/povi/status", func(w http.ResponseWriter, req *http.Request) {
			total, verified, _ := poviStore.Stats(req.Context())
			writeJSONOK(w, http.StatusOK, map[string]any{
				"minting_enabled":   cfg.POVIMintingEnabled,
				"receipts_total":    total,
				"receipts_verified": verified,
				"attestation_only":  true,
				"warning": "Receipts are ATTESTATION + TAMPER-EVIDENCE, NOT proof of honest computation. " +
					"Provisional receipt-based minting is UNSAFE without stake + challenge (Parts 2/3) and is " +
					"OFF by default; when off, receipts are verified + recorded for audit but NO LENS is minted. " +
					"Separately, the existing trust-based compute mint (RecordServedRequest) remains active and " +
					"unsecured until Part 3 — PoVI is designed to replace it.",
			})
		})

		// ── PoVI staking (Part 2): node collateral that gates minting-
		// eligibility and is slashable (Part 3). Staking does NOT gate serving —
		// an unstaked node still serves, it's just not minting-eligible.
		econ.post(authed, "/v1/povi/nodes/{nodeID}/stake", func(w http.ResponseWriter, req *http.Request) {
			nodeID := chi.URLParam(req, "nodeID")
			if !requireNodeOwnership(w, req, poviStakeManager, nodeID) {
				return
			}
			var in struct {
				Amount int64 `json:"amount_ulens"` // µLENS (SEC-2)
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			st, err := poviStakeManager.Stake(req.Context(), nodeID, in.Amount)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, st)
		})

		econ.post(authed, "/v1/povi/nodes/{nodeID}/unbond", func(w http.ResponseWriter, req *http.Request) {
			nodeID := chi.URLParam(req, "nodeID")
			if !requireNodeOwnership(w, req, poviStakeManager, nodeID) {
				return
			}
			if err := poviStakeManager.Unbond(req.Context(), nodeID); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			st, _ := poviStakeManager.Get(req.Context(), nodeID)
			writeJSONOK(w, http.StatusOK, st)
		})

		econ.post(authed, "/v1/povi/nodes/{nodeID}/release", func(w http.ResponseWriter, req *http.Request) {
			nodeID := chi.URLParam(req, "nodeID")
			if !requireNodeOwnership(w, req, poviStakeManager, nodeID) {
				return
			}
			if err := poviStakeManager.Release(req.Context(), nodeID); err != nil {
				// Unbonding-not-elapsed is a client error (try again later).
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			st, _ := poviStakeManager.Get(req.Context(), nodeID)
			writeJSONOK(w, http.StatusOK, st)
		})

		econ.get(authed, "/v1/povi/nodes/{nodeID}/stake", func(w http.ResponseWriter, req *http.Request) {
			nodeID := chi.URLParam(req, "nodeID")
			if !requireNodeOwnership(w, req, poviStakeManager, nodeID) {
				return
			}
			st, err := poviStakeManager.Get(req.Context(), nodeID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if st == nil {
				writeJSONOK(w, http.StatusOK, map[string]any{"node_id": nodeID, "staked": false, "eligible": false})
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]any{
				"stake":     st,
				"eligible":  poviStakeManager.IsEligible(req.Context(), nodeID),
				"min_stake": cfg.POVIMinStake,
			})
		})

		econ.get(authed, "/v1/povi/staking/status", func(w http.ResponseWriter, req *http.Request) {
			status, err := poviStakeManager.Status(req.Context())
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, status)
		})

		// ── PoVI challenge-and-slash (Part 3). Lens publishes its challenge
		// pubkey for nodes to pin; audits challenges; allows a manual challenge
		// (admin/testing); and reports the honest economic-deterrent summary.
		econ.get(authed, "/v1/povi/pubkey", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, map[string]string{
				"ed25519_pubkey": povi.EncodePublicKey(lensChallengePub),
				"purpose":        "PoVI Part-3 challenge signing — pin this to verify challenges are from Lens",
			})
		})

		econ.get(authed, "/v1/povi/challenges", func(w http.ResponseWriter, req *http.Request) {
			// Authz (#146 P3): node-keyed read. A specific node must be owned by
			// the caller; the unfiltered list (no node) is admin-only — a tenant
			// can't enumerate every node's challenges.
			node := req.URL.Query().Get("node")
			if node == "" {
				if _, isAdmin := auth.WorkspaceIdentity(req.Context()); !isAdmin {
					writeJSONErr(w, http.StatusForbidden, "forbidden: specify a node you own, or use an admin credential")
					return
				}
			} else if !requireNodeOwnership(w, req, poviStakeManager, node) {
				return
			}
			list, err := challengeStore.List(req.Context(), node, "")
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, list)
		})

		econ.get(authed, "/v1/povi/challenges/{id}", newChallengeGetHandler(challengeStore))

		econ.post(authed, "/v1/povi/challenges/issue", newChallengeIssueHandler(poviStore, poviChallenger))

		econ.get(authed, "/v1/povi/security/status", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, map[string]any{
				"minting_enabled":         cfg.POVIMintingEnabled,
				"trustful_compute_mint":   cfg.TrustfulComputeMintEnabled,
				"challenge_rate":          cfg.POVIChallengeRate,
				"slash_fraction":          cfg.POVISlashFraction,
				"positions_per_challenge": poviChallenger.PositionsPerChallenge(),
				"min_stake":               cfg.POVIMinStake,
				"model": "PROBABILISTIC, NOT ABSOLUTE. Security is economic: a rational node with stake at risk " +
					"finds cheating unprofitable when expected_cost = P(challenge) × slash_amount > gain_from_cheating. " +
					"A low challenge rate leaves some bad receipts unchallenged — this is NOT a cryptographic guarantee " +
					"of honest computation; receipts are attestation + tamper-evidence, and challenge-and-slash makes " +
					"cheating economically irrational, not impossible.",
				"retirement_note": "With Parts 1+2+3 in place, receipt minting is now SAFE to enable " +
					"(LENS_POVI_MINTING_ENABLED=true) — operator decision, default OFF. Once enabled, retire the legacy " +
					"trust-mint with LENS_TRUSTFUL_COMPUTE_MINT_ENABLED=false.",
			})
		})

		// Guardrails: per-workspace safety policy + pre-flight check.
		// Policy changes apply immediately; the engine reads on every
		// proxy request and there is no per-request policy cache.
		authed.Get("/v1/guardrails/policy", newGuardrailsPolicyGetHandler(guardrailsEngine))

		authed.Put("/v1/guardrails/policy", newGuardrailsPolicyPutHandler(guardrailsEngine))

		authed.Post("/v1/guardrails/check", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				Prompt      string `json:"prompt"`
				WorkspaceID string `json:"workspace_id"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			eff, ok := applyPhase2WSID(req, in.WorkspaceID)
			if !ok {
				phase2Forbidden(w)
				return
			}
			in.WorkspaceID = eff
			result := guardrailsEngine.Check(req.Context(), in.WorkspaceID, in.Prompt, nil)
			writeJSONOK(w, http.StatusOK, result)
		})

		// ─── workspace-scoped guardrails config (Upgrade 13) ───
		// REST surface per workspace. POST/PATCH replace the policy; DELETE
		// reverts to the default. The /test dry-run previews input + output
		// triggers without affecting traffic (output preview runs even when
		// the output stage is off).
		authed.Get("/v1/workspaces/{wsID}/guardrails", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, guardrailsEngine.GetPolicy(chi.URLParam(req, "wsID")))
		})
		setGuardrails := func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in guardrails.GuardrailPolicy
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := guardrailsEngine.SetPolicy(req.Context(), wsID, in); err != nil {
				// Policy is live on this node (map-first) but did not persist — tell
				// the admin so they retry (it won't survive a restart otherwise).
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, guardrailsEngine.GetPolicy(wsID))
		}
		authed.Post("/v1/workspaces/{wsID}/guardrails", setGuardrails)
		authed.Patch("/v1/workspaces/{wsID}/guardrails", setGuardrails)
		authed.Delete("/v1/workspaces/{wsID}/guardrails", func(w http.ResponseWriter, req *http.Request) {
			if err := guardrailsEngine.DeletePolicy(req.Context(), chi.URLParam(req, "wsID")); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})
		authed.Get("/v1/workspaces/{wsID}/guardrails/test", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			q := req.URL.Query()
			out := map[string]any{}
			if p := q.Get("prompt"); p != "" {
				out["input"] = guardrailsEngine.Check(req.Context(), wsID, p, nil)
			}
			if c := q.Get("content"); c != "" {
				out["output"] = guardrailsEngine.CheckOutputPreview(wsID, c)
			}
			writeJSONOK(w, http.StatusOK, out)
		})

		// Audit export — synchronous download. The exporter streams rows
		// straight to the http.ResponseWriter so a 100k-record CSV never
		// materialises in memory. Format defaults to JSON; query params
		// drive the WHERE filters and the LIMIT cap.
		authed.Get("/v1/audit/export", newAuditExportHandler(auditExporter))

		authed.Post("/v1/audit/webhook", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				WebhookURL string             `json:"webhook_url"`
				Filter     audit.ExportFilter `json:"filter"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.WebhookURL == "" {
				writeJSONErr(w, http.StatusBadRequest, "webhook_url required")
				return
			}
			// Authz (#146): non-admin scoped to own workspace; admin honors the
			// filter (empty workspace = all tenants, admin-only).
			effWS, _, ok := effectiveWorkspaceID(req, in.Filter.WorkspaceID)
			if !ok {
				writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
				return
			}
			in.Filter.WorkspaceID = effWS
			// Fire-and-forget so the caller doesn't block on a slow SIEM.
			// Use a fresh context detached from the request so cancellation
			// of the HTTP connection doesn't kill the export mid-flight.
			go func(filter audit.ExportFilter, url string) {
				if err := auditExporter.ExportWebhook(context.Background(), url, filter); err != nil {
					logger.Warn("audit: webhook export failed",
						slog.String("url", url),
						slog.String("err", err.Error()),
					)
				}
			}(in.Filter, in.WebhookURL)
			writeJSONOK(w, http.StatusAccepted, map[string]any{"ok": true, "message": "export started"})
		})

		// API-key pool: enterprise customers attach multiple keys per
		// provider to escape per-key rate limits. Raw key material is
		// kept in memory only; the pool's Stats API never returns it.
		authed.Post("/v1/api/keys/pool", requireAdmin(authManager, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				Provider  string `json:"provider"`
				Key       string `json:"key"`
				Alias     string `json:"alias"`
				RateLimit int    `json:"rate_limit"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			pk, err := keyPool.Add(in.Provider, in.Key, in.Alias, in.RateLimit)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, map[string]string{
				"id":       pk.ID,
				"provider": pk.Provider,
				"alias":    pk.Alias,
			})
		})))

		// ⚠ NO GATE, while POST and DELETE on this same path are requireAdmin.
		// Measured and recorded in W6.18; closing it is a decision, not a repair.
		authed.Get("/v1/api/keys/pool", requireAdmin(authManager, newKeyPoolStatsHandler(keyPool)))

		authed.Delete("/v1/api/keys/pool/{keyID}", requireAdmin(authManager, http.HandlerFunc(newPoolKeyDeleteHandler(keyPool))))

		// Fallback chain inspection and override. The router is in-memory;
		// updates here are not persisted — restarting the binary resets
		// chains to the defaults.
		// ⚠ NO GATE, while PUT on this same path is requireAdmin. Same note as above.
		authed.Get("/v1/api/fallback/chains", requireAdmin(authManager, newFallbackChainsHandler(fallbackRouter)))

		authed.Put("/v1/api/fallback/chains/{provider}", requireAdmin(authManager, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			provider := chi.URLParam(req, "provider")
			var targets []fallback.FallbackTarget
			if err := json.NewDecoder(req.Body).Decode(&targets); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			fallbackRouter.SetChain(provider, targets)
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})))

		// Prompt management — named, versioned prompts that teams edit
		// without redeploys. Every write goes through the Manager so the
		// in-memory cache stays consistent with the DB.
		authed.Post("/v1/prompts", newPromptCreateHandler(promptManager))

		authed.Get("/v1/prompts", func(w http.ResponseWriter, req *http.Request) {
			wsID, ok := applyPhase2WSID(req, req.URL.Query().Get("workspace_id"))
			if !ok {
				phase2Forbidden(w)
				return
			}
			list, err := promptManager.List(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, list)
		})

		authed.Get("/v1/prompts/{name}", newPromptGetHandler(promptManager))

		authed.Put("/v1/prompts/{name}", func(w http.ResponseWriter, req *http.Request) {
			name := chi.URLParam(req, "name")
			var in struct {
				Content     string `json:"content"`
				Description string `json:"description"`
				WorkspaceID string `json:"workspace_id"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			// Authz (#146): a non-admin may mutate only its OWN prompt; admin
			// honors the body (empty → "default").
			eff, _, ok := effectiveWorkspaceID(req, in.WorkspaceID)
			if !ok {
				writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
				return
			}
			in.WorkspaceID = eff
			if in.WorkspaceID == "" {
				in.WorkspaceID = "default"
			}
			updated, err := promptManager.Update(req.Context(), name, in.WorkspaceID, in.Content, in.Description)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, updated)
		})

		authed.Get("/v1/prompts/{name}/history", func(w http.ResponseWriter, req *http.Request) {
			name := chi.URLParam(req, "name")
			wsID, ok := applyPhase2WSID(req, req.URL.Query().Get("workspace_id"))
			if !ok {
				phase2Forbidden(w)
				return
			}
			hist, err := promptManager.History(req.Context(), name, wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, hist)
		})

		authed.Post("/v1/prompts/{name}/rollback", func(w http.ResponseWriter, req *http.Request) {
			name := chi.URLParam(req, "name")
			var in struct {
				Version     int    `json:"version"`
				WorkspaceID string `json:"workspace_id"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			// Authz (#146): a non-admin may roll back only its OWN prompt; admin
			// honors the body (empty → "default").
			eff, _, ok := effectiveWorkspaceID(req, in.WorkspaceID)
			if !ok {
				writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
				return
			}
			in.WorkspaceID = eff
			if in.WorkspaceID == "" {
				in.WorkspaceID = "default"
			}
			rolled, err := promptManager.Rollback(req.Context(), name, in.WorkspaceID, in.Version)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, rolled)
		})

		authed.Get("/v1/prompts/{name}/diff", func(w http.ResponseWriter, req *http.Request) {
			name := chi.URLParam(req, "name")
			wsID, ok := applyPhase2WSID(req, req.URL.Query().Get("workspace_id"))
			if !ok {
				phase2Forbidden(w)
				return
			}
			fromV, _ := strconv.Atoi(req.URL.Query().Get("from"))
			toV, _ := strconv.Atoi(req.URL.Query().Get("to"))
			if fromV <= 0 || toV <= 0 {
				writeJSONErr(w, http.StatusBadRequest, "from and to query params required (positive integers)")
				return
			}
			d, err := promptManager.Diff(req.Context(), name, wsID, fromV, toV)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, d)
		})

		// B23.1: {"request_id", "signal": "negative"|"repeat"} removes the stored answer that request was
		// given; the older {"prompt_hash", "signal"} form still reaches the quality scorer.
		authed.Post("/v1/feedback", proxy.AnswerFeedbackHandler(p, pool, func(ctx context.Context, wsID, promptHash, signal string) (bool, error) {
			return qualityScorer.RecordFeedback(ctx, wsID, promptHash, quality.FeedbackSignal(signal))
		}))
	})

	// Verify the TLS cert cache directory exists and is writable before
	// handing control to the autocert manager.  Failing here is cheap;
	// failing at the first TLS handshake burns Let's Encrypt rate-limit quota.
	if cfg.TLSDomain != "" {
		if err := checkTLSCacheDir(cfg.TLSCacheDir); err != nil {
			return fmt.Errorf("TLS setup: %w", err)
		}
	}

	servers, serverErr := startServers(cfg, r, logger)

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-serverErr:
		if err != nil {
			return err
		}
	}

	// Graceful drain (Upgrade 7): mark this instance draining so /readyz starts
	// returning 503 and the load balancer pulls it from rotation; let in-flight
	// requests finish via srv.Shutdown; then deregister and close the gossip
	// subscription. When HA is disabled the registry/breaker calls are no-ops
	// and the drain timeout stays at the original 15s, so shutdown is unchanged.
	drainTimeout := 15 * time.Second
	if cfg.HAEnabled {
		drainTimeout = cfg.HADrainTimeout
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	if err := haComps.registry.SetDraining(drainCtx); err != nil {
		logger.Warn("ha: set draining failed", slog.String("err", err.Error()))
	}
	if cfg.HAEnabled {
		// Give the load balancer a moment to observe the 503 from /readyz
		// before we stop accepting connections, so no new request is dropped.
		select {
		case <-time.After(2 * time.Second):
		case <-drainCtx.Done():
		}
	}

	if err := servers.shutdown(drainCtx); err != nil {
		return err
	}

	if err := haComps.registry.Deregister(context.Background()); err != nil {
		logger.Warn("ha: deregister failed", slog.String("err", err.Error()))
	}
	if err := haComps.breaker.Close(); err != nil {
		logger.Warn("ha: breaker close failed", slog.String("err", err.Error()))
	}

	return nil
}

// loadOrGenChallengeKey returns Lens's ed25519 challenge-signing keypair. A
// base64 seed/private key (LENS_POVI_CHALLENGE_KEY) gives a STABLE key so a Lens
// restart doesn't invalidate the pubkey nodes have pinned (which would make
// honest nodes fail challenges). Empty → ephemeral key + a loud warning.
//
// ⚠ SET-BUT-INVALID EXITS; UNSET FALLS BACK. This deliberately mirrors the LENS_JWT_PRIVATE_KEY
// block above (main.go, "Parse or generate the EC P-256 JWT signing key"), which is the same
// problem — a long-lived asymmetric key that must survive restarts — and which already resolved
// it: a key that is PRESENT and unparseable is a configuration error, not a request to generate
// one, so it exits rather than proceeding.
//
// The old behaviour warned and generated an ephemeral key, which made a MISCONFIGURED key
// indistinguishable in effect from NO key. That is the worse of the two: an operator who set the
// variable believes persistence is configured, so the one line telling them otherwise is the line
// they are least likely to be looking for. Pinned node pubkeys then break silently on every
// restart and honest nodes fail challenges — the exact outcome the variable exists to prevent.
//
// Unset is left as a warn-and-generate, also matching JWT: a dev run with no key must still boot.
//
// Coordination note: if the JWT-key work lands a shared helper for "load this key or fail", move
// BOTH call sites onto it. This block is kept deliberately small and local so that rebase is a
// deletion rather than a merge.
// applyCatalogOverrides layers LENS_MODEL_CATALOG_OVERRIDES (Upgrade 16) onto the embedded model
// catalog, so an operator can add or reprice a model without a rebuild.
//
// ⚠ IT DECODES THROUGH catalog.DecodeOverrides, NEVER THROUGH A BARE json.Unmarshal INTO
// []catalog.Model. A reprice states a price; decoded into a fresh Model, every fact the operator did
// not restate arrived as its zero value and replaced the seeded truth. MEASURED: a price-only
// override of gpt-4o blanked its capabilities and turned a 200 streaming vision request into a 422
// (internal/proxy/reprice_override_vision_test.go), and dropped it out of ByProvider("openai") — the
// fallback-anchor population, NOT the redirect's (that walks modality.providerPreference).
//
// ⚠ AND IT IS A FUNCTION SO THE BOOT BEHAVIOUR IS TESTABLE. Inline in main() this block ran only on a
// real process start, which is exactly the shape #157 called out: the closure that matters is the
// WIRING, and a rule nothing can execute is a rule nothing can red.
func applyCatalogOverrides(raw string, logger *slog.Logger) {
	if raw == "" {
		return
	}
	overrides, err := catalog.DecodeOverrides([]byte(raw))
	if err != nil {
		logger.Warn("catalog: invalid LENS_MODEL_CATALOG_OVERRIDES", slog.String("err", err.Error()))
		return
	}
	catalog.LoadOverrides(overrides)
	logger.Info("catalog: applied model overrides", slog.Int("count", len(overrides)))
}

func loadOrGenChallengeKey(b64 string, logger *slog.Logger) (ed25519.PublicKey, ed25519.PrivateKey) {
	if b64 != "" {
		if raw, err := base64.StdEncoding.DecodeString(b64); err == nil {
			switch len(raw) {
			case ed25519.SeedSize:
				priv := ed25519.NewKeyFromSeed(raw)
				return priv.Public().(ed25519.PublicKey), priv
			case ed25519.PrivateKeySize:
				priv := ed25519.PrivateKey(raw)
				return priv.Public().(ed25519.PublicKey), priv
			}
		}
		logger.Error("LENS_POVI_CHALLENGE_KEY is SET but is not a valid base64 ed25519 seed or private key. "+
			"Refusing to start rather than silently substituting an ephemeral key, which would invalidate "+
			"every pinned node pubkey on this restart and fail honest challenges.",
			slog.String("remedy", "generate one with: openssl rand -base64 32   (32 raw bytes = an ed25519 seed); "+
				"or unset the variable entirely to run with an ephemeral key"),
			slog.Int("decoded_bytes", decodedLen(b64)),
			slog.Int("want_bytes", ed25519.SeedSize), slog.Int("or_bytes", ed25519.PrivateKeySize))
		os.Exit(1)
	}
	logger.Warn("PoVI challenge key generated EPHEMERALLY — set LENS_POVI_CHALLENGE_KEY (base64 ed25519 seed) in production; a restart otherwise invalidates pinned node pubkeys and would fail honest challenges")
	pub, priv, _ := povi.GenerateNodeKey()
	return pub, priv
}

// decodedLen reports how many bytes a base64 string decodes to, or -1 when it is not valid base64.
// Reported in the failure above so the operator can tell "wrong length" (a truncated paste) from
// "not base64 at all" (a raw PEM, a hex string) without echoing any key material.
func decodedLen(b64 string) int {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return -1
	}
	return len(raw)
}

func writeJSONOK(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONErr(w http.ResponseWriter, status int, msg string) {
	// 5xx REDACTION (defense-in-depth, centralised). An internal error must not
	// leak its raw string — DB/driver messages, file paths, internal hosts,
	// stack context — to the caller. For status >= 500 we log the full detail
	// server-side (through the app's slog default, wired in main) and return a
	// generic body. 4xx messages are legitimately client-facing (validation,
	// not-found, auth) and pass through unchanged. Doing this in the ONE helper
	// means no 500 call site (there are ~77 passing err.Error()) can forget to
	// redact — the information-disclosure class is closed by construction.
	if status >= http.StatusInternalServerError {
		slog.Error("request failed", slog.Int("status", status), slog.String("detail", msg))
		msg = "internal server error"
	}
	writeJSONOK(w, status, map[string]string{"error": msg})
}

// ─── tenant isolation (cross-tenant BOLA guard) ──────────────────
//
// The authed group authenticates any valid credential, but the
// /v1/workspaces/{wsID}/... handlers used to trust the {wsID} path segment
// outright. A holder of workspace A's key could therefore read/modify
// workspace B's config and budgets and — worst of all — mint API keys for
// workspace B (full tenant takeover). These helpers enforce that a caller may
// only act on its own workspace, with the global admin key exempted.

// workspaceAuthorized is the pure tenant-isolation decision: the global admin
// (isAdmin) may act on any workspace; every other caller may act only on the
// workspace its credential belongs to. Deny-by-default — an empty caller
// workspace never matches a non-empty target.
func workspaceAuthorized(callerWorkspaceID string, isAdmin bool, targetWorkspaceID string) bool {
	if isAdmin {
		return true
	}
	return callerWorkspaceID != "" && callerWorkspaceID == targetWorkspaceID
}

// callerWorkspaceID resolves the authenticated caller's workspace and whether
// it is the global admin, handling both AuthMiddleware credential paths:
//   - DB workspace/team keys take the fast path, which populates only the
//     APIKey context slot (auth.GetAPIKey). These are never admin.
//   - The global admin key and JWT bearer tokens take the Manager fallback,
//     which populates the AuthContext slot (auth.GetAuthContext). Only the
//     global key carries IsAdmin. NOTE: both the global key and JWTs synthesize
//     an APIKey with ID=="global", so APIKey.ID is NOT a safe admin signal —
//     AuthContext.IsAdmin is the authoritative one, which is why it is checked
//     first.
func callerWorkspaceID(ctx context.Context) (workspaceID string, isAdmin bool) {
	if actx := auth.GetAuthContext(ctx); actx != nil {
		return actx.WorkspaceID, actx.IsAdmin
	}
	if k := auth.GetAPIKey(ctx); k != nil {
		return k.WorkspaceID, false
	}
	return "", false
}

// effectiveWorkspaceID resolves which workspace a NON-{wsID}-path route may act
// on, deriving identity from the authenticated credential rather than caller
// input. It closes the cross-tenant authorization cluster (#146): routes that
// took workspace_id from a query/body param trusted attacker-chosen input,
// because workspaceIsolationMiddleware only guards the {wsID} PATH segment.
//
//   - A NON-ADMIN caller is ALWAYS forced to its own workspace; `requested`
//     (whatever the query/body said) is ignored — it can never name another
//     tenant. An honest caller naming its own workspace is unaffected.
//   - The global ADMIN honors `requested`; an empty value preserves each
//     handler's admin-wide semantics (e.g. audit export's all-tenant dump).
//
// ok is false ONLY when a non-admin has no resolvable workspace — the caller
// must 403 rather than fall through to a shared/"default"/all-tenant path.
func effectiveWorkspaceID(r *http.Request, requested string) (wsID string, isAdmin bool, ok bool) {
	caller, admin := callerWorkspaceID(r.Context())
	if admin {
		return requested, true, true
	}
	if caller == "" {
		return "", false, false
	}
	return caller, false, true
}

// workspaceIsolationMiddleware enforces tenant isolation on every routed
// request that carries a {wsID} path param. Routes without a {wsID} param
// (auth, catalog, proxy, global status) pass straight through. chi resolves
// URL params before a group's Use chain executes, so chi.URLParam(r,"wsID") is
// populated here.
func workspaceIsolationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wsID := chi.URLParam(r, "wsID"); wsID != "" {
			callerWS, isAdmin := callerWorkspaceID(r.Context())
			if !workspaceAuthorized(callerWS, isAdmin, wsID) {
				writeJSONErr(w, http.StatusForbidden, "forbidden: credential not authorized for this workspace")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// parseSinceParam turns a `?since=<RFC3339>` query value into a
// time.Time. Empty / unparseable input returns the zero value,
// which the attribution store treats as "all time".
func parseSinceParam(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// ensureBatchFlag stamps batch_eligible:true into the body so the
// BatchRouter's body-only IsEligible sees the trigger that arrived via
// the X-Talyvor-Batch header.
func ensureBatchFlag(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["batch_eligible"] = true
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

func initTracing(ctx context.Context) (*sdktrace.TracerProvider, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("talyvor-lens"),
		),
	)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	// Composite propagator extracts/injects both W3C traceparent (the
	// trace ID we care about) and W3C baggage (key-value context that
	// upstream apps may attach to requests).
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp, nil
}
