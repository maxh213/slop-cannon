package cartridge

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// selectFake is a scriptable cartridge for the selection tests. Its Detect
// verdict can be swapped at any time and every probe is counted, which is
// what the fallback and caching tests need. Two of them are registered
// under different names so no test depends on a single adapter (D-19).
type selectFake struct {
	name string
	cfg  config.Cartridge

	mu      sync.Mutex
	status  Status
	err     error
	detects atomic.Int32
}

var _ Cartridge = (*selectFake)(nil)

func (f *selectFake) Name() string { return f.name }

func (f *selectFake) Detect(context.Context) (Status, error) {
	f.detects.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.err
}

func (f *selectFake) Run(context.Context, RunSpec) (Session, error) {
	p := NewPipe(f.name, nil)
	p.Finish(Result{Output: f.name}, nil)
	return p, nil
}

// set replaces the Detect verdict.
func (f *selectFake) set(st Status, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.err = st, err
}

func selectHealthy(name string) Status {
	return Status{Installed: true, Path: "/usr/bin/" + name, Version: "2.0.0", Auth: AuthOK}
}

// selectFakes holds the two cartridges a test built through the registry.
type selectFakes struct {
	alpha, beta *selectFake
}

// selectRegistry returns a fresh registry with "alpha" and "beta" factories
// whose products are captured in the returned selectFakes.
func selectRegistry(t *testing.T) (*Registry, *selectFakes) {
	t.Helper()
	fakes := &selectFakes{}
	r := NewRegistry()
	factory := func(slot **selectFake) Factory {
		return func(name string, cfg config.Cartridge) (Cartridge, error) {
			f := &selectFake{name: name, cfg: cfg, status: selectHealthy(name)}
			*slot = f
			return f, nil
		}
	}
	r.MustRegister("alpha", factory(&fakes.alpha))
	r.MustRegister("beta", factory(&fakes.beta))
	return r, fakes
}

// selectConfig is a config with both cartridges, alpha active and the
// tester role overridden to beta.
func selectConfig() config.Config {
	cfg := config.Defaults()
	cfg.ActiveCartridge = "alpha"
	cfg.Cartridges = map[string]config.Cartridge{
		"alpha": {Model: "a-model"},
		"beta":  {Model: "b-model"},
	}
	cfg.Roles = map[domain.Role]string{domain.RoleTester: "beta"}
	return cfg
}

// selectRecorder collects published events.
type selectRecorder struct {
	mu  sync.Mutex
	evs []events.Event
}

func (r *selectRecorder) Publish(ev events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, ev)
}

func (r *selectRecorder) warnings() []events.Warning {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []events.Warning
	for _, ev := range r.evs {
		if w, ok := ev.(events.Warning); ok {
			out = append(out, w)
		}
	}
	return out
}

// selectClock is a manual clock.
type selectClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *selectClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *selectClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func selectNew(t *testing.T, cfg config.Config, opts ...SelectorOption) (*Selector, *selectFakes, *selectRecorder) {
	t.Helper()
	r, fakes := selectRegistry(t)
	rec := &selectRecorder{}
	opts = append([]SelectorOption{WithPublisher(rec), WithRun("run-1")}, opts...)
	s, err := BuildSelector(r, cfg, opts...)
	require.NoError(t, err)
	return s, fakes, rec
}

func TestNewSelectorRejectsBadConfig(t *testing.T) {
	r, _ := selectRegistry(t)

	t.Run("no active cartridge", func(t *testing.T) {
		cfg := selectConfig()
		cfg.ActiveCartridge = ""
		_, err := BuildSelector(r, cfg)
		require.ErrorIs(t, err, ErrNoActiveCartridge)
	})

	t.Run("active cartridge not built", func(t *testing.T) {
		cfg := selectConfig()
		built, err := r.Build(cfg)
		require.NoError(t, err)
		delete(built, "alpha")
		_, err = NewSelector(cfg, built)
		require.ErrorIs(t, err, ErrCartridgeNotBuilt)
		assert.Contains(t, err.Error(), `active cartridge "alpha"`)
		assert.Contains(t, err.Error(), "built: beta")
	})

	t.Run("role override not built", func(t *testing.T) {
		cfg := selectConfig()
		cfg.Roles[domain.RoleEngineer] = "gamma"
		_, err := BuildSelector(r, cfg)
		require.ErrorIs(t, err, ErrCartridgeNotBuilt)
		assert.Contains(t, err.Error(), `role engineer override "gamma"`)
	})

	t.Run("unknown configured cartridge fails in Build", func(t *testing.T) {
		cfg := selectConfig()
		cfg.Cartridges["gamma"] = config.Cartridge{}
		_, err := BuildSelector(r, cfg)
		require.ErrorIs(t, err, ErrUnknownCartridge)
	})

	t.Run("empty override is ignored", func(t *testing.T) {
		cfg := selectConfig()
		cfg.Roles[domain.RoleEngineer] = ""
		s, err := BuildSelector(r, cfg)
		require.NoError(t, err)
		assert.Equal(t, "alpha", s.Requested(domain.RoleEngineer))
	})
}

