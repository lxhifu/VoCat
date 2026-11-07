package remoteproc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type firedEvent struct {
	kind string
	ev   RecoveryEvent
}

type watcherFixture struct {
	*fixture
	w        *RecoveryWatcher
	wifi     string
	writes   []string
	writeErr error
	events   []firedEvent
	logBuf   bytes.Buffer
}

func testWatcherConfig() WatcherConfig {
	return WatcherConfig{
		PollInterval:        10 * time.Second,
		ConfirmReads:        2,
		Settle:              90 * time.Second,
		MaxAttempts:         3,
		AttemptWindow:       time.Hour,
		Cooldown:            5 * time.Minute,
		RunningTimeout:      120 * time.Second,
		RunningPollInterval: 2 * time.Second,
	}
}

func newWatcherFixture(t *testing.T, cfg WatcherConfig) *watcherFixture {
	t.Helper()
	f := newFixture(t)
	wf := &watcherFixture{fixture: f, wifi: filepath.Join(filepath.Dir(f.owner), "remoteproc0")}
	// The passive fixture fakes symlink resolution but never creates the
	// WWAN class directory; the watcher discovers ports by listing it.
	wwanDir := filepath.Join(filepath.Dir(filepath.Dir(f.owner)), "wwan")
	if err := os.MkdirAll(wwanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wwanDir, "wwan0qmi0"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	record := func(kind string) func(context.Context, RecoveryEvent) {
		return func(_ context.Context, ev RecoveryEvent) {
			wf.events = append(wf.events, firedEvent{kind: kind, ev: ev})
		}
	}
	logger := slog.New(slog.NewTextHandler(&wf.logBuf, nil))
	wf.w = newRecoveryWatcher(f.m, cfg, RecoveryEvents{
		OnCrashDetected:     record("crash"),
		OnRecoverySucceeded: record("succeeded"),
		OnRecoveryFailed:    record("failed"),
		OnRecoveryGaveUp:    record("gaveup"),
	}, logger)
	wf.w.writeFile = func(name string, data []byte) error {
		wf.writes = append(wf.writes, string(data))
		if wf.writeErr != nil {
			return wf.writeErr
		}
		return os.WriteFile(name, data, 0o600)
	}
	return wf
}

func (wf *watcherFixture) step() { wf.w.step(context.Background()) }

func (wf *watcherFixture) advance(d time.Duration) { wf.clock = wf.clock.Add(d) }

func (wf *watcherFixture) countEvents(kind string) int {
	count := 0
	for _, event := range wf.events {
		if event.kind == kind {
			count++
		}
	}
	return count
}

func (wf *watcherFixture) assertOnlyRecoverWrites(t *testing.T) {
	t.Helper()
	for _, payload := range wf.writes {
		if payload != "recover" {
			t.Fatalf("watcher wrote unexpected recovery payload %q (writes: %q)", payload, wf.writes)
		}
	}
}

// confirmCrash drives the fixture modem into a confirmed crash and returns
// after the first recovery trigger.
func (wf *watcherFixture) confirmCrash(t *testing.T) {
	t.Helper()
	wf.write(t, "state", "crashed")
	wf.step()
	if len(wf.writes) != 0 || len(wf.events) != 0 {
		t.Fatalf("single crashed read acted on: writes=%q events=%v", wf.writes, wf.events)
	}
	wf.step()
	if wf.countEvents("crash") != 1 || len(wf.writes) != 1 {
		t.Fatalf("confirmed crash: writes=%q events=%v", wf.writes, wf.events)
	}
}

func TestRecoveryWatcherLeavesRunningModemAlone(t *testing.T) {
	wf := newWatcherFixture(t, testWatcherConfig())
	for range 3 {
		wf.step()
		wf.advance(10 * time.Second)
	}
	if len(wf.writes) != 0 || len(wf.events) != 0 {
		t.Fatalf("running modem acted on: writes=%q events=%v", wf.writes, wf.events)
	}
	recovery, err := os.ReadFile(filepath.Join(wf.owner, "recovery"))
	if err != nil || string(recovery) != "disabled\n" {
		t.Fatalf("recovery setting changed: %q %v", recovery, err)
	}
	if _, err := wf.m.checkPort(wf.port); err != nil {
		t.Fatalf("running modem gated: %v", err)
	}
}

func TestRecoveryWatcherRequiresConsecutiveCrashReads(t *testing.T) {
	wf := newWatcherFixture(t, testWatcherConfig())
	wf.write(t, "state", "crashed")
	wf.step()
	wf.write(t, "state", "running")
	wf.step()
	wf.write(t, "state", "crashed")
	wf.step()
	if len(wf.writes) != 0 || len(wf.events) != 0 {
		t.Fatalf("interrupted crash reads acted on: writes=%q events=%v", wf.writes, wf.events)
	}
	wf.step()
	if wf.countEvents("crash") != 1 || len(wf.writes) != 1 {
		t.Fatalf("confirmed crash: writes=%q events=%v", wf.writes, wf.events)
	}
	wf.assertOnlyRecoverWrites(t)
}

func TestRecoveryWatcherWritesRecoverOnceWhileCrashPersists(t *testing.T) {
	wf := newWatcherFixture(t, testWatcherConfig())
	wf.confirmCrash(t)
	// Still crashed: several polls inside the running timeout, then past
	// it, then up to (but not past) the cooldown. No second write may
	// happen and the timeout failure must be reported exactly once.
	for range 5 {
		wf.advance(30 * time.Second)
		wf.step()
	}
	for range 4 {
		wf.advance(30 * time.Second)
		wf.step()
	}
	if len(wf.writes) != 1 {
		t.Fatalf("writes while crash persists: %q", wf.writes)
	}
	if wf.countEvents("failed") != 1 {
		t.Fatalf("failed events: %v", wf.events)
	}
	wf.assertOnlyRecoverWrites(t)
	recovery, err := os.ReadFile(filepath.Join(wf.owner, "recovery"))
	if err != nil || string(recovery) != "recover" {
		t.Fatalf("recovery file after trigger: %q %v", recovery, err)
	}
}

func TestRecoveryWatcherSuccessHoldsGateForSettle(t *testing.T) {
	wf := newWatcherFixture(t, testWatcherConfig())
	wf.confirmCrash(t)
	wf.write(t, "state", "running")
	wf.advance(10 * time.Second)
	wf.step()
	if wf.countEvents("succeeded") != 1 {
		t.Fatalf("succeeded events: %v", wf.events)
	}
	if got := wf.events[1].ev.Attempt; got != 1 {
		t.Fatalf("succeeded attempt = %d, want 1", got)
	}
	// The passive monitor settle is 5s; the watcher's 90s hold must keep
	// the gate closed well beyond it.
	if _, err := wf.m.checkPort(wf.port); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("gate open during hold: %v", err)
	}
	wf.advance(89 * time.Second)
	if _, err := wf.m.checkPort(wf.port); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("gate open before settle elapsed: %v", err)
	}
	wf.advance(2 * time.Second)
	if _, err := wf.m.checkPort(wf.port); err != nil {
		t.Fatalf("gate still closed after settle: %v", err)
	}
}

