package catalog

import (
	"strings"
	"testing"
	"time"
)

// B18.12 — every catalog model has a release date and a tier, so the chat picker orders models by when
// they were released and defaults to the newest frontier model instead of parsing either from the name.
func TestEveryCatalogModelHasAReleaseDateAndATier(t *testing.T) {
	tiers := map[string]bool{TierFrontier: true, TierBalanced: true, TierFast: true, TierEmbedding: true, TierSpeech: true}
	models := All()
	if len(models) < 50 {
		t.Fatalf("the catalog holds %d models — this test would pass on an empty seed", len(models))
	}
	for _, m := range models {
		released, err := time.Parse(time.DateOnly, m.ReleaseDate)
		if err != nil || released.After(time.Now()) {
			t.Errorf("%s: release date %q is not a past YYYY-MM-DD date", m.ID, m.ReleaseDate)
		}
		if !tiers[m.Tier] {
			t.Errorf("%s: tier %q is not frontier, balanced, fast, embedding or speech", m.ID, m.Tier)
		}
		if (m.Tier == TierEmbedding) != strings.Contains(m.ID, "embedding") {
			t.Errorf("%s: tier %q — embedding models, and only they, are in the embedding tier", m.ID, m.Tier)
		}
		if (m.Tier == TierSpeech) != (m.PerMinute > 0 || m.Per1MChars > 0) {
			t.Errorf("%s: tier %q — speech models, and only they, are priced per minute or per character", m.ID, m.Tier)
		}
	}
}