func TestSelectorAccessors(t *testing.T) {
	s, fakes, _ := selectNew(t, selectConfig())
	assert.Equal(t, "alpha", s.Active())
	assert.Equal(t, []string{"alpha", "beta"}, s.Names())
	assert.Equal(t, "alpha", s.Requested(domain.RolePO))
	assert.Equal(t, "alpha", s.Requested(domain.RoleEngineer))
	assert.Equal(t, "beta", s.Requested(domain.RoleTester))

	c, ok := s.Cartridge("beta")
	require.True(t, ok)
	assert.Same(t, fakes.beta, c)
	_, ok = s.Cartridge("gamma")
	assert.False(t, ok)

	assert.Equal(t, "a-model", fakes.alpha.cfg.Model, "factory received the configured settings")
	assert.Equal(t, "b-model", fakes.beta.cfg.Model)

	_, err := s.Detect(context.Background(), "gamma")
	require.ErrorIs(t, err, ErrCartridgeNotBuilt)
	st, err := s.Detect(context.Background(), "beta")
	require.NoError(t, err)
	assert.Equal(t, "2.0.0", st.Version)
}

func TestSelectPolicyTable(t *testing.T) {
	ctx := context.Background()
	authErr := NewError(KindAuth, "beta", "not logged in", nil)
	crashErr := NewError(KindCrash, "alpha", "exit 1", nil)

	tests := []struct {
		name  string
		role  domain.Role
		alpha func(*selectFake)
		beta  func(*selectFake)

		want       string
		fellBack   bool
		wantErr    error
		reasonKind Kind
		warnings   int
		warnHas    []string
	}{
		{
			name: "no override uses active",
			role: domain.RolePO,
			want: "alpha",
		},
		{
			name: "engineer without override uses active",
			role: domain.RoleEngineer,
			want: "alpha",
		},
		{
			name: "override used when healthy",
			role: domain.RoleTester,
			want: "beta",
		},
		{
			name:       "override Detect error falls back to active with a warning",
			role:       domain.RoleTester,
			beta:       func(f *selectFake) { f.set(Status{Installed: true, Auth: AuthMissing}, authErr) },
			want:       "alpha",
			fellBack:   true,
			reasonKind: KindAuth,
			warnings:   1,
			warnHas:    []string{`cartridge "beta" failed detection`, "role tester", `active cartridge "alpha"`},
		},
		{
			name:       "override not installed (nil error) falls back",
			role:       domain.RoleTester,
			beta:       func(f *selectFake) { f.set(Status{Installed: false, Detail: "beta not on PATH"}, nil) },
			want:       "alpha",
			fellBack:   true,
			reasonKind: KindNotInstalled,
			warnings:   1,
			warnHas:    []string{"beta not on PATH"},
		},
		{
			name:       "override auth missing (nil error) falls back",
			role:       domain.RoleTester,
			beta:       func(f *selectFake) { f.set(Status{Installed: true, Auth: AuthMissing}, nil) },
			want:       "alpha",
			fellBack:   true,
			reasonKind: KindAuth,
			warnings:   1,
		},
		{
			name:     "active failing with no override is an error",
			role:     domain.RoleEngineer,
			alpha:    func(f *selectFake) { f.set(Status{Installed: true, Version: "2.0.0"}, crashErr) },
			wantErr:  ErrCrash,
			warnings: 1,
			warnHas:  []string{`cartridge "alpha" failed detection (version 2.0.0)`, "no fallback"},
		},
		{
			name:       "override and active both failing reports the active error",
			role:       domain.RoleTester,
			alpha:      func(f *selectFake) { f.set(Status{}, crashErr) },
			beta:       func(f *selectFake) { f.set(Status{}, authErr) },
			fellBack:   true,
			reasonKind: KindAuth,
			wantErr:    ErrCrash,
			warnings:   2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, fakes, rec := selectNew(t, selectConfig())
			if tc.alpha != nil {
				tc.alpha(fakes.alpha)
			}
			if tc.beta != nil {
				tc.beta(fakes.beta)
			}

			sel, err := s.Choose(ctx, tc.role)
			assert.Equal(t, tc.role, sel.Role)
			assert.Equal(t, s.Requested(tc.role), sel.Requested)
			assert.Equal(t, tc.fellBack, sel.FellBack)
			if tc.reasonKind != "" {
				assert.Equal(t, tc.reasonKind, KindOf(sel.Reason))
			} else {
				assert.NoError(t, sel.Reason)
			}
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				var ce *Error
				require.ErrorAs(t, err, &ce, "selection errors are *Error")
				assert.Nil(t, sel.Cartridge)
				c, err := s.Select(ctx, tc.role)
				require.ErrorIs(t, err, tc.wantErr)
				assert.Nil(t, c)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, sel.Name)
				assert.Equal(t, tc.want, sel.Cartridge.Name())
				assert.True(t, sel.Status.Ready())
				c, err := s.Select(ctx, tc.role)
				require.NoError(t, err)
				assert.Equal(t, tc.want, c.Name())
			}

			warns := rec.warnings()
			require.Len(t, warns, tc.warnings, "warnings: %+v", warns)
			for _, w := range warns {
				assert.Equal(t, "cartridge", w.Source)
				assert.Equal(t, domain.RunID("run-1"), w.RunID())
				assert.NotEmpty(t, w.Err)
				assert.False(t, w.At().IsZero())
			}
			if len(warns) > 0 {
				for _, want := range tc.warnHas {
					assert.Contains(t, warns[0].Message+" | "+warns[0].Err, want)
				}
			}
		})
	}
}