func TestRecoveryWatcherAttemptWindowAndCooldown(t *testing.T) {
	cfg := testWatcherConfig()
	cfg.RunningTimeout = 30 * time.Second
	wf := newWatcherFixture(t, cfg)
	wf.confirmCrash(t)

	failAttempt := func() {
		t.Helper()
		wf.advance(31 * time.Second)
		wf.step()
	}
	// Attempt 1 times out; an immediate re-poll is inside the cooldown.
	failAttempt()
	wf.advance(10 * time.Second)
	wf.step()
	if len(wf.writes) != 1 {
		t.Fatalf("cooldown did not defer retry: %q", wf.writes)
	}
	// Attempts 2 and 3 after their cooldowns.
	for want := 2; want <= 3; want++ {
		wf.advance(5 * time.Minute)
		wf.step()
		if len(wf.writes) != want {
			t.Fatalf("attempt %d not triggered: %q", want, wf.writes)
		}
		failAttempt()
	}
	// The window (3 attempts within 1h) is now exhausted: one gave-up
	// event, no fourth write, and no repeated gave-up spam.
	wf.advance(5 * time.Minute)
	wf.step()
	wf.step()
	if len(wf.writes) != 3 {
		t.Fatalf("window cap exceeded: %q", wf.writes)
	}
	if wf.countEvents("failed") != 3 || wf.countEvents("gaveup") != 1 {
		t.Fatalf("events: %v", wf.events)
	}
	wf.assertOnlyRecoverWrites(t)
}

func TestRecoveryWatcherWriteFailureCountsAsFailedAttempt(t *testing.T) {
	wf := newWatcherFixture(t, testWatcherConfig())
	wf.writeErr = errors.New("open /sys/class/remoteproc/remoteproc0/recovery: read-only file system")
	wf.write(t, "state", "crashed")
	wf.step()
	wf.step()
	if len(wf.writes) != 1 || wf.countEvents("failed") != 1 {
		t.Fatalf("write failure: writes=%q events=%v", wf.writes, wf.events)
	}
	failed := wf.events[1]
	if failed.kind != "failed" || failed.ev.Err == nil {
		t.Fatalf("failed event carries no error: %+v", failed)
	}
	if !bytes.Contains(wf.logBuf.Bytes(), []byte("level=ERROR")) ||
		!bytes.Contains(wf.logBuf.Bytes(), []byte("read-only file system")) {
		t.Fatalf("write failure not logged at error level with cause:\n%s", wf.logBuf.String())
	}
	// Failed writes still consume window budget: two more failures after
	// their cooldowns exhaust the cap and produce the gave-up event.
	for range 2 {
		wf.advance(5*time.Minute + time.Second)
		wf.step()
	}
	wf.advance(5*time.Minute + time.Second)
	wf.step()
	if len(wf.writes) != 3 || wf.countEvents("failed") != 3 || wf.countEvents("gaveup") != 1 {
		t.Fatalf("after failures: writes=%q events=%v", wf.writes, wf.events)
	}
	wf.assertOnlyRecoverWrites(t)
}

func TestRecoveryWatcherNeverTouchesWiFiProcessor(t *testing.T) {
	wf := newWatcherFixture(t, testWatcherConfig())
	// The fixture's Wi-Fi remoteproc0 sits in state=crashed the whole
	// time; it does not own the WWAN port and must never be written to or
	// reported on.
	wf.confirmCrash(t)
	for _, event := range wf.events {
		if event.ev.Path == wf.wifi {
			t.Fatalf("watcher reported on Wi-Fi processor: %+v", event)
		}
	}
	if _, err := os.Stat(filepath.Join(wf.wifi, "recovery")); !os.IsNotExist(err) {
		t.Fatalf("Wi-Fi recovery file appeared: %v", err)
	}
	wifiState, err := os.ReadFile(filepath.Join(wf.wifi, "state"))
	if err != nil || string(wifiState) != "crashed" {
		t.Fatalf("Wi-Fi state changed: %q %v", wifiState, err)
	}
}
