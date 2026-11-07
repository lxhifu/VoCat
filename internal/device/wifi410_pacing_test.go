package device

import (
	"testing"
	"time"

	"vocat/internal/modem"
)

func wifi410TestCandidate() modem.Candidate {
	// Mirrors a discovered OpenStick 410 (MHI WWAN) candidate.
	return modem.Candidate{
		ID:           "mhi-wwan0",
		HardwareKind: "wwan",
		QMIControl:   "/dev/wwan0qmi0",
	}
}

func TestIsWiFi410Candidate(t *testing.T) {
	tests := []struct {
		name      string
		candidate modem.Candidate
		want      bool
	}{
		{name: "openstick 410 wwan qmi", candidate: wifi410TestCandidate(), want: true},
		{
			name: "usb ec20 with cdc-wdm control",
			candidate: modem.Candidate{
				ID: "usb-2c7c-0125", HardwareKind: "usb",
				VendorID: "2c7c", ProductID: "0125", QMIControl: "/dev/cdc-wdm0",
			},
			want: false,
		},
		{
			name: "wwan kind but non-qmi control",
			candidate: modem.Candidate{
				ID: "mhi-wwan0", HardwareKind: "wwan", QMIControl: "/dev/wwan0at0",
			},
			want: false,
		},
		{
			name: "zxic usb identity excluded first",
			candidate: modem.Candidate{
				ID: "usb-19d2-0536", HardwareKind: "wwan",
				VendorID: "19d2", ProductID: "0536", QMIControl: "/dev/wwan0qmi0",
			},
			want: false,
		},
		{
			name: "ml307 product excluded first",
			candidate: modem.Candidate{
				ID: "usb-ml307", HardwareKind: "wwan", Product: "ML307R",
				QMIControl: "/dev/wwan0qmi0",
			},
			want: false,
		},
		{name: "empty candidate", candidate: modem.Candidate{}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isWiFi410Candidate(test.candidate); got != test.want {
				t.Fatalf("isWiFi410Candidate(%+v) = %v, want %v", test.candidate, got, test.want)
			}
		})
	}
}

func TestHotSwapRecoveryMinInterval(t *testing.T) {
	if got := hotSwapRecoveryMinInterval(true); got != 120*time.Second {
		t.Fatalf("wifi410 interval = %v, want 120s", got)
	}
	if got := hotSwapRecoveryMinInterval(false); got != 15*time.Second {
		t.Fatalf("default interval = %v, want 15s", got)
	}
}

func TestSnapshotHotSwapThresholdReached(t *testing.T) {
	if snapshotHotSwapThresholdReached(false, 0) != true {
		t.Fatal("non-410 devices must have no threshold")
	}
	for streak, want := range map[int]bool{0: false, 1: false, 2: false, 3: true, 4: true} {
		if got := snapshotHotSwapThresholdReached(true, streak); got != want {
			t.Fatalf("wifi410 streak %d: got %v, want %v", streak, got, want)
		}
	}
}

func TestProfileSwitchPacingSelection(t *testing.T) {
	settle, interval := profileSwitchRefreshPacing(true)
	if settle != 5*time.Second || interval != 5*time.Second {
		t.Fatalf("wifi410 refresh pacing = %v/%v, want 5s/5s", settle, interval)
	}
	settle, interval = profileSwitchRefreshPacing(false)
	if settle != time.Second || interval != time.Second {
		t.Fatalf("default refresh pacing = %v/%v, want 1s/1s", settle, interval)
	}
	if got := profileSwitchVerifyInterval(true); got != 5*time.Second {
		t.Fatalf("wifi410 verify interval = %v, want 5s", got)
	}
	if got := profileSwitchVerifyInterval(false); got != time.Second {
		t.Fatalf("default verify interval = %v, want 1s", got)
	}
}

