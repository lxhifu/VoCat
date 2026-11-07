package remoteproc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WatcherConfig controls the opt-in modem crash recovery watcher. The zero
// value is completed with the defaults documented per field.
type WatcherConfig struct {
	// PollInterval is how often remoteproc states are polled (default 10s).
	PollInterval time.Duration
	// ConfirmReads is the number of consecutive "crashed" reads required
	// before a crash is acted on (default 2), so a single torn read of the
	// sysfs state file cannot trigger a recovery.
	ConfirmReads int
	// Settle is how long the monitor keeps device clients gated after the
	// processor reports "running" again (default 90s). Field experience
	// shows the modem firmware needs substantially longer than the passive
	// monitor's 5s debounce before VoWiFi should reconnect.
	Settle time.Duration
	// MaxAttempts caps recovery triggers inside AttemptWindow (default 3).
	MaxAttempts int
	// AttemptWindow is the rolling window MaxAttempts applies to
	// (default 1h).
	AttemptWindow time.Duration
	// Cooldown is the minimum spacing between two recovery triggers
	// (default 5m).
	Cooldown time.Duration
	// RunningTimeout bounds the wait for state=running after a trigger
	// (default 120s).
	RunningTimeout time.Duration
	// RunningPollInterval is the poll cadence while waiting for a triggered
	// recovery to reach state=running (default 2s).
	RunningPollInterval time.Duration
}

func (c WatcherConfig) withDefaults() WatcherConfig {
	if c.PollInterval <= 0 {
		c.PollInterval = 10 * time.Second
	}
	if c.ConfirmReads <= 0 {
		c.ConfirmReads = 2
	}
	if c.Settle <= 0 {
		c.Settle = 90 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.AttemptWindow <= 0 {
		c.AttemptWindow = time.Hour
	}
	if c.Cooldown < 0 {
		c.Cooldown = 5 * time.Minute
	}
	if c.RunningTimeout <= 0 {
		c.RunningTimeout = 120 * time.Second
	}
	if c.RunningPollInterval <= 0 {
		c.RunningPollInterval = 2 * time.Second
	}
	return c
}

// RecoveryEvent describes one watcher observation or action. Attempt is the
// number of recovery triggers used inside the current rolling window,
// including the trigger the event is about when one was made.
type RecoveryEvent struct {
	Path    string
	Name    string
	At      time.Time
	Attempt int
	Err     error
	Reason  string
}

// RecoveryEvents are optional callbacks invoked by the watcher. Any of them
// may be nil. They run on the watcher's goroutine and must not block for
// long; hand work off if it can take time.
type RecoveryEvents struct {
	OnCrashDetected     func(context.Context, RecoveryEvent)
	OnRecoverySucceeded func(context.Context, RecoveryEvent)
	OnRecoveryFailed    func(context.Context, RecoveryEvent)
	OnRecoveryGaveUp    func(context.Context, RecoveryEvent)
}

type watchState struct {
	crashedStreak int
	inEpisode     bool
	awaiting      bool
	awaitDeadline time.Time
	attempt       int
	gaveUp        bool
}

// RecoveryWatcher polls the modem's remote processor and, after a confirmed
// firmware crash, triggers the kernel's one-shot recovery by writing
// "recover" to the remoteproc recovery file. It deliberately never writes
// the recovery policy values ("enabled"/"disabled"): the policy stays
// exactly as the operator configured it, the watcher only pulls the
// one-shot trigger. Only remote processors owning a WWAN port are watched,
// using the passive monitor's topology walk, so an unrelated processor
// (for example the Wi-Fi remoteproc) is never touched.
type RecoveryWatcher struct {
	monitor *monitor
	cfg     WatcherConfig
	events  RecoveryEvents
	logger  *slog.Logger
	now     func() time.Time
	// writeFile is a seam for tests; production writes go to sysfs.
	writeFile func(name string, data []byte) error

	mu          sync.Mutex
	owners      map[string]*watchState
	attempts    []time.Time
	lastAttempt time.Time
}

// NewRecoveryWatcher returns a watcher bound to the system sysfs monitor.
func NewRecoveryWatcher(cfg WatcherConfig, events RecoveryEvents, logger *slog.Logger) *RecoveryWatcher {
	return newRecoveryWatcher(system, cfg, events, logger)
}

func newRecoveryWatcher(m *monitor, cfg WatcherConfig, events RecoveryEvents, logger *slog.Logger) *RecoveryWatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &RecoveryWatcher{
		monitor: m,
		cfg:     cfg.withDefaults(),
		events:  events,
		logger:  logger,
		now:     m.now,
		writeFile: func(name string, data []byte) error {
			return os.WriteFile(name, data, 0o600)
		},
		owners: make(map[string]*watchState),
	}
}

