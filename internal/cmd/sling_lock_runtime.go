package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const slingLockAbandonAfter = 10 * time.Minute

type slingLockKind string

const (
	slingLockKindBead     slingLockKind = "bead"
	slingLockKindAssignee slingLockKind = "assignee"
)

type slingLockInfo struct {
	Kind       slingLockKind `json:"kind"`
	Subject    string        `json:"subject"`
	PID        int           `json:"pid"`
	AcquiredAt time.Time     `json:"acquired_at"`
	Hostname   string        `json:"hostname,omitempty"`
}

type slingLockTruth struct {
	Path         string        `json:"path"`
	Kind         string        `json:"kind"`
	Subject      string        `json:"subject"`
	PID          int           `json:"pid"`
	AcquiredAt   time.Time     `json:"acquired_at"`
	Age          time.Duration `json:"-"`
	State        string        `json:"state"`
	Recoverable  bool          `json:"recoverable"`
	RecoveryNote string        `json:"recovery_note,omitempty"`
}

func slingLockDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "locks", "sling")
}

func slingLockPathForBead(townRoot, beadID string) string {
	return filepath.Join(slingLockDir(townRoot), sanitizeSlingLockSubject(beadID)+".flock")
}

func slingLockPathForAssignee(townRoot, targetAgent string) string {
	return filepath.Join(slingLockDir(townRoot), "assignee_"+sanitizeSlingLockSubject(targetAgent)+".flock")
}

func sanitizeSlingLockSubject(subject string) string {
	return strings.NewReplacer("/", "_", ":", "_").Replace(subject)
}

func slingLockInfoPath(lockPath string) string {
	return lockPath + ".json"
}

func tryAcquireSlingLock(lockPath string, info slingLockInfo) (func(), bool, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644) //nolint:gosec // lock files are local operational state
	if err != nil {
		return nil, false, fmt.Errorf("opening sling lock: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("acquiring sling lock: %w", err)
	}

	info.PID = os.Getpid()
	info.AcquiredAt = time.Now()
	info.Hostname, _ = os.Hostname()
	if err := writeSlingLockInfo(slingLockInfoPath(lockPath), info); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, false, fmt.Errorf("writing sling lock metadata: %w", err)
	}

	release := func() {
		_ = os.Remove(slingLockInfoPath(lockPath))
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	return release, true, nil
}

func writeSlingLockInfo(path string, info slingLockInfo) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sling lock metadata: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil { //nolint:gosec // local operational metadata
		return fmt.Errorf("write temp sling lock metadata: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename sling lock metadata: %w", err)
	}
	return nil
}

func readSlingLockInfo(path string) (*slingLockInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var info slingLockInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func inspectSlingLocks(townRoot string, now time.Time) []slingLockTruth {
	dir := slingLockDir(townRoot)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	metadataByLock := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".flock.json") {
			continue
		}
		lockName := strings.TrimSuffix(entry.Name(), ".json")
		metadataByLock[filepath.Join(dir, lockName)] = filepath.Join(dir, entry.Name())
	}

	var truths []slingLockTruth
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".flock") {
			continue
		}

		lockPath := filepath.Join(dir, entry.Name())
		infoPath, hasMetadata := metadataByLock[lockPath]
		if !hasMetadata {
			truths = append(truths, inspectLegacySlingLock(lockPath, entry, now))
			continue
		}

		info, err := readSlingLockInfo(infoPath)
		if err != nil {
			truths = append(truths, slingLockTruth{
				Path:         infoPath,
				State:        "invalid",
				Recoverable:  true,
				RecoveryNote: "metadata is unreadable",
			})
			continue
		}

		truth := slingLockTruth{
			Path:       infoPath,
			Kind:       string(info.Kind),
			Subject:    info.Subject,
			PID:        info.PID,
			AcquiredAt: info.AcquiredAt,
			Age:        now.Sub(info.AcquiredAt),
		}

		switch {
		case info.PID <= 0 || !pidIsLive(info.PID):
			truth.State = "stale"
			truth.Recoverable = true
			truth.RecoveryNote = "owner pid is not alive"
		case truth.Age > slingLockAbandonAfter:
			truth.State = "abandoned"
			truth.Recoverable = true
			truth.RecoveryNote = fmt.Sprintf("lock age %s exceeds %s", truth.Age.Round(time.Second), slingLockAbandonAfter)
		default:
			truth.State = "active"
		}

		truths = append(truths, truth)
	}

	return truths
}

