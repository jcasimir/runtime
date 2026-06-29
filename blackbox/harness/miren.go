package harness

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Miren wraps CLI execution against a target cluster.
type Miren struct {
	t       *testing.T
	cluster *Cluster
	envVars map[string]string
}

// NewMiren creates a CLI runner bound to the given cluster.
func NewMiren(t *testing.T, cluster *Cluster) *Miren {
	t.Helper()
	return &Miren{t: t, cluster: cluster}
}

// SetEnv sets an environment variable that will be injected into subsequent
// miren CLI and RunCmd invocations inside the dev container.
func (m *Miren) SetEnv(key, value string) {
	if m.envVars == nil {
		m.envVars = make(map[string]string)
	}
	m.envVars[key] = value
}

// Run executes a miren CLI command and returns the result.
// In dev mode the command is dispatched through hack/dev-exec.
func (m *Miren) Run(args ...string) *Result {
	m.t.Helper()

	var cmd *exec.Cmd

	switch m.cluster.Mode {
	case ModeDev:
		// hack/dev-exec m <args>
		devExec := filepath.Join(m.cluster.RepoRoot, "hack", "dev-exec")
		execArgs := m.prependEnvArgs("m", args...)
		cmd = exec.Command(devExec, execArgs...)
		cmd.Dir = m.cluster.RepoRoot
	case ModeLocal:
		cmd = exec.Command(m.cluster.MirenBin, args...)
		for k, v := range m.envVars {
			cmd.Env = append(cmd.Environ(), k+"="+v)
		}
	case ModePeers:
		// iso peers exec coordinator -- m <args>
		execArgs := append([]string{"peers", "exec", "coordinator", "--"}, m.prependEnvArgs("m", args...)...)
		cmd = exec.Command("iso", execArgs...)
		cmd.Dir = m.cluster.RepoRoot
	default:
		m.t.Fatalf("unknown mode: %s", m.cluster.Mode)
		return nil
	}

	// Suppress interactive prompts
	cmd.Env = append(cmd.Environ(), "TERM=dumb")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			m.t.Fatalf("failed to execute command: %v", err)
		}
	}

	r := &Result{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}

	m.t.Logf("miren %s -> exit %d", strings.Join(args, " "), exitCode)
	if r.Stdout != "" {
		m.t.Logf("stdout: %s", r.Stdout)
	}
	if r.Stderr != "" {
		m.t.Logf("stderr: %s", r.Stderr)
	}

	return r
}

// MustRun executes a miren CLI command and fails the test on non-zero exit.
func (m *Miren) MustRun(args ...string) *Result {
	m.t.Helper()
	r := m.Run(args...)
	r.RequireSuccess(m.t)
	return r
}

// RunCmd executes an arbitrary command (not miren CLI) in the dev container.
// In local mode it runs the command directly on the host.
func (m *Miren) RunCmd(args ...string) *Result {
	m.t.Helper()
	if len(args) == 0 {
		m.t.Fatalf("RunCmd requires at least one argument")
		return nil
	}

	var cmd *exec.Cmd

	switch m.cluster.Mode {
	case ModeDev:
		devExec := filepath.Join(m.cluster.RepoRoot, "hack", "dev-exec")
		execArgs := m.prependEnvArgs(args[0], args[1:]...)
		cmd = exec.Command(devExec, execArgs...)
		cmd.Dir = m.cluster.RepoRoot
	case ModeLocal:
		cmd = exec.Command(args[0], args[1:]...)
		for k, v := range m.envVars {
			cmd.Env = append(cmd.Environ(), k+"="+v)
		}
	case ModePeers:
		execArgs := append([]string{"peers", "exec", "coordinator", "--"}, m.prependEnvArgs(args[0], args[1:]...)...)
		cmd = exec.Command("iso", execArgs...)
		cmd.Dir = m.cluster.RepoRoot
	default:
		m.t.Fatalf("unknown mode: %s", m.cluster.Mode)
		return nil
	}

	cmd.Env = append(cmd.Environ(), "TERM=dumb")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			m.t.Fatalf("failed to execute command: %v", err)
		}
	}

	r := &Result{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}

	m.t.Logf("cmd %s -> exit %d", strings.Join(args, " "), exitCode)
	if r.Stdout != "" {
		m.t.Logf("stdout: %s", r.Stdout)
	}
	if r.Stderr != "" {
		m.t.Logf("stderr: %s", r.Stderr)
	}

	return r
}

// PeerExec runs a command on a specific iso peer container. This is only
// meaningful in ModePeers and is used for verification tasks like querying
// runner-side state or hitting internal HTTP endpoints.
func (m *Miren) PeerExec(peer string, args ...string) *Result {
	m.t.Helper()
	if m.cluster.Mode != ModePeers {
		m.t.Fatalf("PeerExec requires ModePeers (current: %s)", m.cluster.Mode)
		return nil
	}

	execArgs := append([]string{"peers", "exec", peer, "--"}, args...)
	cmd := exec.Command("iso", execArgs...)
	cmd.Dir = m.cluster.RepoRoot
	cmd.Env = append(cmd.Environ(), "TERM=dumb")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			m.t.Fatalf("PeerExec(%s) failed: %v", peer, err)
		}
	}

	r := &Result{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}

	m.t.Logf("peer(%s) %s -> exit %d", peer, strings.Join(args, " "), exitCode)
	if r.Stdout != "" {
		m.t.Logf("stdout: %s", r.Stdout)
	}
	if r.Stderr != "" {
		m.t.Logf("stderr: %s", r.Stderr)
	}

	return r
}