// Run polls until ctx is cancelled. It is intended to be started once from
// the process assembly when the operator enabled the watcher.
func (w *RecoveryWatcher) Run(ctx context.Context) {
	w.logger.Info("modem crash recovery watcher started",
		"poll_interval", w.cfg.PollInterval, "settle", w.cfg.Settle,
		"max_attempts", w.cfg.MaxAttempts, "attempt_window", w.cfg.AttemptWindow,
		"cooldown", w.cfg.Cooldown)
	w.step(ctx)
	for {
		interval := w.cfg.PollInterval
		if w.anyAwaiting() {
			interval = w.cfg.RunningPollInterval
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			w.logger.Info("modem crash recovery watcher stopped")
			return
		case <-timer.C:
		}
		w.step(ctx)
	}
}

func (w *RecoveryWatcher) anyAwaiting() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, state := range w.owners {
		if state.awaiting {
			return true
		}
	}
	return false
}

// step performs one poll cycle over every known modem owner. Owners stay
// tracked after discovery because a crashed modem's WWAN class symlinks can
// disappear while it is down.
func (w *RecoveryWatcher) step(ctx context.Context) {
	paths := w.monitor.modemOwners()
	w.mu.Lock()
	seen := make(map[string]struct{}, len(paths)+len(w.owners))
	for _, path := range paths {
		seen[path] = struct{}{}
		if w.owners[path] == nil {
			w.owners[path] = &watchState{}
		}
	}
	for path := range w.owners {
		seen[path] = struct{}{}
	}
	w.mu.Unlock()
	for path := range seen {
		w.stepOwner(ctx, path)
	}
}

func (w *RecoveryWatcher) stepOwner(ctx context.Context, path string) {
	raw, err := os.ReadFile(filepath.Join(path, "state"))
	if err != nil {
		// The processor (or the whole class entry) is temporarily
		// unreadable, e.g. mid-recovery. Keep the episode bookkeeping and
		// try again next cycle instead of guessing a state.
		return
	}
	state := strings.TrimSpace(string(raw))
	now := w.now()

	w.mu.Lock()
	ws := w.owners[path]
	if ws == nil {
		ws = &watchState{}
		w.owners[path] = ws
	}

	if ws.awaiting {
		switch {
		case state == "running" || state == "attached":
			attempt := ws.attempt
			ws.awaiting = false
			ws.inEpisode = false
			ws.crashedStreak = 0
			ws.gaveUp = false
			w.mu.Unlock()
			w.monitor.setRecoveryHold(path, now.Add(w.cfg.Settle))
			w.logger.Info("modem remoteproc recovered; holding device access during settle",
				"remoteproc", path, "attempt", attempt, "settle", w.cfg.Settle)
			w.fire(ctx, w.events.OnRecoverySucceeded, RecoveryEvent{
				Path: path, Name: filepath.Base(path), At: now, Attempt: attempt,
			})
		case now.After(ws.awaitDeadline):
			attempt := ws.attempt
			ws.awaiting = false
			if state != "crashed" {
				ws.inEpisode = false
				ws.crashedStreak = 0
			}
			w.mu.Unlock()
			err := fmt.Errorf("remoteproc did not return to running within %s (last state %q)", w.cfg.RunningTimeout, state)
			w.logger.Error("modem recovery attempt failed", "remoteproc", path, "attempt", attempt, "error", err)
			w.fire(ctx, w.events.OnRecoveryFailed, RecoveryEvent{
				Path: path, Name: filepath.Base(path), At: now, Attempt: attempt, Err: err,
			})
		default:
			w.mu.Unlock()
		}
		return
	}

	if state != "crashed" {
		ws.crashedStreak = 0
		ws.inEpisode = false
		ws.gaveUp = false
		w.mu.Unlock()
		return
	}

	ws.crashedStreak++
	confirmed := ws.crashedStreak >= w.cfg.ConfirmReads
	firstConfirmation := confirmed && !ws.inEpisode
	if confirmed {
		ws.inEpisode = true
	}
	w.mu.Unlock()

	if firstConfirmation {
		w.logger.Warn("modem firmware crash confirmed", "remoteproc", path)
		w.fire(ctx, w.events.OnCrashDetected, RecoveryEvent{
			Path: path, Name: filepath.Base(path), At: now, Attempt: w.attemptCount(now),
		})
	}
	if confirmed {
		w.maybeAttempt(ctx, path, now)
	}
}

