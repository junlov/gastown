package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/witness"
)

var rigRecoverCmd = &cobra.Command{
	Use:   "recover [rig]",
	Short: "Recover patrol runtime drift for a local rig",
	Long: `Recover patrol runtime drift for an operational local rig.

This is the write-side companion to 'gt rig doctor':
  - starts the daemon if supervision is missing
  - removes stale tracked PID files
  - removes or stops stale nudge-poller processes
  - starts or restarts witness/refinery patrol sessions
  - re-tracks live patrol sessions that are missing PID tracking

The command is intentionally explicit and conservative. It only repairs
runtime drift for OPERATIONAL rigs and refuses cases that still need
human judgment, such as a live tracked PID with no tmux session.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRigRecover,
}

func init() {
	rigCmd.AddCommand(rigRecoverCmd)
}

type rigRecoverPlan struct {
	StartDaemon bool
	Patrols     []rigRecoverPatrolPlan
	SlingLocks  []rigRecoverSlingLockPlan
}

type rigRecoverPatrolPlan struct {
	Name             string
	Session          string
	Start            bool
	Restart          bool
	ReTrackPID       bool
	RemoveTrackedPID bool
	StopPoller       bool
	RemovePollerPID  bool
	PruneNudges      bool
	RetainNudges     int
}

type rigRecoverSlingLockPlan struct {
	Kind    string
	Subject string
	State   string
}

func runRigRecover(cmd *cobra.Command, args []string) error {
	rigName, err := resolveRigDoctorName(args)
	if err != nil {
		return err
	}

	townRoot, r, err := getRig(rigName)
	if err != nil {
		return err
	}

	before := buildRigDoctorReport(townRoot, r)
	if err := validateRigRecoverReport(before); err != nil {
		return err
	}

	plan := buildRigRecoverPlan(before)
	if !plan.hasActions() {
		fmt.Printf("%s Rig %s has no recoverable drift\n", style.Success.Render("✓"), rigName)
		return nil
	}

	fmt.Printf("Recovering rig %s...\n", style.Bold.Render(rigName))

	if plan.StartDaemon {
		fmt.Printf("  Starting daemon supervision...\n")
		if err := startDaemonForRecover(townRoot); err != nil {
			return fmt.Errorf("starting daemon: %w", err)
		}
	}

	for _, patrol := range plan.Patrols {
		if err := applyRigRecoverPatrolPlan(townRoot, r, patrol); err != nil {
			return fmt.Errorf("recovering %s: %w", patrol.Name, err)
		}
	}
	if len(plan.SlingLocks) > 0 {
		fmt.Printf("  recovering %d sling lock(s)\n", len(plan.SlingLocks))
		recovered, err := recoverSlingLocks(townRoot, time.Now())
		if err != nil {
			return fmt.Errorf("recovering sling locks: %w", err)
		}
		if recovered > 0 {
			fmt.Printf("  recovered %d sling lock(s)\n", recovered)
		}
	}

	after := buildRigDoctorReport(townRoot, r)
	if len(after.Findings) > 0 {
		return fmt.Errorf("recovery incomplete:\n- %s", strings.Join(after.Findings, "\n- "))
	}

	fmt.Printf("%s Rig %s recovered\n", style.Success.Render("✓"), rigName)
	return nil
}

func validateRigRecoverReport(report rigDoctorReport) error {
	if report.OperationalState != "OPERATIONAL" {
		return fmt.Errorf("rig %s is %s (%s); recovery only supports OPERATIONAL rigs", report.Rig, strings.ToLower(report.OperationalState), report.OperationalSource)
	}

	for _, patrol := range report.Patrols {
		if !patrol.TmuxSession && patrol.TrackedPIDFile && patrol.TrackedPIDLive {
			return fmt.Errorf("%s has a live tracked pid but no tmux session; inspect manually before recovery", patrol.Name)
		}
	}

	return nil
}

func buildRigRecoverPlan(report rigDoctorReport) rigRecoverPlan {
	plan := rigRecoverPlan{
		StartDaemon: !report.Daemon.Running,
	}

	for _, patrol := range report.Patrols {
		patrolPlan := rigRecoverPatrolPlan{
			Name:    patrol.Name,
			Session: patrol.Session,
		}

		if !patrol.TmuxSession && patrol.TrackedPIDFile && !patrol.TrackedPIDLive {
			patrolPlan.RemoveTrackedPID = true
		}
		if patrol.PollerPIDLive && !patrol.TmuxSession {
			patrolPlan.StopPoller = true
		}
		if patrol.PollerPIDFile && !patrol.PollerPIDLive {
			patrolPlan.RemovePollerPID = true
		}
		if patrol.NudgeExpired > 0 || patrol.NudgeMalformed > 0 || patrol.NudgeStaleClaim > 0 {
			patrolPlan.PruneNudges = true
		}
		patrolPlan.RetainNudges = patrol.NudgeQueue

		switch {
		case patrol.ExpectedRunning && !patrol.TmuxSession:
			patrolPlan.Start = true
		case patrol.TmuxHealth == tmux.AgentDead.String() || patrol.TmuxHealth == tmux.AgentHung.String():
			patrolPlan.Restart = true
		case patrol.ExpectedRunning && patrol.TmuxSession && !patrol.TrackedPIDFile:
			patrolPlan.ReTrackPID = true
		}

		if patrolPlan.hasActions() {
			plan.Patrols = append(plan.Patrols, patrolPlan)
		}
	}
	for _, slingLock := range report.SlingLocks {
		if slingLock.Recoverable {
			plan.SlingLocks = append(plan.SlingLocks, rigRecoverSlingLockPlan{
				Kind:    slingLock.Kind,
				Subject: slingLock.Subject,
				State:   slingLock.State,
			})
		}
	}

	return plan
}

func (p rigRecoverPlan) hasActions() bool {
	return p.StartDaemon || len(p.Patrols) > 0 || len(p.SlingLocks) > 0
}

func (p rigRecoverPatrolPlan) hasActions() bool {
	return p.Start || p.Restart || p.ReTrackPID || p.RemoveTrackedPID || p.StopPoller || p.RemovePollerPID || p.PruneNudges
}

func applyRigRecoverPatrolPlan(townRoot string, r *rig.Rig, plan rigRecoverPatrolPlan) error {
	if plan.PruneNudges {
		result, err := nudge.PruneQueue(townRoot, plan.Session)
		if err != nil {
			return err
		}
		if result.RemovedExpired > 0 || result.RemovedBadJSON > 0 || result.RequeuedClaims > 0 {
			fmt.Printf("  %s: pruned stale nudge queue entries", plan.Name)
			if result.RemovedExpired > 0 {
				fmt.Printf(" expired=%d", result.RemovedExpired)
			}
			if result.RemovedBadJSON > 0 {
				fmt.Printf(" malformed=%d", result.RemovedBadJSON)
			}
			if result.RequeuedClaims > 0 {
				fmt.Printf(" requeued-claims=%d", result.RequeuedClaims)
			}
			fmt.Println()
		}
	}

	if plan.RemoveTrackedPID {
		fmt.Printf("  %s: removing stale tracked pid file\n", plan.Name)
		if err := os.Remove(trackedPIDPathForRecover(townRoot, plan.Session)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	if plan.StopPoller {
		fmt.Printf("  %s: stopping orphaned nudge-poller\n", plan.Name)
		if err := nudge.StopPoller(townRoot, plan.Session); err != nil {
			return err
		}
	}

	if plan.RemovePollerPID {
		fmt.Printf("  %s: removing stale nudge-poller pid file\n", plan.Name)
		if err := os.Remove(pollerPIDPathForRecover(townRoot, plan.Session)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	if plan.Restart {
		fmt.Printf("  %s: restarting patrol session\n", plan.Name)
		if err := restartRigRecoverPatrol(r, plan.Name); err != nil {
			return err
		}
	}

	if plan.Start {
		fmt.Printf("  %s: starting patrol session\n", plan.Name)
		if err := startRigRecoverPatrol(r, plan.Name); err != nil {
			return err
		}
	}

	if plan.ReTrackPID {
		fmt.Printf("  %s: re-tracking session pid\n", plan.Name)
		if err := session.TrackSessionPID(townRoot, plan.Session, tmux.NewTmux()); err != nil {
			return err
		}
	}

	if plan.RetainNudges > 0 && (plan.Start || plan.Restart) {
		fmt.Printf("  %s: retaining %d queued nudges for delivery after patrol recovery\n", plan.Name, plan.RetainNudges)
	}

	return nil
}

func startRigRecoverPatrol(r *rig.Rig, name string) error {
	switch name {
	case "witness":
		mgr := witness.NewManager(r)
		if err := mgr.Start(false, "", nil); err != nil && err != witness.ErrAlreadyRunning {
			return err
		}
		return nil
	case "refinery":
		mgr := refinery.NewManager(r)
		if err := mgr.Start(false, ""); err != nil && err != refinery.ErrAlreadyRunning {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unsupported patrol %q", name)
	}
}

func restartRigRecoverPatrol(r *rig.Rig, name string) error {
	switch name {
	case "witness":
		mgr := witness.NewManager(r)
		_ = mgr.Stop()
		return mgr.Start(false, "", nil)
	case "refinery":
		mgr := refinery.NewManager(r)
		if err := mgr.Stop(); err != nil && err != refinery.ErrNotRunning {
			return err
		}
		return mgr.Start(false, "")
	default:
		return fmt.Errorf("unsupported patrol %q", name)
	}
}

func startDaemonForRecover(townRoot string) error {
	running, _, err := daemon.IsRunning(townRoot)
	if err != nil {
		return err
	}
	if running {
		return nil
	}

	gtPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}

	cmd := exec.Command(gtPath, "daemon", "run")
	cmd.Dir = townRoot
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return err
	}

	for range 30 {
		time.Sleep(100 * time.Millisecond)
		running, _, err = daemon.IsRunning(townRoot)
		if err != nil {
			return err
		}
		if running {
			return nil
		}
	}

	return fmt.Errorf("daemon failed to start (check logs with 'gt daemon logs')")
}

func trackedPIDPathForRecover(townRoot, sessionName string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "pids", sessionName+".pid")
}

func pollerPIDPathForRecover(townRoot, sessionName string) string {
	safe := strings.ReplaceAll(sessionName, "/", "_")
	return filepath.Join(townRoot, constants.DirRuntime, "nudge_poller", safe+".pid")
}