// BackgroundProcess represents a process running in the dev container.
type BackgroundProcess struct {
	PID     string
	LogFile string
	m       *Miren
}

// Stop sends SIGTERM to the background process (runs as root since
// background processes are started as root).
func (p *BackgroundProcess) Stop() {
	if p.PID == "" {
		return
	}
	p.m.RunCmdAsRoot("bash", "-c", "kill "+p.PID+" 2>/dev/null || true")
}

// Logs returns the contents of the background process log file.
func (p *BackgroundProcess) Logs() string {
	r := p.m.RunCmd("cat", p.LogFile)
	return r.Stdout
}

// RunCmdBackground starts a command in the dev container as a background
// process using nohup. Output is captured to a log file. The process is
// killed via t.Cleanup. Must be called in dev mode.
func (m *Miren) RunCmdBackground(t *testing.T, env map[string]string, args ...string) *BackgroundProcess {
	t.Helper()
	if m.cluster.Mode != ModeDev {
		t.Fatal("RunCmdBackground only supported in dev mode")
	}

	// Generate a unique log file path
	name := filepath.Base(args[0])
	logFile := fmt.Sprintf("/tmp/bb-%s-%d.log", name, time.Now().UnixNano())

	// Build the shell command: export env vars, then nohup the binary
	var exports []string
	for k, v := range env {
		exports = append(exports, fmt.Sprintf("export %s=%s", k, shellQuote(v)))
	}
	sort.Strings(exports) // deterministic order

	var cmdParts []string
	for _, a := range args {
		cmdParts = append(cmdParts, shellQuote(a))
	}

	cmdStr := strings.Join(cmdParts, " ")
	var shellCmd string
	if len(exports) > 0 {
		shellCmd = fmt.Sprintf("%s; nohup %s >%s 2>&1 </dev/null & echo $!",
			strings.Join(exports, "; "), cmdStr, logFile)
	} else {
		shellCmd = fmt.Sprintf("nohup %s >%s 2>&1 </dev/null & echo $!",
			cmdStr, logFile)
	}

	devExec := filepath.Join(m.cluster.RepoRoot, "hack", "dev-exec")
	cmd := exec.Command(devExec, "--root", "bash", "-c", shellCmd)
	cmd.Dir = m.cluster.RepoRoot
	cmd.Env = append(cmd.Environ(), "TERM=dumb")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("RunCmdBackground failed: %v\nstderr: %s", err, stderr.String())
	}

	pid := strings.TrimSpace(stdout.String())
	if pid == "" {
		t.Fatalf("RunCmdBackground: no PID returned\nstderr: %s", stderr.String())
	}

	t.Logf("background %s started (PID %s, log %s)", name, pid, logFile)

	proc := &BackgroundProcess{
		PID:     pid,
		LogFile: logFile,
		m:       m,
	}

	t.Cleanup(func() {
		proc.Stop()
	})

	return proc
}

// RunCmdAsRoot executes a command in the dev container as root.
func (m *Miren) RunCmdAsRoot(args ...string) *Result {
	m.t.Helper()
	if m.cluster.Mode != ModeDev {
		m.t.Fatal("RunCmdAsRoot only supported in dev mode")
		return nil
	}

	devExec := filepath.Join(m.cluster.RepoRoot, "hack", "dev-exec")
	execArgs := append([]string{"--root"}, args...)
	cmd := exec.Command(devExec, execArgs...)
	cmd.Dir = m.cluster.RepoRoot
	cmd.Env = append(cmd.Environ(), "TERM=dumb")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			m.t.Fatalf("failed to execute command: %v", err)
		}
	}

	r := &Result{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}

	m.t.Logf("root-cmd %s → exit %d", strings.Join(args, " "), exitCode)
	if r.Stdout != "" {
		m.t.Logf("stdout: %s", r.Stdout)
	}
	if r.Stderr != "" {
		m.t.Logf("stderr: %s", r.Stderr)
	}

	return r
}

// prependEnvArgs builds a command arg slice with "env K=V" prefixed when
// the Miren instance has env vars set. This makes the env vars available
// inside the dev container since hack/dev-exec constructs a shell command
// from the provided args.
func (m *Miren) prependEnvArgs(binary string, args ...string) []string {
	if len(m.envVars) == 0 {
		return append([]string{binary}, args...)
	}

	result := []string{"env"}
	keys := make([]string, 0, len(m.envVars))
	for k := range m.envVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		result = append(result, k+"="+m.envVars[k])
	}
	result = append(result, binary)
	result = append(result, args...)
	return result
}

// shellQuote wraps a string in single quotes for safe shell interpolation.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// ContainerPath translates a host-side path to a container-internal path.
// In dev mode, the repo is mounted at /src inside the iso container.
func (m *Miren) ContainerPath(hostPath string) string {
	if m.cluster.Mode != ModeDev && m.cluster.Mode != ModePeers {
		return hostPath
	}
	rel, err := filepath.Rel(m.cluster.RepoRoot, hostPath)
	if err != nil {
		m.t.Fatalf("path %q is not under repo root %q: %v", hostPath, m.cluster.RepoRoot, err)
	}
	rel = filepath.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		m.t.Fatalf("path %q is outside repo root %q", hostPath, m.cluster.RepoRoot)
	}
	return filepath.Join("/src", rel)
}
