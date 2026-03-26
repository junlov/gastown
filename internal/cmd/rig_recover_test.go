package cmd

import "testing"

func TestBuildRigRecoverPlan_CurrentObservedFailureMode(t *testing.T) {
	report := rigDoctorReport{
		Rig:              "gastown",
		OperationalState: "OPERATIONAL",
		Daemon: rigDoctorDaemonTruth{
			Running: false,
		},
		Patrols: []rigDoctorTargetTruth{
			{
				Name:            "witness",
				Session:         "gt-gastown-witness",
				ExpectedRunning: true,
				TmuxSession:     false,
				TmuxHealth:      "session-dead",
				TrackedPIDFile:  true,
				TrackedPIDLive:  false,
			},
			{
				Name:            "refinery",
				Session:         "gt-gastown-refinery",
				ExpectedRunning: true,
				TmuxSession:     false,
				TmuxHealth:      "session-dead",
				NudgeQueue:      1,
			},
		},
	}

	plan := buildRigRecoverPlan(report)

	if !plan.StartDaemon {
		t.Fatalf("expected daemon start")
	}
	if len(plan.Patrols) != 2 {
		t.Fatalf("expected 2 patrol plans, got %d", len(plan.Patrols))
	}

	witnessPlan := plan.Patrols[0]
	if !witnessPlan.Start || !witnessPlan.RemoveTrackedPID {
		t.Fatalf("expected witness plan to start and remove stale pid, got %+v", witnessPlan)
	}

	refineryPlan := plan.Patrols[1]
	if !refineryPlan.Start {
		t.Fatalf("expected refinery plan to start, got %+v", refineryPlan)
	}
}

func TestValidateRigRecoverReportRejectsLiveTrackedPIDWithoutSession(t *testing.T) {
	report := rigDoctorReport{
		Rig:              "gastown",
		OperationalState: "OPERATIONAL",
		Patrols: []rigDoctorTargetTruth{
			{
				Name:            "witness",
				ExpectedRunning: true,
				TmuxSession:     false,
				TrackedPIDFile:  true,
				TrackedPIDLive:  true,
			},
		},
	}

	if err := validateRigRecoverReport(report); err == nil {
		t.Fatalf("expected validation error")
	}
}

func TestBuildRigRecoverPlan_ReTracksHealthySessionWithoutPID(t *testing.T) {
	report := rigDoctorReport{
		Rig:              "gastown",
		OperationalState: "OPERATIONAL",
		Daemon: rigDoctorDaemonTruth{
			Running: true,
		},
		Patrols: []rigDoctorTargetTruth{
			{
				Name:            "witness",
				Session:         "gt-gastown-witness",
				ExpectedRunning: true,
				TmuxSession:     true,
				TmuxHealth:      "healthy",
				TrackedPIDFile:  false,
			},
		},
	}

	plan := buildRigRecoverPlan(report)
	if plan.StartDaemon {
		t.Fatalf("did not expect daemon action")
	}
	if len(plan.Patrols) != 1 {
		t.Fatalf("expected 1 patrol plan, got %d", len(plan.Patrols))
	}
	if !plan.Patrols[0].ReTrackPID {
		t.Fatalf("expected re-track action, got %+v", plan.Patrols[0])
	}
}

func TestBuildRigRecoverPlan_IncludesRecoverableSlingLocks(t *testing.T) {
	report := rigDoctorReport{
		Rig:              "gastown",
		OperationalState: "OPERATIONAL",
		Daemon: rigDoctorDaemonTruth{
			Running: true,
		},
		SlingLocks: []slingLockTruth{
			{Kind: "bead", Subject: "gt-123", State: "stale", Recoverable: true},
			{Kind: "assignee", Subject: "gastown/polecats/nux", State: "active", Recoverable: false},
		},
	}

	plan := buildRigRecoverPlan(report)
	if len(plan.SlingLocks) != 1 {
		t.Fatalf("expected 1 recoverable sling lock, got %+v", plan.SlingLocks)
	}
	if plan.SlingLocks[0].Subject != "gt-123" {
		t.Fatalf("unexpected sling lock plan: %+v", plan.SlingLocks[0])
	}
}
