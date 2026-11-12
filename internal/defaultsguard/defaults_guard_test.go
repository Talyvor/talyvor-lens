package defaultsguard_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/buildverify"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/distill"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/eval"
	"github.com/talyvor/lens/internal/keel"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/modelwatch"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/povi"
	"github.com/talyvor/lens/internal/ratelimit"
	"github.com/talyvor/lens/internal/tare/kompress"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/workspace"
)

// THE EXPORTED Default* SCALARS ARE NOW PINNED. Twelve of them were not.
//
// MEASURED 2026-08-28 (tab-k2w8, W4.33) by mutation, not by reading:
// ~/talyvor-queue/w433-lens-default-census-k2w8.py replaces one Default's value
// at a time with a clearly-different value of the same type, compiles it, and
// runs CI's exact command (go test -timeout 120s -count=1 -p 1 ./...) against a
// real Postgres. Population 26 of the 27 exported Default* scalar declarations
// in internal/** (DefaultVisionPrompt was not mutated; prose, not a scalar).
//
// RESULT: 12 UNPINNED · 11 CAUGHT · 3 INVALID.
//
// The 11 CAUGHT were already defended by their own packages — including both
// proof-of-improvement MINT rates, whose declaration claims to be "the SINGLE
// SOURCE OF TRUTH so money-path tests bind to the SAME value production runs
// at". That claim is TRUE, and this census is how it was checked rather than
// believed.
//
// The 12 UNPINNED could each be changed and the ENTIRE suite stayed green:
//
//	economy.DefaultAgentCeilingLXC      50_000_000 -> 50_000_000_000  (a spend ceiling, x1000)
//	mining.DefaultPatternEarnCapPerWorkspace  50k -> 50M              (a mint earn cap)
//	poolroyalty.DefaultMinConsensusAttesters    2 -> 1                (a SINGLE attester mints)
//	keel.DefaultMinWorkspaces                   3 -> 1                (k-anonymity floor -> 1)
//	auth.DefaultTokenTTL                      24h -> 1 year
//	tenant.DefaultRetentionDays                90 -> 36500
//	distill.DefaultIsolatorTimeout            30s -> 30h              (untrusted work)
//	distill.DefaultIsolatorMemoryBytes     512MiB -> 512GiB           (untrusted work)
//	povi.DefaultTraceTTL · localrouter.DefaultHealthCheckInterval ·
//	modelwatch.DefaultInterval · config.DefaultEmbeddingModel (a PRICED model swap)
//
// ⚠ THE SPLIT IS NOT RANDOM AND IT IS THE POINT: poolroyalty defends its own
// royalty share, both mint rates and its unlinked-grader floor — and does NOT
// defend the consensus-attester floor sitting in the same package. The repo
// defends the values it thought about, and the ones next to them went unwatched.
//
// ⚠ THIS FILE CHANGES NO VALUE. Every number is the number already shipping.
// Whether any is the RIGHT number is a product decision — several are money and
// anti-abuse thresholds — and is deliberately not taken here. What changes is
// that altering one becomes an edit to a named table instead of a silent
// one-token diff.
//
// ⚠ WHAT THIS DOES NOT PROVE: that each default is ENFORCED. Pinning
// DefaultIsolatorMemoryBytes does not prove the isolator applies it. Reach is a
// separate question this merge does not answer.

