package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
)

var rigDoctorJSON bool

var rigDoctorCmd = &cobra.Command{
	Use:   "doctor [rig]",
	Short: "Check what a rig is actually running",
	Long: `Run a runtime truth check for a local rig.

This compares configured patrol expectations against observable runtime signals:
  - daemon supervision state
  - tmux session existence and health
  - tracked session PID files
  - queued nudges
  - nudge-poller PID files

The goal is operator trust: show what is actually alive, what is only configured,
and where those signals disagree.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRigDoctor,
}

func init() {
	rigDoctorCmd.Flags().BoolVar(&rigDoctorJSON, "json", false, "Output as JSON")
	rigCmd.AddCommand(rigDoctorCmd)
}

type rigDoctorReport struct {
	Rig               string                 `json:"rig"`
	OperationalState  string                 `json:"operational_state"`
	OperationalSource string                 `json:"operational_source"`
	Daemon            rigDoctorDaemonTruth   `json:"daemon"`
	Patrols           []rigDoctorTargetTruth `json:"patrols"`
	SlingLocks        []slingLockTruth       `json:"sling_locks,omitempty"`
	Findings          []string               `json:"findings"`
}

type rigDoctorDaemonTruth struct {
	Running       bool      `json:"running"`
	PID           int       `json:"pid,omitempty"`
	LastHeartbeat time.Time `json:"last_heartbeat,omitempty"`
}

type rigDoctorTargetTruth struct {
	Name            string   `json:"name"`
	Role            string   `json:"role"`
	Session         string   `json:"session"`
	ExpectedRunning bool     `json:"expected_running"`
	TmuxSession     bool     `json:"tmux_session"`
	TmuxHealth      string   `json:"tmux_health"`
	TrackedPIDFile  bool     `json:"tracked_pid_file"`
	TrackedPID      int      `json:"tracked_pid,omitempty"`
	TrackedPIDLive  bool     `json:"tracked_pid_live"`
	NudgeQueue      int      `json:"nudge_queue"`
	NudgeReady      int      `json:"nudge_ready"`
	NudgeDeferred   int      `json:"nudge_deferred"`
	NudgeExpired    int      `json:"nudge_expired"`
	NudgeMalformed  int      `json:"nudge_malformed"`
	NudgeStaleClaim int      `json:"nudge_stale_claim"`
	PollerPIDFile   bool     `json:"poller_pid_file"`
	PollerPID       int      `json:"poller_pid,omitempty"`
	PollerPIDLive   bool     `json:"poller_pid_live"`
	Findings        []string `json:"findings,omitempty"`
}

type pidTruth struct {
	Exists bool
	PID    int
	Live   bool
}

func runRigDoctor(cmd *cobra.Command, args []string) error {
	rigName, err := resolveRigDoctorName(args)
	if err != nil {
		return err
	}

	townRoot, r, err := getRig(rigName)
	if err != nil {
		return err
	}

	report := buildRigDoctorReport(townRoot, r)

	if rigDoctorJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	printRigDoctorReport(report)
	return nil
}

func resolveRigDoctorName(args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}

	roleInfo, err := GetRole()
	if err != nil {
		return "", fmt.Errorf("detecting rig from current directory: %w", err)
	}
	if roleInfo.Rig == "" {
		return "", fmt.Errorf("could not detect rig from current directory; please specify rig name")
	}
	return roleInfo.Rig, nil
}

func buildRigDoctorReport(townRoot string, r *rig.Rig) rigDoctorReport {
	opState, opSource := getRigOperationalState(townRoot, r.Name)

	daemonRunning, daemonPID, _ := daemon.IsRunning(townRoot)
	daemonState, _ := daemon.LoadState(townRoot)

	report := rigDoctorReport{
		Rig:               r.Name,
		OperationalState:  opState,
		OperationalSource: opSource,
		Daemon: rigDoctorDaemonTruth{
			Running: daemonRunning,
			PID:     daemonPID,
		},
	}
	if daemonState != nil {
		report.Daemon.LastHeartbeat = daemonState.LastHeartbeat
	}

	expectedPatrols := opState == "OPERATIONAL"
	prefix := session.PrefixFor(r.Name)
	t := tmux.NewTmux()

	report.Patrols = []rigDoctorTargetTruth{
		inspectRigDoctorTarget(townRoot, t, "witness", "patrol", session.WitnessSessionName(prefix), expectedPatrols),
		inspectRigDoctorTarget(townRoot, t, "refinery", "patrol", session.RefinerySessionName(prefix), expectedPatrols),
	}
	report.SlingLocks = inspectSlingLocks(townRoot, time.Now())

	report.Findings = evaluateRigDoctorReportFindings(report)
	return report
}

func inspectRigDoctorTarget(townRoot string, t *tmux.Tmux, name, role, sessionName string, expectedRunning bool) rigDoctorTargetTruth {
	hasSession, _ := t.HasSession(sessionName)
	health := t.CheckSessionHealth(sessionName, 0).String()
	trackedPID := readPIDTruth(filepath.Join(townRoot, constants.DirRuntime, "pids", sessionName+".pid"))
	pollerPID := readPIDTruth(filepath.Join(townRoot, constants.DirRuntime, "nudge_poller", strings.ReplaceAll(sessionName, "/", "_")+".pid"))
	queueStats, _ := nudge.InspectQueue(townRoot, sessionName)

	target := rigDoctorTargetTruth{
		Name:            name,
		Role:            role,
		Session:         sessionName,
		ExpectedRunning: expectedRunning,
		TmuxSession:     hasSession,
		TmuxHealth:      health,
		TrackedPIDFile:  trackedPID.Exists,
		TrackedPID:      trackedPID.PID,
		TrackedPIDLive:  trackedPID.Live,
		NudgeQueue:      queueStats.Retained(),
		NudgeReady:      queueStats.Ready,
		NudgeDeferred:   queueStats.Deferred,
		NudgeExpired:    queueStats.Expired,
		NudgeMalformed:  queueStats.Malformed,
		NudgeStaleClaim: queueStats.StaleClaims,
		PollerPIDFile:   pollerPID.Exists,
		PollerPID:       pollerPID.PID,
		PollerPIDLive:   pollerPID.Live,
	}
	target.Findings = evaluateRigDoctorTargetFindings(target)
	return target
}

func evaluateRigDoctorReportFindings(report rigDoctorReport) []string {
	var findings []string

	if report.OperationalState == "OPERATIONAL" && !report.Daemon.Running {
		findings = append(findings, "daemon is not running, so configured patrols are not supervised")
	}

	if report.OperationalState != "OPERATIONAL" {
		for _, patrol := range report.Patrols {
			if patrol.TmuxSession {
				findings = append(findings, fmt.Sprintf("%s patrol session is running even though rig state is %s", patrol.Name, strings.ToLower(report.OperationalState)))
			}
		}
	}

	for _, patrol := range report.Patrols {
		for _, finding := range patrol.Findings {
			findings = append(findings, fmt.Sprintf("%s: %s", patrol.Name, finding))
		}
	}
	for _, slingLock := range report.SlingLocks {
		switch slingLock.State {
		case "stale":
			findings = append(findings, fmt.Sprintf("stale sling lock for %s %q is left behind by dead pid %d; run 'gt rig recover'", slingLock.Kind, slingLock.Subject, slingLock.PID))
		case "invalid":
			findings = append(findings, "invalid sling lock metadata should be cleaned with 'gt rig recover'")
		case "abandoned":
			findings = append(findings, fmt.Sprintf("sling lock for %s %q has been held by pid %d for %s; run 'gt rig recover' for explicit recovery", slingLock.Kind, slingLock.Subject, slingLock.PID, slingLock.Age.Round(time.Second)))
		case "legacy-stale":
			findings = append(findings, fmt.Sprintf("legacy sling lock file for %s %q is present without owner metadata and is no longer held; run 'gt rig recover'", slingLock.Kind, slingLock.Subject))
		case "legacy-active":
			findings = append(findings, fmt.Sprintf("legacy sling lock file for %s %q is still held but has no owner metadata; wait for the active sling or restart it under the rebuilt container", slingLock.Kind, slingLock.Subject))
		}
	}

	return findings
}

func evaluateRigDoctorTargetFindings(target rigDoctorTargetTruth) []string {
	var findings []string

	if target.ExpectedRunning && !target.TmuxSession {
		findings = append(findings, "configured patrol is not running")
	}
	if target.TmuxHealth == tmux.AgentDead.String() {
		findings = append(findings, "tmux session exists but the agent process is dead")
	}
	if target.TmuxHealth == tmux.AgentHung.String() {
		findings = append(findings, "tmux session exists but the agent appears hung")
	}
	if target.TmuxSession && !target.TrackedPIDFile {
		findings = append(findings, "tmux session exists but tracked pid file is missing")
	}
	if !target.TmuxSession && target.TrackedPIDFile && target.TrackedPIDLive {
		findings = append(findings, "tracked pid is still live but tmux session is missing")
	}
	if !target.TmuxSession && target.TrackedPIDFile && !target.TrackedPIDLive {
		findings = append(findings, "stale tracked pid file remains after the session exited")
	}
	if target.NudgeQueue > 0 && !target.TmuxSession {
		findings = append(findings, fmt.Sprintf("%d queued nudges are retained for eventual delivery, but the patrol session is absent", target.NudgeQueue))
	}
	if target.NudgeQueue > 0 && target.TmuxHealth == tmux.AgentDead.String() {
		findings = append(findings, fmt.Sprintf("%d queued nudges are retained for eventual delivery, but the patrol is not actually alive", target.NudgeQueue))
	}
	if target.NudgeExpired > 0 || target.NudgeMalformed > 0 {
		staleCount := target.NudgeExpired + target.NudgeMalformed
		findings = append(findings, fmt.Sprintf("%d stale nudge queue entries should be pruned with 'gt rig recover'", staleCount))
	}
	if target.NudgeStaleClaim > 0 {
		findings = append(findings, fmt.Sprintf("%d stale nudge claim files should be requeued with 'gt rig recover'", target.NudgeStaleClaim))
	}
	if target.PollerPIDFile && !target.PollerPIDLive {
		findings = append(findings, "stale nudge-poller pid file remains")
	}
	if target.PollerPIDLive && !target.TmuxSession {
		findings = append(findings, "nudge-poller is live but the patrol session is absent")
	}

	return findings
}

func readPIDTruth(path string) pidTruth {
	data, err := os.ReadFile(path)
	if err != nil {
		return pidTruth{}
	}

	fields := strings.SplitN(strings.TrimSpace(string(data)), "|", 2)
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return pidTruth{Exists: true}
	}

	return pidTruth{
		Exists: true,
		PID:    pid,
		Live:   pidIsLive(pid),
	}
}

func pidIsLive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func printRigDoctorReport(report rigDoctorReport) {
	fmt.Printf("%s\n", style.Bold.Render(report.Rig))
	fmt.Printf("  Operational: %s (%s)\n", report.OperationalState, report.OperationalSource)
	fmt.Printf("  Daemon: %s\n", formatRigDoctorDaemon(report.Daemon))
	fmt.Println()

	fmt.Printf("%s\n", style.Bold.Render("Patrol Truth"))
	for _, patrol := range report.Patrols {
		fmt.Printf("  %s: %s\n", patrol.Name, formatRigDoctorPatrolSummary(patrol))
	}

	if len(report.SlingLocks) > 0 {
		fmt.Println()
		fmt.Printf("%s\n", style.Bold.Render("Sling Locks"))
		for _, slingLock := range report.SlingLocks {
			fmt.Printf("  %s %q: %s\n", slingLock.Kind, slingLock.Subject, formatRigDoctorSlingLockSummary(slingLock))
		}
	}

	fmt.Println()
	fmt.Printf("%s\n", style.Bold.Render("Findings"))
	if len(report.Findings) == 0 {
		fmt.Printf("  %s no mismatches detected\n", style.Success.Render("✓"))
		return
	}

	for _, finding := range report.Findings {
		fmt.Printf("  %s %s\n", style.Warning.Render("!"), finding)
	}
}

func formatRigDoctorDaemon(daemonTruth rigDoctorDaemonTruth) string {
	if !daemonTruth.Running {
		return "not running"
	}

	parts := []string{"running"}
	if daemonTruth.PID > 0 {
		parts = append(parts, fmt.Sprintf("pid %d", daemonTruth.PID))
	}
	if !daemonTruth.LastHeartbeat.IsZero() {
		parts = append(parts, fmt.Sprintf("last heartbeat %s ago", time.Since(daemonTruth.LastHeartbeat).Round(time.Second)))
	}
	return strings.Join(parts, ", ")
}

func formatRigDoctorPatrolSummary(target rigDoctorTargetTruth) string {
	return fmt.Sprintf(
		"tmux=%s health=%s tracked-pid=%s nudge-queue=%d poller=%s expected=%t",
		boolWord(target.TmuxSession),
		target.TmuxHealth,
		describePIDState(target.TrackedPIDFile, target.TrackedPIDLive),
		target.NudgeQueue,
		describePIDState(target.PollerPIDFile, target.PollerPIDLive),
		target.ExpectedRunning,
	)
}

func formatRigDoctorSlingLockSummary(lockTruth slingLockTruth) string {
	parts := []string{lockTruth.State}
	if lockTruth.PID > 0 {
		parts = append(parts, fmt.Sprintf("pid %d", lockTruth.PID))
	}
	if !lockTruth.AcquiredAt.IsZero() {
		parts = append(parts, fmt.Sprintf("age %s", lockTruth.Age.Round(time.Second)))
	}
	if lockTruth.RecoveryNote != "" {
		parts = append(parts, lockTruth.RecoveryNote)
	}
	return strings.Join(parts, ", ")
}

func describePIDState(exists, live bool) string {
	switch {
	case !exists:
		return "missing"
	case live:
		return "live"
	default:
		return "stale"
	}
}

func boolWord(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
