// Package remoteproc observes the remote processor owning a native WWAN port.
//
// The passive monitor in this file never starts, stops, or changes recovery
// settings on a remote processor. Triggering a one-shot recovery after a
// confirmed firmware crash is a separate, explicitly opt-in watcher
// (recovery.go); it only ever writes the "recover" trigger value and never
// touches the recovery policy setting.
package remoteproc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrUnavailable = errors.New("modem remote processor is unavailable")

// Observation describes a positively identified WWAN remote processor. An
// empty Path means sysfs did not expose a matching owner (for example in a
// container or on a PCIe modem); callers must retain their normal behavior.
type Observation struct {
	Path       string
	State      string
	Recovery   string
	Generation uint64
}

type history struct {
	generation   uint64
	interrupted  bool
	runningSince time.Time
	// holdUntil extends the post-recovery gate beyond the built-in settle
	// interval. The recovery watcher sets it after triggering a recovery so
	// device clients stay away while the modem firmware settles for longer
	// than the passive debounce.
	holdUntil time.Time
}

type monitor struct {
	sysRoot    string
	resolve    func(string) (string, error)
	now        func() time.Time
	settle     time.Duration
	mu         sync.Mutex
	seen       map[string]*history
	portOwners map[string]string
}

var system = newMonitor("/sys")

func newMonitor(sysRoot string) *monitor {
	return &monitor{sysRoot: sysRoot, resolve: filepath.EvalSymlinks,
		now: time.Now, settle: 5 * time.Second, seen: make(map[string]*history), portOwners: make(map[string]string)}
}

// CheckPort reads Linux's documented remoteproc state/recovery ABI:
// https://www.kernel.org/doc/Documentation/ABI/testing/sysfs-class-remoteproc
// Only an ancestor of this WWAN port is considered: remoteproc0 may be Wi-Fi.
// After an observed interruption, require a short stable running interval
// before letting ordinary clients reopen. This is a debounce, not a guarantee
// that SIM initialization has completed; existing readiness checks still apply.
func CheckPort(port string) (Observation, error) { return system.checkPort(port) }

// wwanKey groups the AT and QMI ports of one wwanN device: they share one
// owner, even when one class symlink disappears during recovery before its
// first AT access.
func wwanKey(name string) string {
	key := name
	end := len("wwan")
	for end < len(name) && name[end] >= '0' && name[end] <= '9' {
		end++
	}
	if end > len("wwan") {
		key = name[:end]
	}
	return key
}

// ownerForPort applies the same sysfs topology walk as checkPort: only an
// ancestor of this WWAN port is considered (remoteproc0 may be Wi-Fi). It
// returns the port's topology key and the owner's sysfs class path, or an
// empty owner when sysfs does not expose a matching processor.
func (m *monitor) ownerForPort(name string) (key, owner string) {
	key = wwanKey(name)
	devicePath, err := m.resolve(filepath.Join(m.sysRoot, "class", "wwan", name))
	if err != nil {
		return key, ""
	}
	entries, err := os.ReadDir(filepath.Join(m.sysRoot, "class", "remoteproc"))
	if err != nil {
		return key, ""
	}
	ownerDepth := 0
	for _, entry := range entries {
		classPath := filepath.Join(m.sysRoot, "class", "remoteproc", entry.Name())
		realPath, resolveErr := m.resolve(classPath)
		if resolveErr != nil || filepath.Base(filepath.Dir(realPath)) != "remoteproc" {
			continue
		}
		parent := filepath.Dir(filepath.Dir(realPath))
		if within(parent, devicePath) && len(parent) > ownerDepth {
			owner, ownerDepth = classPath, len(parent)
		}
	}
	return key, owner
}

