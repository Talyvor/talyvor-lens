package plans

import (
	"reflect"
	"testing"
)

// A lens.env written before the two ladders names the 5 Oct plans. Migration 0398 has moved every account by the
// time Lens reads it, so Lens runs on the defaults and says so, rather than refusing to start (B61.57).
func TestLoad_TheOctoberGatesRunOnTheDefaults(t *testing.T) {
	const october = `{"free":{"agents":3,"seats":1,"own_provider_keys":"none","live_money":false,"slack_teams_approvals":false,"sso":false,"audit_export":false,"edge":false},"team":{"agents":25,"seats":5,"own_provider_keys":"add_on","live_money":true,"slack_teams_approvals":true,"sso":false,"audit_export":false,"edge":false},"business":{"agents":-1,"seats":25,"own_provider_keys":"included","live_money":true,"slack_teams_approvals":true,"sso":true,"audit_export":true,"edge":false},"enterprise":{"agents":-1,"seats":-1,"own_provider_keys":"included","live_money":true,"slack_teams_approvals":true,"sso":true,"audit_export":true,"edge":true}}`
	got, err := Load(func(string) string { return october })
	if err != nil || !reflect.DeepEqual(got, Defaults()) {
		t.Fatalf("Load(the 5 Oct gates) = %v, %v; want the defaults", got, err)
	}
	// Anything else that names a plan Lens does not have still stops it.
	if _, err := Load(func(string) string { return `{"free":{"agents":3,"seats":1,"own_provider_keys":"none"}}` }); err == nil {
		t.Fatal("a setting naming one 5 Oct plan and no other loads; want it to stop Lens")
	}
}