// maybeAttempt triggers one recovery when the anti-flap guards allow it:
// at most MaxAttempts triggers inside the rolling AttemptWindow and at
// least Cooldown between triggers. A trigger denied by the window cap ends
// automatic recovery for the current crash episode (reported once via
// OnRecoveryGaveUp); a trigger denied only by the cooldown is deferred to a
// later poll of the same episode.
func (w *RecoveryWatcher) maybeAttempt(ctx context.Context, path string, now time.Time) {
	w.mu.Lock()
	w.pruneAttemptsLocked(now)
	if len(w.attempts) >= w.cfg.MaxAttempts {
		ws := w.owners[path]
		notify := ws != nil && !ws.gaveUp
		if ws != nil {
			ws.gaveUp = true
		}
		attempts := len(w.attempts)
		w.mu.Unlock()
		if notify {
			reason := fmt.Sprintf("recovery attempt limit reached: %d attempts within %s", attempts, w.cfg.AttemptWindow)
			w.logger.Error("modem recovery gave up for this crash episode",
				"remoteproc", path, "attempts", attempts, "attempt_window", w.cfg.AttemptWindow)
			w.fire(ctx, w.events.OnRecoveryGaveUp, RecoveryEvent{
				Path: path, Name: filepath.Base(path), At: now, Attempt: attempts, Reason: reason,
			})
		}
		return
	}
	if !w.lastAttempt.IsZero() && now.Sub(w.lastAttempt) < w.cfg.Cooldown {
		w.mu.Unlock()
		return
	}
	w.attempts = append(w.attempts, now)
	w.lastAttempt = now
	attempt := len(w.attempts)
	if ws := w.owners[path]; ws != nil {
		ws.attempt = attempt
	}
	w.mu.Unlock()

	recoveryPath := filepath.Join(path, "recovery")
	// "recover" is the kernel's one-shot trigger value. The watcher never
	// writes the policy values "enabled" or "disabled" here.
	if err := w.writeFile(recoveryPath, []byte("recover")); err != nil {
		w.logger.Error("modem recovery trigger write failed",
			"remoteproc", path, "attempt", attempt, "error", err)
		w.fire(ctx, w.events.OnRecoveryFailed, RecoveryEvent{
			Path: path, Name: filepath.Base(path), At: now, Attempt: attempt, Err: err,
		})
		return
	}
	w.logger.Info("modem recovery triggered", "remoteproc", path, "attempt", attempt)

	w.mu.Lock()
	if ws := w.owners[path]; ws != nil {
		ws.awaiting = true
		ws.awaitDeadline = now.Add(w.cfg.RunningTimeout)
	}
	w.mu.Unlock()
}

func (w *RecoveryWatcher) pruneAttemptsLocked(now time.Time) {
	cutoff := now.Add(-w.cfg.AttemptWindow)
	kept := w.attempts[:0]
	for _, at := range w.attempts {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	w.attempts = kept
}

func (w *RecoveryWatcher) attemptCount(now time.Time) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneAttemptsLocked(now)
	return len(w.attempts)
}

func (w *RecoveryWatcher) fire(ctx context.Context, fn func(context.Context, RecoveryEvent), event RecoveryEvent) {
	if fn == nil {
		return
	}
	fn(ctx, event)
}
