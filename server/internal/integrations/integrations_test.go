package integrations_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	// The ping integration self-registers into the default registry on import
	// (the database/sql driver pattern); this blank import pins that wiring.
	"github.com/ydtoloza/homarr/server/internal/integrations"
	_ "github.com/ydtoloza/homarr/server/internal/integrations/ping"
)

// fake is a minimal Integration implementation for registry tests.
type fake struct{ called bool }

func (f *fake) Check(context.Context) (integrations.CheckResult, error) {
	f.called = true
	return integrations.CheckResult{StatusCode: 200}, nil
}

func newFakeFactory(called *bool) integrations.Factory {
	return func(integrations.Input) (integrations.Integration, error) {
		*called = true
		return &fake{}, nil
	}
}

// TestRegistryRegisterAndResolve pins the factory-per-kind contract: one
// definition per kind, resolved through integrations.New (the mirror of
// upstream createIntegrationAsync, packages/integrations/src/base/creator.ts).
func TestRegistryRegisterAndResolve(t *testing.T) {
	r := integrations.NewRegistry()
	var built bool
	r.Register(integrations.Definition{Kind: "plex", Name: "Plex"}, newFakeFactory(&built))

	def, err := r.DefinitionOf("plex")
	if err != nil {
		t.Fatalf("DefinitionOf: %v", err)
	}
	if def.Kind != "plex" || def.Name != "Plex" {
		t.Fatalf("definition = %+v, want {plex Plex}", def)
	}

	inst, err := r.New("plex", integrations.Input{ID: "i1", URL: "http://x"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !built {
		t.Fatal("factory was not invoked")
	}
	res, err := inst.Check(t.Context())
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("Check = %+v, %v", res, err)
	}

	// Kind is injected into the input when the caller omits it.
	_, err = r.New("plex", integrations.Input{ID: "i2"})
	if err != nil {
		t.Fatalf("New without explicit kind: %v", err)
	}
}

// TestRegistryUnknownKind pins the unknown-kind error surface.
func TestRegistryUnknownKind(t *testing.T) {
	r := integrations.NewRegistry()
	_, err := r.New("nope", integrations.Input{ID: "i1"})
	if !errors.Is(err, integrations.ErrUnknownKind) {
		t.Fatalf("err = %v, want ErrUnknownKind", err)
	}
	_, err = r.DefinitionOf("nope")
	if !errors.Is(err, integrations.ErrUnknownKind) {
		t.Fatalf("DefinitionOf err = %v, want ErrUnknownKind", err)
	}
}

// TestRegistryDuplicateKindPanics pins the collision behavior: upstream fails
// duplicates at compile time (integrationCreators is an object literal), the
// registry panics to keep the fail-fast property.
func TestRegistryDuplicateKindPanics(t *testing.T) {
	r := integrations.NewRegistry()
	var built bool
	r.Register(integrations.Definition{Kind: "dup", Name: "First"}, newFakeFactory(&built))
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("expected panic on duplicate kind")
		}
		if msg, ok := rec.(string); !ok || !strings.Contains(msg, `kind "dup" registered twice`) {
			t.Fatalf("panic = %v, want duplicate kind message", rec)
		}
	}()
	r.Register(integrations.Definition{Kind: "dup", Name: "Second"}, newFakeFactory(&built))
}

// TestRegistryInvalidRegistrationPanics pins the input validation of Register.
func TestRegistryInvalidRegistrationPanics(t *testing.T) {
	cases := []struct {
		name string
		def  integrations.Definition
	}{
		{"empty kind", integrations.Definition{Kind: "", Name: "X"}},
		{"kind with dot", integrations.Definition{Kind: "a.b", Name: "X"}},
		{"kind with space", integrations.Definition{Kind: "a b", Name: "X"}},
		{"empty name", integrations.Definition{Kind: "ok", Name: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := integrations.NewRegistry()
			defer func() {
				if rec := recover(); rec == nil {
					t.Fatal("expected panic on invalid registration")
				}
			}()
			r.Register(tc.def, func(integrations.Input) (integrations.Integration, error) {
				return &fake{}, nil
			})
		})
	}

	// Nil factory panics too.
	r := integrations.NewRegistry()
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected panic on nil factory")
		}
	}()
	r.Register(integrations.Definition{Kind: "ok", Name: "X"}, nil)
}

// TestDefaultRegistryPingKind pins that the ping integration is registered in
// the process-wide registry by importing its package (the blank-import wiring
// M4 will extend per integration).
func TestDefaultRegistryPingKind(t *testing.T) {
	var found bool
	for _, k := range integrations.Kinds() {
		if k == integrations.KindPing {
			found = true
		}
	}
	if !found {
		t.Fatalf("Kinds() = %v, want %q registered", integrations.Kinds(), integrations.KindPing)
	}
	def, err := integrations.DefinitionOf(integrations.KindPing)
	if err != nil {
		t.Fatalf("DefinitionOf(ping): %v", err)
	}
	if def.Name != "Ping" {
		t.Fatalf("ping definition = %+v", def)
	}
}