func inspectLegacySlingLock(lockPath string, entry os.DirEntry, now time.Time) slingLockTruth {
	info, err := entry.Info()
	if err != nil {
		info = nil
	}
	truth := slingLockTruth{
		Path:        lockPath,
		Kind:        legacySlingLockKind(entry.Name()),
		Subject:     legacySlingLockSubject(entry.Name()),
		Recoverable: false,
	}
	if info != nil {
		truth.AcquiredAt = info.ModTime()
		truth.Age = now.Sub(info.ModTime())
	}

	release, locked, err := tryAcquireLegacySlingLock(lockPath)
	switch {
	case err != nil:
		truth.State = "legacy-opaque"
		truth.RecoveryNote = "pre-metadata sling lock could not be inspected"
	case locked:
		release()
		truth.State = "legacy-stale"
		truth.Recoverable = true
		truth.RecoveryNote = "pre-metadata sling lock file is not held"
	default:
		truth.State = "legacy-active"
		truth.RecoveryNote = "pre-metadata sling lock is still held and has no owner metadata"
	}

	return truth
}

func legacySlingLockKind(name string) string {
	base := strings.TrimSuffix(name, ".flock")
	if strings.HasPrefix(base, "assignee_") {
		return string(slingLockKindAssignee)
	}
	return string(slingLockKindBead)
}

func legacySlingLockSubject(name string) string {
	base := strings.TrimSuffix(name, ".flock")
	base = strings.TrimPrefix(base, "assignee_")
	return base
}

func tryAcquireLegacySlingLock(lockPath string) (func(), bool, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644) //nolint:gosec // local operational lock files
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true, nil
}

func recoverSlingLocks(townRoot string, now time.Time) (int, error) {
	truths := inspectSlingLocks(townRoot, now)
	recovered := 0

	for _, truth := range truths {
		if !truth.Recoverable {
			continue
		}

		switch truth.State {
		case "stale", "invalid":
			if err := os.Remove(truth.Path); err != nil && !os.IsNotExist(err) {
				return recovered, fmt.Errorf("removing stale sling lock metadata %s: %w", truth.Path, err)
			}
			recovered++
		case "legacy-stale":
			if err := os.Remove(truth.Path); err != nil && !os.IsNotExist(err) {
				return recovered, fmt.Errorf("removing legacy stale sling lock %s: %w", truth.Path, err)
			}
			recovered++
		case "abandoned":
			if truth.PID > 0 && pidIsLive(truth.PID) {
				proc, err := os.FindProcess(truth.PID)
				if err == nil {
					_ = proc.Signal(syscall.SIGTERM)
				}
				deadline := now.Add(3 * time.Second)
				for time.Now().Before(deadline) {
					if !pidIsLive(truth.PID) {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
			if truth.PID > 0 && pidIsLive(truth.PID) {
				return recovered, fmt.Errorf("sling lock for %s %q is still held by live pid %d after SIGTERM", truth.Kind, truth.Subject, truth.PID)
			}
			if err := os.Remove(truth.Path); err != nil && !os.IsNotExist(err) {
				return recovered, fmt.Errorf("removing abandoned sling lock metadata %s: %w", truth.Path, err)
			}
			recovered++
		}
	}

	return recovered, nil
}

func describeContendedSlingLock(lockPath string) string {
	info, err := readSlingLockInfo(slingLockInfoPath(lockPath))
	if err != nil {
		return "owner unknown"
	}

	age := time.Since(info.AcquiredAt).Round(time.Second)
	switch {
	case info.PID <= 0 || !pidIsLive(info.PID):
		return fmt.Sprintf("stale metadata from dead pid %d, run 'gt rig recover'", info.PID)
	case age > slingLockAbandonAfter:
		return fmt.Sprintf("held by pid %d for %s (older than %s, run 'gt rig recover' for explicit recovery)", info.PID, age, slingLockAbandonAfter)
	default:
		return fmt.Sprintf("held by pid %d for %s", info.PID, age)
	}
}
