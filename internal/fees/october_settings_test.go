package fees

import (
	"reflect"
	"testing"
)

// The by-plan fees of a lens.env written before the two ladders name the 5 Oct plans: Lens runs on the defaults
// (B61.57).
func TestLoad_TheOctoberByPlanFeesRunOnTheDefaults(t *testing.T) {
	old := map[string]string{
		"LENS_PLATFORM_FEE_BPS": `{"free":550,"team":300,"business":100,"enterprise":100}`,
		"LENS_FX_MARGIN_BPS":    `{"free":0,"team":50,"business":25,"enterprise":15}`,
	}
	got, err := Load(func(k string) string { return old[k] })
	if err != nil || !reflect.DeepEqual(got, Defaults()) {
		t.Fatalf("Load(the 5 Oct by-plan fees) = %+v, %v; want the defaults", got, err)
	}
	if _, err := Load(func(k string) string {
		if k == "LENS_FX_MARGIN_BPS" {
			return `{"free":0,"team":50,"business":25,"enterprise":15,"personal_solo":100}`
		}
		return ""
	}); err == nil {
		t.Fatal("a by-plan fee naming the 5 Oct plans and one more loads; want it to stop Lens")
	}
}