// modemOwners returns the sysfs class paths of every remote processor that
// owns a WWAN port. Owners remembered from earlier port checks are included:
// their class symlinks may disappear while the modem is down, which is
// exactly when the recovery watcher still needs to find them.
func (m *monitor) modemOwners() []string {
	set := make(map[string]struct{})
	if entries, err := os.ReadDir(filepath.Join(m.sysRoot, "class", "wwan")); err == nil {
		for _, entry := range entries {
			if _, owner := m.ownerForPort(entry.Name()); owner != "" {
				set[owner] = struct{}{}
			}
		}
	}
	m.mu.Lock()
	for _, owner := range m.portOwners {
		if owner != "" {
			set[owner] = struct{}{}
		}
	}
	m.mu.Unlock()
	owners := make([]string, 0, len(set))
	for owner := range set {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	return owners
}

// setRecoveryHold records that the processor at path has just been triggered
// to recover: it stays gated until the given time even if the passive settle
// interval would let clients back in sooner. The bookkeeping mirrors the
// crash branch of checkPort — the interruption is marked and the generation
// bumped once, so existing clients are rebuilt after the hold expires.
func (m *monitor) setRecoveryHold(path string, until time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item := m.seen[path]
	if item == nil {
		item = &history{generation: 1}
		m.seen[path] = item
	}
	if !item.interrupted {
		item.generation++
		item.interrupted = true
		item.runningSince = time.Time{}
	}
	if until.After(item.holdUntil) {
		item.holdUntil = until
	}
}

func (m *monitor) checkPort(port string) (Observation, error) {
	name := filepath.Base(port)
	if !strings.HasPrefix(name, "wwan") {
		return Observation{}, nil
	}
	key, owner := m.ownerForPort(name)
	if owner == "" {
		return m.missingTopology(key)
	}
	// Serialize the read and transition so two callers cannot apply observations
	// out of order and mistake a recovering processor for a second cold boot.
	m.mu.Lock()
	defer m.mu.Unlock()
	m.portOwners[key] = owner
	item := m.seen[owner]
	state, err := os.ReadFile(filepath.Join(owner, "state"))
	if err != nil {
		if item != nil && item.interrupted {
			return Observation{Path: owner, Generation: item.generation},
				fmt.Errorf("%w: cannot read state during recovery", ErrUnavailable)
		}
		return Observation{}, nil
	}
	obs := Observation{Path: owner, State: strings.TrimSpace(string(state))}
	if recovery, readErr := os.ReadFile(filepath.Join(owner, "recovery")); readErr == nil {
		obs.Recovery = strings.TrimSpace(string(recovery))
	}
	if item == nil {
		item = &history{generation: 1}
		m.seen[owner] = item
	}
	obs.Generation = item.generation
	switch obs.State {
	case "crashed", "offline", "suspended", "invalid", "deleted", "detached":
		if !item.interrupted {
			item.generation++
			obs.Generation = item.generation
		}
		item.interrupted = true
		item.runningSince = time.Time{}
		return obs, fmt.Errorf("%w: state=%s recovery=%s; restore the modem before retrying", ErrUnavailable, obs.State, obs.Recovery)
	case "running", "attached":
		if item.interrupted {
			now := m.now()
			if item.runningSince.IsZero() {
				item.runningSince = now
			}
			gate := item.runningSince.Add(m.settle)
			if item.holdUntil.After(gate) {
				gate = item.holdUntil
			}
			if now.Before(gate) {
				return obs, fmt.Errorf("%w: waiting for stable running state after recovery", ErrUnavailable)
			}
			item.interrupted = false
			item.holdUntil = time.Time{}
		} else if m.now().Before(item.holdUntil) {
			return obs, fmt.Errorf("%w: waiting for stable running state after recovery", ErrUnavailable)
		}
	default:
		if item.interrupted {
			item.runningSince = time.Time{}
			return obs, fmt.Errorf("%w: unknown state during recovery", ErrUnavailable)
		}
	}
	return obs, nil
}

func (m *monitor) missingTopology(port string) (Observation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	owner := m.portOwners[port]
	if item := m.seen[owner]; item != nil && item.interrupted {
		return Observation{Path: owner, Generation: item.generation},
			fmt.Errorf("%w: device sysfs is unavailable during recovery", ErrUnavailable)
	}
	return Observation{}, nil
}

func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
