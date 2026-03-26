package cmd

import (
	"testing"
	"time"
)

func TestEvaluateRigDoctorTargetFindings_QueuedNudgesWithoutSession(t *testing.T) {
	target := rigDoctorTargetTruth{
		Name:            "witness",
		ExpectedRunning: true,
		TmuxSession:     false,
		TmuxHealth:      "session-dead",
		NudgeQueue:      2,
	}

	findings := evaluateRigDoctorTargetFindings(target)

	assertContainsFinding(t, findings, "configured patrol is not running")
	assertContainsFinding(t, findings, "2 queued nudges are retained for eventual delivery, but the patrol session is absent")
}

func TestEvaluateRigDoctorTargetFindings_SessionWithoutTrackedPID(t *testing.T) {
	target := rigDoctorTargetTruth{
		Name:            "refinery",
		ExpectedRunning: true,
		TmuxSession:     true,
		TmuxHealth:      "healthy",
		TrackedPIDFile:  false,
	}

	findings := evaluateRigDoctorTargetFindings(target)

	assertContainsFinding(t, findings, "tmux session exists but tracked pid file is missing")
}

func TestEvaluateRigDoctorReportFindings_DaemonDown(t *testing.T) {
	report := rigDoctorReport{
		OperationalState: "OPERATIONAL",
		Daemon: rigDoctorDaemonTruth{
			Running: false,
		},
	}

	findings := evaluateRigDoctorReportFindings(report)

	assertContainsFinding(t, findings, "daemon is not running, so configured patrols are not supervised")
}

func TestEvaluateRigDoctorReportFindings_SlingLockRecovery(t *testing.T) {
	report := rigDoctorReport{
		SlingLocks: []slingLockTruth{
			{
				Kind:    "bead",
				Subject: "gt-123",
				PID:     4242,
				State:   "stale",
			},
			{
				Kind:        "assignee",
				Subject:     "gastown/polecats/nux",
				PID:         5252,
				State:       "abandoned",
				Age:         12 * time.Minute,
				Recoverable: true,
			},
		},
	}

	findings := evaluateRigDoctorReportFindings(report)

	assertContainsFinding(t, findings, "stale sling lock for bead \"gt-123\" is left behind by dead pid 4242; run 'gt rig recover'")
	assertContainsFinding(t, findings, "sling lock for assignee \"gastown/polecats/nux\" has been held by pid 5252 for 12m0s; run 'gt rig recover' for explicit recovery")
}

func TestEvaluateRigDoctorReportFindings_LegacySlingLockRecovery(t *testing.T) {
	report := rigDoctorReport{
		SlingLocks: []slingLockTruth{
			{Kind: "bead", Subject: "nr-3kn", State: "legacy-stale"},
			{Kind: "assignee", Subject: "nightrider_rig_polecats_rust", State: "legacy-active"},
		},
	}

	findings := evaluateRigDoctorReportFindings(report)

	assertContainsFinding(t, findings, "legacy sling lock file for bead \"nr-3kn\" is present without owner metadata and is no longer held; run 'gt rig recover'")
	assertContainsFinding(t, findings, "legacy sling lock file for assignee \"nightrider_rig_polecats_rust\" is still held but has no owner metadata; wait for the active sling or restart it under the rebuilt container")
}

func TestEvaluateRigDoctorTargetFindings_StaleQueueEntries(t *testing.T) {
	target := rigDoctorTargetTruth{
		Name:            "witness",
		ExpectedRunning: true,
		TmuxSession:     true,
		TmuxHealth:      "healthy",
		NudgeExpired:    2,
		NudgeMalformed:  1,
		NudgeStaleClaim: 1,
	}

	findings := evaluateRigDoctorTargetFindings(target)

	assertContainsFinding(t, findings, "3 stale nudge queue entries should be pruned with 'gt rig recover'")
	assertContainsFinding(t, findings, "1 stale nudge claim files should be requeued with 'gt rig recover'")
}

func assertContainsFinding(t *testing.T, findings []string, want string) {
	t.Helper()
	for _, finding := range findings {
		if finding == want {
			return
		}
	}
	t.Fatalf("findings %v do not contain %q", findings, want)
}
