package sandbox

import (
	"strings"
	"testing"
)

func TestParseServices(t *testing.T) {
	svs, err := ParseServices(" pg=10.0.0.5:5432 , redis=cache.internal:6379,,")
	if err != nil || len(svs) != 2 || svs[0] != (Service{"pg", "10.0.0.5:5432"}) || svs[1] != (Service{"redis", "cache.internal:6379"}) {
		t.Fatalf("%v %v", svs, err)
	}
	if svs, err := ParseServices(""); err != nil || svs != nil {
		t.Errorf("empty: %v %v", svs, err)
	}
	for _, bad := range []string{"pg", "pg=10.0.0.5", "pg=:5432", "pg=h:0", "pg=h:70000", "PG=h:1", "1pg=h:1", "p-g=h:1",
		"pg=h:1,pg=h:2", strings.Repeat("a", 33) + "=h:1"} {
		if _, err := ParseServices(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func svcSpec(egress EgressMode, svs []Service) SandboxSpec {
	return SandboxSpec{Version: SpecVersion, Network: NetworkPolicy{Egress: egress, Services: svs}}
}

// A layer may keep some of its parent's services, never add one or point
// one elsewhere; a run with no network has none.
func TestResolveServices(t *testing.T) {
	pg, cache := Service{"pg", "10.0.0.5:5432"}, Service{"cache", "10.0.0.6:6379"}
	sys := Layer{Name: LayerSystem, Spec: svcSpec(EgressRestricted, []Service{pg, cache})}
	run := func(svs []Service) Layer { return Layer{Name: LayerRun, Spec: svcSpec("", svs)} }

	got, err := Resolve(sys)
	if err != nil || len(got.Network.Services) != 2 {
		t.Fatalf("system: %v %v", got.Network.Services, err)
	}
	if got, err := Resolve(sys, run(nil)); err != nil || len(got.Network.Services) != 2 {
		t.Errorf("inherited: %v %v", got.Network.Services, err)
	}
	if got, err := Resolve(sys, run([]Service{pg})); err != nil || len(got.Network.Services) != 1 || got.Network.Services[0] != pg {
		t.Errorf("narrowed: %v %v", got.Network.Services, err)
	}
	if got, err := Resolve(sys, run([]Service{})); err != nil || len(got.Network.Services) != 0 {
		t.Errorf("dropped: %v %v", got.Network.Services, err)
	}
	if _, err := Resolve(sys, run([]Service{{"other", "10.0.0.7:1"}})); err == nil {
		t.Error("a layer added a service")
	}
	if _, err := Resolve(sys, run([]Service{{"pg", "10.0.0.99:5432"}})); err == nil {
		t.Error("a layer pointed a service elsewhere")
	}
	if got, err := Resolve(sys, Layer{Name: LayerRun, Spec: svcSpec(EgressNone, nil)}); err != nil || got.Network.Services != nil {
		t.Errorf("no network: %v %v", got.Network.Services, err)
	}
	if got, err := Resolve(Layer{Name: LayerSystem, Spec: svcSpec(EgressNone, []Service{pg})}); err != nil || got.Network.Services != nil {
		t.Errorf("system with no network: %v %v", got.Network.Services, err)
	}
	if _, err := Resolve(Layer{Name: LayerSystem, Spec: svcSpec(EgressRestricted, []Service{pg, pg})}); err == nil {
		t.Error("a service named twice")
	}
}