func TestModemQuietPeriod(t *testing.T) {
	now := time.Now()
	if !modemQuietUntil(time.Time{}).IsZero() {
		t.Fatal("no restart observed must mean no quiet window")
	}
	if inModemQuietPeriod(time.Time{}, now) {
		t.Fatal("no restart observed must not be quiet")
	}
	restart := now.Add(-30 * time.Second)
	if until := modemQuietUntil(restart); !until.Equal(restart.Add(WiFi410ModemQuietPeriod)) {
		t.Fatalf("quiet until = %v, want restart+120s", until)
	}
	if !inModemQuietPeriod(restart, now) {
		t.Fatal("30s after restart must still be quiet")
	}
	if inModemQuietPeriod(now.Add(-WiFi410ModemQuietPeriod-time.Second), now) {
		t.Fatal("after 120s the quiet window must be over")
	}
}

func TestSnapshotHotSwapRecoveryAllowedGates(t *testing.T) {
	manager, err := NewManager(Options{})
	if err != nil {
		t.Fatal(err)
	}
	candidate := wifi410TestCandidate()
	// F2: the first two consecutive not-ready snapshots are gated off, the
	// third reaches the threshold.
	if manager.snapshotHotSwapRecoveryAllowed("dev410", candidate) {
		t.Fatal("first not-ready snapshot must not attempt recovery")
	}
	if manager.snapshotHotSwapRecoveryAllowed("dev410", candidate) {
		t.Fatal("second not-ready snapshot must not attempt recovery")
	}
	if !manager.snapshotHotSwapRecoveryAllowed("dev410", candidate) {
		t.Fatal("third consecutive not-ready snapshot must allow recovery")
	}
	// A ready snapshot resets the streak.
	manager.resetSIMNotReadyStreak("dev410")
	if manager.snapshotHotSwapRecoveryAllowed("dev410", candidate) {
		t.Fatal("streak must restart after a ready snapshot")
	}
	// Non-410 candidates are never gated by the streak.
	usb := modem.Candidate{ID: "usb-2c7c-0125", HardwareKind: "usb", VendorID: "2c7c", ProductID: "0125", QMIControl: "/dev/cdc-wdm0"}
	if !manager.snapshotHotSwapRecoveryAllowed("devusb", usb) {
		t.Fatal("non-410 candidates must not be gated")
	}
}

func TestSnapshotHotSwapRecoveryAllowedDuringQuietPeriod(t *testing.T) {
	manager, err := NewManager(Options{})
	if err != nil {
		t.Fatal(err)
	}
	manager.devices["dev410"] = &managedDevice{lastModemRestart: time.Now()}
	candidate := wifi410TestCandidate()
	// F1: even with the streak pre-filled past the threshold, the quiet
	// window blocks the snapshot-embedded recovery outright.
	for i := 0; i < wifi410HotSwapNotReadyThreshold; i++ {
		manager.noteSIMNotReady("dev410")
	}
	if manager.snapshotHotSwapRecoveryAllowed("dev410", candidate) {
		t.Fatal("quiet window must block snapshot hot-swap recovery")
	}
	if got := manager.LastModemRestart("dev410"); got.IsZero() {
		t.Fatal("LastModemRestart must return the recorded restart")
	}
	if until := manager.ModemQuietUntil("dev410"); !until.After(time.Now()) {
		t.Fatal("ModemQuietUntil must be in the future during the window")
	}
	if got := manager.LastModemRestart("unknown"); !got.IsZero() {
		t.Fatal("unknown device must report zero restart")
	}
}

func TestSIMRecoveredAtRecording(t *testing.T) {
	manager, err := NewManager(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := manager.SIMRecoveredAt("dev410"); !got.IsZero() {
		t.Fatal("no recovery recorded yet")
	}
	manager.devices["dev410"] = &managedDevice{}
	manager.markSIMRecovered("dev410")
	if got := manager.SIMRecoveredAt("dev410"); got.IsZero() || time.Since(got) > time.Minute {
		t.Fatalf("SIMRecoveredAt = %v, want ~now", got)
	}
	// Unknown devices are a no-op, not a panic.
	manager.markSIMRecovered("unknown")
}
