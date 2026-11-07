package device

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"

	"vocat/internal/modem"
)

// WiFi 410 pacing gates (fork-only behavior change).
//
// These constants implement the rhythm gating specified in
// VoCat-crash-analysis-2026-11-07.md section 7 (items F1-F5, dated
// 2026-11-07): on OpenStick 410 (MSM8916) hardware the modem firmware can
// fatal-error (smd_dsm_memcpy.c:297) when UIM/DMS operations pile up while
// the SIM state is still settling after a modem restart, a hot-swap
// recovery, or an eSIM profile switch. The gates below only ever apply when
// the candidate is a WiFi 410 device; every other device type keeps the
// exact upstream pacing.
const (
	// WiFi410ModemQuietPeriod (F1): after the modem processor comes back
	// (a remoteproc generation change), hold off full snapshots and VoWiFi
	// starts for this long. 120s matches the empirically safe idle window
	// observed when the modem was left alone after a crash.
	WiFi410ModemQuietPeriod = 120 * time.Second
	// WiFi410ProfileSwitchCooldown (F3): after a successful post-switch SIM
	// recovery, wait this long before the reconciler may start VoWiFi, so
	// SIM AKA does not pile onto the power-cycle/verification traffic.
	WiFi410ProfileSwitchCooldown = 45 * time.Second
	// wifi410HotSwapNotReadyThreshold (F2): the snapshot-embedded hot-swap
	// power cycle may only be attempted after this many consecutive
	// snapshots reported the SIM not ready.
	wifi410HotSwapNotReadyThreshold = 3
	// wifi410HotSwapMinInterval (F2): minimum spacing between hot-swap
	// power cycles on WiFi 410. Other devices keep
	// nativeQMISIMRecoveryMinInterval (15s).
	wifi410HotSwapMinInterval = 120 * time.Second
	// wifi410ProfileSwitchVerifyInterval (F5): spacing between post-switch
	// ICCID verification attempts on WiFi 410 (1s elsewhere).
	wifi410ProfileSwitchVerifyInterval = 5 * time.Second
	// wifi410ProfileSwitchRefreshSettle and wifi410ProfileSwitchRefreshInterval
	// (F5): post-switch snapshot refresh pacing on WiFi 410 (1s elsewhere).
	wifi410ProfileSwitchRefreshSettle   = 5 * time.Second
	wifi410ProfileSwitchRefreshInterval = 5 * time.Second
)

// nativeQMISIMRecoveryMinInterval is the upstream minimum spacing between
// native QMI SIM hot-swap power cycles for non-WiFi-410 devices.
const nativeQMISIMRecoveryMinInterval = 15 * time.Second

// isWiFi410Candidate reports whether a discovery candidate is an OpenStick
// 410 — the hardware cmd/vocat's provisionedDeviceType maps to
// store.DeviceTypeWiFi410. The checks mirror that mapping exactly (ZXIC and
// ML307 USB devices excluded first, then native WWAN QMI), so the pacing
// gates apply to precisely the same hardware set as the provisioned type.
func isWiFi410Candidate(candidate modem.Candidate) bool {
	if modem.ChipsetForUSB(candidate.VendorID, candidate.ProductID) == modem.ChipsetZXIC {
		return false
	}
	if modem.IsML307(candidate) {
		return false
	}
	controlName := filepath.Base(filepath.Clean(candidate.QMIControl))
	return candidate.HardwareKind == "wwan" &&
		strings.HasPrefix(controlName, "wwan") && strings.Contains(controlName, "qmi")
}

// isWiFi410Device resolves a managed device to the WiFi 410 pacing class.
func (manager *Manager) isWiFi410Device(id string) bool {
	state, err := manager.lookup(id)
	if err != nil {
		return false
	}
	return isWiFi410Candidate(manager.candidateFor(state))
}

// hotSwapRecoveryMinInterval selects the hot-swap power-cycle spacing for a
// device class (F2).
func hotSwapRecoveryMinInterval(wifi410 bool) time.Duration {
	if wifi410 {
		return wifi410HotSwapMinInterval
	}
	return nativeQMISIMRecoveryMinInterval
}

// snapshotHotSwapThresholdReached reports whether the snapshot-embedded
// hot-swap recovery may be attempted after consecutiveNotReady consecutive
// not-ready snapshots (F2). Non-WiFi-410 devices have no threshold.
func snapshotHotSwapThresholdReached(wifi410 bool, consecutiveNotReady int) bool {
	if !wifi410 {
		return true
	}
	return consecutiveNotReady >= wifi410HotSwapNotReadyThreshold
}

// modemQuietUntil returns the end of the post-restart quiet window (F1), or
// the zero time when no modem restart has been observed.
func modemQuietUntil(lastRestart time.Time) time.Time {
	if lastRestart.IsZero() {
		return time.Time{}
	}
	return lastRestart.Add(WiFi410ModemQuietPeriod)
}

// inModemQuietPeriod reports whether now falls inside the quiet window that
// follows a modem restart (F1).
func inModemQuietPeriod(lastRestart, now time.Time) bool {
	until := modemQuietUntil(lastRestart)
	return !until.IsZero() && now.Before(until)
}