func sha12(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

type recorded struct {
	got  any
	want any
	note string
}

// shipped is keyed "pkgdir.ConstName" so the completeness floor below can
// compare it against what is actually declared in internal/**.
var shipped = map[string]recorded{
	"api.DefaultPage":                             {api.DefaultPage, 1, "first page"},
	"api.DefaultPageSize":                         {api.DefaultPageSize, 20, "page size"},
	"auth.DefaultTokenTTL":                        {auth.DefaultTokenTTL, 24 * time.Hour, "auth token lifetime"},
	"buildverify.DefaultImage":                    {buildverify.DefaultImage, "golang:1.25-alpine", "build-verify base image"},
	"cache.DefaultReplayStrategy":                 {cache.DefaultReplayStrategy, cache.ReplayFast, "stream replay strategy"},
	"config.DefaultEvalContributionRatePerPoint":  {config.DefaultEvalContributionRatePerPoint, 0.05, "MINT rate, LENS per point"},
	"config.DefaultRoutingPredictionRatePerPoint": {config.DefaultRoutingPredictionRatePerPoint, 0.02, "MINT rate, LENS per point"},
	"config.DefaultSessionKeyTTL":                 {config.DefaultSessionKeyTTL, time.Hour, "session key lifetime"},
	"config.DefaultEmbeddingModel":                {config.DefaultEmbeddingModel, "text-embedding-3-small", "PRICED embedding model"},
	"config.DefaultSemanticThreshold":             {config.DefaultSemanticThreshold, 0.98, "cache-hit threshold"},
	"distill.DefaultIsolatorMemoryBytes":          {distill.DefaultIsolatorMemoryBytes, 512 << 20, "sandbox memory bound"},
	"distill.DefaultIsolatorTimeout":              {distill.DefaultIsolatorTimeout, 30 * time.Second, "sandbox wall-clock bound"},
	"economy.DefaultAgentCeilingLXC":              {economy.DefaultAgentCeilingLXC, int64(50_000_000), "agent LXC spend ceiling"},
	"eval.DefaultEstOutputTokens":                 {eval.DefaultEstOutputTokens, 256, "pre-serve cost estimate"},
	"keel.DefaultMinWorkspaces":                   {keel.DefaultMinWorkspaces, 3, "k-anonymity floor"},
	"localrouter.DefaultHealthCheckInterval":      {localrouter.DefaultHealthCheckInterval, 30 * time.Second, "health check interval"},
	"market.DefaultAgeRating":                     {market.DefaultAgeRating, "18+", "rating of a listing published without one, so none reaches a family filter unrated (B41.185)"},
	"market.DefaultTrialMax":                      {market.DefaultTrialMax, 5, "most free trial uses an offer may give (B32.21)"},
	"market.DefaultLineageMaxShareBPS":            {market.DefaultLineageMaxShareBPS, 3000, "largest remix share a royalty listing may ask (B32.24)"},
	"market.DefaultLineageMaxDepth":               {market.DefaultLineageMaxDepth, 5, "generations the lineage read walks up (B32.24)"},
	"market.DefaultLineageTotalCapBPS":            {market.DefaultLineageTotalCapBPS, 5000, "most a listing's parents together receive of a sale (B32.26)"},
	"market.DefaultSimilarityHold":                {market.DefaultSimilarityHold, 0.92, "how similar an undeclared copy may be before it is held (B32.46)"},
	"market.DefaultIPCounterDays":                 {market.DefaultIPCounterDays, 10, "days a seller has to counter an IP claim (B32.47)"},
	"mining.DefaultPatternEarnCapPerWorkspace":    {mining.DefaultPatternEarnCapPerWorkspace, 50_000, "pattern earn cap"},
	"modelwatch.DefaultInterval":                  {modelwatch.DefaultInterval, time.Hour, "model-watch interval"},
	"modelwatch.DefaultApplyInterval":             {modelwatch.DefaultApplyInterval, 5 * time.Minute, "how soon every replica applies a confirmed price or a retirement"},
	"poolroyalty.DefaultMinConsensusAttesters":    {poolroyalty.DefaultMinConsensusAttesters, 2, "anti-collusion floor"},
	"poolroyalty.DefaultMinUnlinkedGraders":       {poolroyalty.DefaultMinUnlinkedGraders, 3, "sybil floor"},
	"poolroyalty.DefaultRoyaltyShare":             {poolroyalty.DefaultRoyaltyShare, 0.5, "revenue split"},
	"povi.DefaultTraceTTL":                        {povi.DefaultTraceTTL, 30 * time.Minute, "POVI trace TTL"},
	"ratelimit.DefaultBurstMultiplier":            {ratelimit.DefaultBurstMultiplier, 1.5, "burst multiplier"},
	"tenant.DefaultRetentionDays":                 {tenant.DefaultRetentionDays, 90, "data retention"},
	"workspace.DefaultCompressionPolicy":          {workspace.DefaultCompressionPolicy, workspace.CompressionDisabled, "compression default"},
	"workspace.DefaultDistillPolicy":              {workspace.DefaultDistillPolicy, workspace.DistillAlways, "distill default"},
	"workspace.DefaultTarePolicy":                 {workspace.DefaultTarePolicy, workspace.TareAlways, "tare default"},
	"kompress.DefaultDir":                         {kompress.DefaultDir, "/models/kompress-small", "where the image carries the Tare phase 2a weights"},
	// Prose, not a scalar — pinned by CONTENT HASH so a silent reword of a prompt
	// sent to a PAID vision model is loud, without inlining 191 characters here.
	"distill.DefaultVisionPrompt": {sha12(distill.DefaultVisionPrompt), "532be6cd7c36", "vision prompt, sha256[:12]"},
}

func TestShippedDefaults(t *testing.T) {
	for name, r := range shipped {
		if fmt.Sprint(r.got) != fmt.Sprint(r.want) {
			t.Errorf("%s (%s): production runs at %v, recorded default is %v.\n"+
				"If this change is deliberate, change the recorded value here in the SAME "+
				"commit and say why — that is the entire point of this file.", name, r.note, r.got, r.want)
		}
	}
}

// TestEveryExportedDefaultIsRecorded is the completeness floor, and it is why
// this guard cannot quietly narrow. The population is DERIVED by walking
// internal/** rather than restated, so a new exported Default* that nobody
// records reds here instead of escaping the census that produced this file.
func TestEveryExportedDefaultIsRecorded(t *testing.T) {
	// Matches a scalar declaration; struct/slice/map/func-valued Defaults are
	// out of population by construction and excluded by the value guard below.
	decl := regexp.MustCompile(`(?m)^\s*(?:const\s+)?(Default[A-Z][A-Za-z0-9]*)(?:\s+[\w.\[\]]+)?\s*=\s*(.*)$`)
	nonScalar := regexp.MustCompile(`^\s*(&|\[|map\[|func|struct)`)

	root := filepath.Join("..") // internal/
	found := map[string]bool{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		pkg := filepath.Base(filepath.Dir(path))
		for _, m := range decl.FindAllStringSubmatch(string(b), -1) {
			if nonScalar.MatchString(m[2]) {
				continue
			}
			found[pkg+"."+m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking internal/: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("parsed ZERO exported Default* declarations out of internal/ — the parser is " +
			"broken, and a guard that finds nothing to check is the defect it exists to catch")
	}

	var missing, stale []string
	for k := range found {
		if _, ok := shipped[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range shipped {
		if !found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("exported Default* declarations NOT recorded: %v\n"+
			"Add them — an unrecorded default is one nothing in this repository defends.", missing)
	}
	if len(stale) > 0 {
		t.Errorf("recorded names no longer declared in internal/: %v\n"+
			"Remove them — a guard pinning a value that no longer exists is inert.", stale)
	}
}
