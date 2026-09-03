package cartridge

import (
	"context"
	"errors"
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
	hook    func(context.Context) (Status, error)
	detects atomic.Int32
}

var _ Cartridge = (*selectFake)(nil)

func (f *selectFake) Name() string { return f.name }

// Detect counts the probe and returns the scripted verdict, or defers to
// the hook when one is set (for probes that must block or watch ctx).
func (f *selectFake) Detect(ctx context.Context) (Status, error) {
	f.detects.Add(1)
	f.mu.Lock()
	status, err, hook := f.status, f.err, f.hook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return status, err
}

func (f *selectFake) Run(context.Context, RunSpec) (Session, error) {
	p := NewPipe(f.name, nil)
	p.Finish(Result{Output: f.name}, nil)
	return p, nil
}

// set replaces the Detect verdict and clears any hook.
func (f *selectFake) set(st Status, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.err, f.hook = st, err, nil
}

// setHook makes Detect call fn instead of returning the scripted verdict.
func (f *selectFake) setHook(fn func(context.Context) (Status, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = fn
}

// selectCancelAware is a Detect hook that behaves like the real adapters:
// it reports the healthy status unless ctx is already done, in which case
// it maps the context error to KindTimeout the way cartridge/mock does.
func selectCancelAware(name string) func(context.Context) (Status, error) {
	return func(ctx context.Context) (Status, error) {
		if err := ctx.Err(); err != nil {
			return Status{}, NewError(KindTimeout, name, "detect cancelled", err)
		}
		return selectHealthy(name), nil
	}
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
		// cancelled runs the selection with an already-cancelled context.
		cancelled bool

		want       string
		fellBack   bool
		wantErr    error
		reasonKind Kind
		warnings   int
		warnHas    []string
		// after runs extra assertions once the selection has been checked.
		after func(t *testing.T, s *Selector, fakes *selectFakes, rec *selectRecorder)
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
		{
			// Both CLIs are healthy; only the caller's context is dead. The
			// probe error is returned as-is with no fallback, nothing is
			// cached and nothing is warned about, so the next Select with a
			// live context gets the override.
			name:      "cancelled context is neither cached nor a fallback",
			role:      domain.RoleTester,
			alpha:     func(f *selectFake) { f.setHook(selectCancelAware("alpha")) },
			beta:      func(f *selectFake) { f.setHook(selectCancelAware("beta")) },
			cancelled: true,
			wantErr:   context.Canceled,
			after: func(t *testing.T, s *Selector, fakes *selectFakes, rec *selectRecorder) {
				assert.EqualValues(t, 2, fakes.beta.detects.Load(), "Choose and Select each probed beta")
				assert.EqualValues(t, 0, fakes.alpha.detects.Load(), "no fallback probe for a dead caller")
				c, err := s.Select(context.Background(), domain.RoleTester)
				require.NoError(t, err)
				assert.Equal(t, "beta", c.Name(), "the cancelled verdict was not cached")
				c, err = s.Select(context.Background(), domain.RolePO)
				require.NoError(t, err)
				assert.Equal(t, "alpha", c.Name())
				assert.Empty(t, rec.warnings())
			},
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
			ctx := ctx
			if tc.cancelled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
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
				assert.Empty(t, sel.Name)
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
			if tc.after != nil {
				tc.after(t, s, fakes, rec)
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

// TestSelectorConcurrentSelects is the scheduler's burst at run start: many
// Selects hit a cold cache at once and must share one probe per cartridge.
// Each probe blocks until every goroutine has called Select, so without
// coalescing every one of them would have probed.
func TestSelectorConcurrentSelects(t *testing.T) {
	const n = 32
	s, fakes, rec := selectNew(t, selectConfig())
	var arrived sync.WaitGroup
	arrived.Add(n)
	release := make(chan struct{})
	go func() {
		arrived.Wait()
		close(release)
	}()
	fakes.beta.setHook(func(context.Context) (Status, error) {
		<-release
		return Status{}, NewError(KindCrash, "beta", "boom", nil)
	})
	fakes.alpha.setHook(func(context.Context) (Status, error) {
		<-release
		return selectHealthy("alpha"), nil
	})

	var wg sync.WaitGroup
	roles := domain.Roles()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(role domain.Role) {
			defer wg.Done()
			arrived.Done()
			c, err := s.Select(context.Background(), role)
			assert.NoError(t, err)
			if assert.NotNil(t, c) {
				assert.Equal(t, "alpha", c.Name())
			}
		}(roles[i%len(roles)])
	}
	wg.Wait()
	assert.Len(t, rec.warnings(), 1, "one warning for the one failed probe")
	assert.EqualValues(t, 1, fakes.beta.detects.Load(), "cold-cache Selects shared one beta probe")
	assert.EqualValues(t, 1, fakes.alpha.detects.Load(), "cold-cache Selects shared one alpha probe")
}

// TestSelectorWaiterRetriesAfterLeaderCancelled: the goroutine that started
// a probe has its context cancelled mid-probe; a waiter with a live
// context must not inherit that failure but probe again itself.
func TestSelectorWaiterRetriesAfterLeaderCancelled(t *testing.T) {
	s, fakes, rec := selectNew(t, selectConfig())
	probing := make(chan struct{}, 2)
	fakes.beta.setHook(func(ctx context.Context) (Status, error) {
		probing <- struct{}{}
		select {
		case <-ctx.Done():
			return Status{}, NewError(KindTimeout, "beta", "detect cancelled", ctx.Err())
		case <-time.After(2 * time.Second):
			return selectHealthy("beta"), nil
		}
	})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	leaderErr := make(chan error, 1)
	go func() {
		_, err := s.Select(leaderCtx, domain.RoleTester)
		leaderErr <- err
	}()
	<-probing // the leader is inside Detect

	waiterDone := make(chan struct{})
	var waiterC Cartridge
	var waiterErr error
	go func() {
		defer close(waiterDone)
		waiterC, waiterErr = s.Select(context.Background(), domain.RoleTester)
	}()
	// Give the waiter time to join the in-flight probe rather than start
	// one; it cannot have probed yet because the leader still holds it.
	time.Sleep(50 * time.Millisecond)
	assert.EqualValues(t, 1, fakes.beta.detects.Load(), "the waiter joined the leader's probe")

	cancelLeader()
	require.ErrorIs(t, <-leaderErr, context.Canceled)
	fakes.beta.set(selectHealthy("beta"), nil)
	// Do not wait on `probing` here: set() clears the hook, so whether the
	// waiter's own probe reaches the hook (and sends) or the plain verdict
	// is a race. Wait for the waiter itself and count the probes instead.
	<-waiterDone
	require.NoError(t, waiterErr)
	assert.Equal(t, "beta", waiterC.Name())
	assert.EqualValues(t, 2, fakes.beta.detects.Load(), "the waiter probed once with its own context")
	assert.EqualValues(t, 0, fakes.alpha.detects.Load(), "no fallback for a cancelled caller")
	assert.Empty(t, rec.warnings())
}

// TestSelectorWaiterOwnContextCancelled: a waiter whose own context ends
// while it is queued behind another caller's probe gets a killed error
// wrapping its context's cause, and the probe's eventual verdict is still
// cached for everyone else.
func TestSelectorWaiterOwnContextCancelled(t *testing.T) {
	s, fakes, rec := selectNew(t, selectConfig())
	probing := make(chan struct{}, 1)
	release := make(chan struct{})
	fakes.alpha.setHook(func(context.Context) (Status, error) {
		probing <- struct{}{}
		<-release
		return selectHealthy("alpha"), nil
	})

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, err := s.Select(context.Background(), domain.RolePO)
		assert.NoError(t, err)
	}()
	<-probing

	cause := errors.New("agent slot withdrawn")
	waiterCtx, cancelWaiter := context.WithCancelCause(context.Background())
	cancelWaiter(cause)
	_, err := s.Select(waiterCtx, domain.RoleEngineer)
	require.ErrorIs(t, err, ErrKilled)
	require.ErrorIs(t, err, cause)
	assert.Equal(t, KindKilled, KindOf(err))

	close(release)
	<-leaderDone
	c, err := s.Select(context.Background(), domain.RoleEngineer)
	require.NoError(t, err)
	assert.Equal(t, "alpha", c.Name())
	assert.EqualValues(t, 1, fakes.alpha.detects.Load(), "the leader's verdict was cached")
	assert.Empty(t, rec.warnings())
}

func TestSelectorDeadlineExceededNotCached(t *testing.T) {
	s, fakes, rec := selectNew(t, selectConfig())
	fakes.alpha.setHook(func(ctx context.Context) (Status, error) {
		<-ctx.Done()
		// The adapter reports the deadline without consulting ctx.Err on
		// the Selector side: errors.Is on the cause must be enough.
		return Status{}, NewError(KindTimeout, "alpha", "probe timed out", context.DeadlineExceeded)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := s.Select(ctx, domain.RolePO)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, rec.warnings(), "a caller's deadline is not a cartridge failure")

	fakes.alpha.set(selectHealthy("alpha"), nil)
	c, err := s.Select(context.Background(), domain.RolePO)
	require.NoError(t, err)
	assert.Equal(t, "alpha", c.Name())
	assert.EqualValues(t, 2, fakes.alpha.detects.Load())
}

// TestSelectorDetectOnUnusedCartridge: a direct Detect on a built cartridge
// that no role uses still publishes a warning, but one that does not claim
// any role falls back.
func TestSelectorDetectOnUnusedCartridge(t *testing.T) {
	cfg := selectConfig()
	cfg.Roles = nil
	s, fakes, rec := selectNew(t, cfg)
	fakes.beta.set(Status{Installed: true, Version: "1.0.0", Auth: AuthMissing, Detail: "run beta login"}, nil)

	_, err := s.Detect(context.Background(), "beta")
	require.ErrorIs(t, err, ErrAuth)
	warns := rec.warnings()
	require.Len(t, warns, 1)
	assert.Equal(t, `cartridge "beta" failed detection (version 1.0.0); no role is configured to use it`, warns[0].Message)
	assert.Contains(t, warns[0].Err, "run beta login")
	assert.NotContains(t, warns[0].Message, "fall back")

	// A single override reads "role X falls back".
	cfg = selectConfig()
	s, fakes, rec = selectNew(t, cfg)
	fakes.beta.set(Status{Installed: false, Detail: "beta not on PATH"}, nil)
	_, err = s.Detect(context.Background(), "beta")
	require.ErrorIs(t, err, ErrNotInstalled)
	require.Len(t, rec.warnings(), 1)
	assert.Equal(t, `cartridge "beta" failed detection; role tester falls back to the active cartridge "alpha"`, rec.warnings()[0].Message)
}

func TestPublisherFunc(t *testing.T) {
	var got events.Event
	p := PublisherFunc(func(ev events.Event) { got = ev })
	p.Publish(events.Warning{Message: "hi"})
	w, ok := got.(events.Warning)
	require.True(t, ok)
	assert.Equal(t, "hi", w.Message)
}