// profileSwitchRefreshPacing selects the post-switch snapshot refresh
// settle/interval pair (F5).
func profileSwitchRefreshPacing(wifi410 bool) (settle, interval time.Duration) {
	if wifi410 {
		return wifi410ProfileSwitchRefreshSettle, wifi410ProfileSwitchRefreshInterval
	}
	return time.Second, time.Second
}

// profileSwitchVerifyInterval selects the post-switch ICCID verification
// spacing (F5).
func profileSwitchVerifyInterval(wifi410 bool) time.Duration {
	if wifi410 {
		return wifi410ProfileSwitchVerifyInterval
	}
	return time.Second
}

// LastModemRestart returns when the modem remote processor owning this
// device most recently changed generation (a crash/recovery cycle, or the
// first observation after process start), or the zero time if unknown.
func (manager *Manager) LastModemRestart(id string) time.Time {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if state := manager.devices[id]; state != nil {
		return state.lastModemRestart
	}
	return time.Time{}
}

// ModemQuietUntil returns the end of the F1 quiet window for this device,
// or the zero time when no modem restart has been observed for it.
func (manager *Manager) ModemQuietUntil(id string) time.Time {
	return modemQuietUntil(manager.LastModemRestart(id))
}

// inModemQuietPeriod reports whether the device is currently inside the F1
// quiet window after a modem restart.
func (manager *Manager) inModemQuietPeriod(id string, now time.Time) bool {
	return inModemQuietPeriod(manager.LastModemRestart(id), now)
}

// SIMRecoveredAt returns when the most recent post-profile-switch SIM
// recovery completed successfully for this device (F3), or the zero time.
func (manager *Manager) SIMRecoveredAt(id string) time.Time {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if state := manager.devices[id]; state != nil {
		return state.simRecoveredAt
	}
	return time.Time{}
}

// markSIMRecovered records a successful post-profile-switch SIM recovery so
// the VoWiFi reconciler can apply the F3 cooldown.
func (manager *Manager) markSIMRecovered(id string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if state := manager.devices[id]; state != nil {
		state.simRecoveredAt = time.Now()
	}
}

// noteSIMNotReady records one more consecutive snapshot that found the SIM
// not ready and returns the current streak length (F2).
func (manager *Manager) noteSIMNotReady(id string) int {
	manager.simNotReadyMu.Lock()
	defer manager.simNotReadyMu.Unlock()
	if manager.simNotReadyStreak == nil {
		manager.simNotReadyStreak = make(map[string]int)
	}
	manager.simNotReadyStreak[id]++
	return manager.simNotReadyStreak[id]
}

// resetSIMNotReadyStreak clears the consecutive-not-ready streak once a
// snapshot finds the SIM ready again (F2).
func (manager *Manager) resetSIMNotReadyStreak(id string) {
	manager.simNotReadyMu.Lock()
	defer manager.simNotReadyMu.Unlock()
	delete(manager.simNotReadyStreak, id)
}

// snapshotHotSwapRecoveryAllowed applies the F1/F2 gates to the
// snapshot-embedded hot-swap power cycle for one snapshot that found the SIM
// not ready. Non-WiFi-410 candidates are never gated here; the in-session
// rate limit still applies to them.
func (manager *Manager) snapshotHotSwapRecoveryAllowed(id string, candidate modem.Candidate) bool {
	if !isWiFi410Candidate(candidate) {
		return true
	}
	if manager.inModemQuietPeriod(id, time.Now()) {
		return false
	}
	return snapshotHotSwapThresholdReached(true, manager.noteSIMNotReady(id))
}

type nativeQMICardStatusSession interface {
	GetCardStatusDetails(context.Context) (*qmi.CardStatusDetails, qmi.SIMStatus, error)
}

// ProbeNativeQMICardStatus performs the cheapest SIM presence check
// available for a native QMI device: a single QMI card-status read. It
// issues no AT commands, writes no snapshot and never attempts recovery, so
// the snapshot poller can run it at the fast cadence — and throughout the
// F1 quiet window — without pressuring the modem firmware. It reports
// whether the card is present (CardState == 1). Non-native devices report
// (false, nil): the probe simply does not apply to them.
func (manager *Manager) ProbeNativeQMICardStatus(ctx context.Context, id string) (bool, error) {
	state, err := manager.lookup(id)
	if err != nil {
		return false, err
	}
	candidate := manager.candidateFor(state)
	if !isNativeQMICandidate(candidate) || manager.qmiRadioOpener == nil {
		return false, nil
	}
	unlockProfileRead, err := manager.lockProfileSwitchRead(ctx, id, state)
	if err != nil {
		return false, err
	}
	defer unlockProfileRead()
	session, err := manager.qmiRadioOpener(ctx, candidate.QMIControl)
	if err != nil {
		return false, err
	}
	defer session.Close()
	statusSession, ok := session.(nativeQMICardStatusSession)
	if !ok {
		return false, nil
	}
	details, _, err := statusSession.GetCardStatusDetails(ctx)
	if err != nil {
		return false, err
	}
	return details != nil && details.CardState == 1, nil
}