func TestSelectorCachesDetectAndWarnsOncePerProbe(t *testing.T) {
	ctx := context.Background()
	clock := &selectClock{t: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	s, fakes, rec := selectNew(t, selectConfig(), WithClock(clock.now), WithDetectTTL(time.Minute))
	fakes.beta.set(Status{}, NewError(KindCrash, "beta", "boom", nil))

	for i := 0; i < 5; i++ {
		c, err := s.Select(ctx, domain.RoleTester)
		require.NoError(t, err)
		assert.Equal(t, "alpha", c.Name())
	}
	assert.EqualValues(t, 1, fakes.beta.detects.Load(), "override probed once within the TTL")
	assert.EqualValues(t, 1, fakes.alpha.detects.Load(), "active probed once within the TTL")
	require.Len(t, rec.warnings(), 1, "one warning per fresh failed probe")
	assert.Equal(t, clock.now(), rec.warnings()[0].At())

	// Past the TTL both cartridges are probed again; beta is healthy now.
	clock.advance(time.Minute)
	fakes.beta.set(selectHealthy("beta"), nil)
	c, err := s.Select(ctx, domain.RoleTester)
	require.NoError(t, err)
	assert.Equal(t, "beta", c.Name())
	assert.EqualValues(t, 2, fakes.beta.detects.Load())
	assert.Len(t, rec.warnings(), 1)

	// Invalidate forces a fresh probe before the TTL elapses.
	fakes.beta.set(Status{}, NewError(KindRateLimited, "beta", "slow down", nil))
	c, err = s.Select(ctx, domain.RoleTester)
	require.NoError(t, err)
	assert.Equal(t, "beta", c.Name(), "cached verdict still trusted")
	s.Invalidate("beta")
	c, err = s.Select(ctx, domain.RoleTester)
	require.NoError(t, err)
	assert.Equal(t, "alpha", c.Name())
	assert.EqualValues(t, 3, fakes.beta.detects.Load())
	assert.EqualValues(t, 2, fakes.alpha.detects.Load(), "alpha's verdict had expired with the TTL too")
	assert.Len(t, rec.warnings(), 2)

	// Invalidate("") clears everything.
	s.Invalidate("")
	_, err = s.Select(ctx, domain.RolePO)
	require.NoError(t, err)
	assert.EqualValues(t, 3, fakes.alpha.detects.Load())
}

func TestSelectorDetectTTLZeroAndNegative(t *testing.T) {
	ctx := context.Background()

	t.Run("zero probes every time", func(t *testing.T) {
		s, fakes, _ := selectNew(t, selectConfig(), WithDetectTTL(0))
		for i := 0; i < 3; i++ {
			_, err := s.Select(ctx, domain.RolePO)
			require.NoError(t, err)
		}
		assert.EqualValues(t, 3, fakes.alpha.detects.Load())
	})

	t.Run("negative caches forever", func(t *testing.T) {
		clock := &selectClock{t: time.Now()}
		s, fakes, _ := selectNew(t, selectConfig(), WithDetectTTL(-1), WithClock(clock.now))
		for i := 0; i < 3; i++ {
			_, err := s.Select(ctx, domain.RolePO)
			require.NoError(t, err)
			clock.advance(24 * time.Hour)
		}
		assert.EqualValues(t, 1, fakes.alpha.detects.Load())
	})
}

func TestSelectorWithoutPublisherStillFallsBack(t *testing.T) {
	r, fakes := selectRegistry(t)
	s, err := BuildSelector(r, selectConfig())
	require.NoError(t, err)
	fakes.beta.set(Status{}, NewError(KindCrash, "beta", "boom", nil))
	sel, err := s.Choose(context.Background(), domain.RoleTester)
	require.NoError(t, err)
	assert.True(t, sel.FellBack)
	assert.Equal(t, "alpha", sel.Name)
}

func TestSelectorPublishesToEventBus(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	sub := bus.Subscribe(context.Background(), 8)
	defer sub.Unsubscribe()

	r, fakes := selectRegistry(t)
	s, err := BuildSelector(r, selectConfig(), WithPublisher(bus), WithRun("run-bus"))
	require.NoError(t, err)
	fakes.beta.set(Status{Installed: false}, nil)

	c, err := s.Select(context.Background(), domain.RoleTester)
	require.NoError(t, err)
	assert.Equal(t, "alpha", c.Name())

	select {
	case ev := <-sub.Events():
		w, ok := ev.(events.Warning)
		require.True(t, ok, "got %T", ev)
		assert.Equal(t, domain.RunID("run-bus"), w.RunID())
		assert.Equal(t, events.KindWarning, w.Kind())
		assert.Contains(t, w.Err, "not installed")
	case <-time.After(2 * time.Second):
		t.Fatal("no warning published")
	}
}

func TestSelectorMultipleRolesShareAnOverride(t *testing.T) {
	cfg := selectConfig()
	cfg.Roles[domain.RoleEngineer] = "beta"
	s, fakes, rec := selectNew(t, cfg)
	fakes.beta.set(Status{}, NewError(KindCreditsExhausted, "beta", "out of credits", nil))

	for _, role := range []domain.Role{domain.RoleEngineer, domain.RoleTester} {
		sel, err := s.Choose(context.Background(), role)
		require.NoError(t, err)
		assert.True(t, sel.FellBack)
		assert.Equal(t, "alpha", sel.Name)
		assert.ErrorIs(t, sel.Reason, ErrCreditsExhausted)
	}
	warns := rec.warnings()
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0].Message, "roles engineer, tester fall back")
}

func TestSelectorConcurrentSelects(t *testing.T) {
	s, fakes, rec := selectNew(t, selectConfig())
	fakes.beta.set(Status{}, NewError(KindCrash, "beta", "boom", nil))

	var wg sync.WaitGroup
	roles := domain.Roles()
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(role domain.Role) {
			defer wg.Done()
			c, err := s.Select(context.Background(), role)
			assert.NoError(t, err)
			if assert.NotNil(t, c) {
				assert.Equal(t, "alpha", c.Name())
			}
		}(roles[i%len(roles)])
	}
	wg.Wait()
	assert.GreaterOrEqual(t, len(rec.warnings()), 1)
	assert.LessOrEqual(t, fakes.beta.detects.Load(), int32(32))
}

func TestPublisherFunc(t *testing.T) {
	var got events.Event
	p := PublisherFunc(func(ev events.Event) { got = ev })
	p.Publish(events.Warning{Message: "hi"})
	w, ok := got.(events.Warning)
	require.True(t, ok)
	assert.Equal(t, "hi", w.Message)
}
