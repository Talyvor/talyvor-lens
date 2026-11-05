package scan

import "testing"

// B52.27 — a flat series has no spread to judge by, so the z rule never calls a point on it unusual.
func TestZRuleNeverFiresOnAFlatSeries(t *testing.T) {
	f, _ := FamilyOf("eia.stocks.cushing-crude")
	pts := weekly(8, 0.9, 1)
	for i := range pts {
		pts[i].value = 400
	}
	z := 3.0
	pts[len(pts)-1].value = 401
	if unusual(f, pts, len(pts)-1, RuleZ, &z) {
		t.Fatal("a point on a flat series was called unusual under the z rule")
	}
}
