package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	stateOnline  = "online"
	stateOffline = "offline"
	stateError   = "error"
)

// Outcome of one check
type CheckResult struct {
	State  string
	Reason string
}

// Host paired with its outcome
type CheckedHost struct {
	Record HostRecord
	Result CheckResult
}

// Run all checks with separate ping and SSH limits; order is preserved
func checkHosts(ctx context.Context, records []HostRecord, settings Settings, bar *progress) []CheckedHost {
	pingLimit := make(chan struct{}, settings.PingConcurrency)
	sshLimit := make(chan struct{}, settings.SSHConcurrency)
	checked := make([]CheckedHost, len(records))
	var group sync.WaitGroup
	for index, record := range records {
		group.Go(func() {
			limit, timeout := pingLimit, settings.PingTimeout
			if record.Method == "ssh" {
				limit, timeout = sshLimit, settings.SSHTimeout
			}
			checked[index] = CheckedHost{record, CheckResult{stateError, "check cancelled"}}
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
				checked[index].Result = checkHost(ctx, record, timeout)
				bar.step(record.Name)
			case <-ctx.Done():
			}
		})
	}
	group.Wait()
	return checked
}

// One check under a hard deadline
func checkHost(ctx context.Context, record HostRecord, timeout time.Duration) CheckResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var result CheckResult
	var err error
	if record.Method == "ping" {
		result, err = ping(ctx, record)
	} else {
		result, err = sshLogin(ctx, record, timeout)
	}
	switch {
	case err == nil:
		return result
	case errors.Is(err, context.DeadlineExceeded):
		return CheckResult{stateOffline, "timeout"}
	case errors.Is(err, exec.ErrNotFound):
		return CheckResult{stateError, "missing local dependency: " + record.Method}
	}
	return CheckResult{stateError, fmt.Sprintf("check failed: %v", err)}
}

// Single echo request via the system ping
func ping(ctx context.Context, record HostRecord) (CheckResult, error) {
	command := exec.CommandContext(ctx, "ping", "-"+strconv.Itoa(record.IPVersion), "-n", "-c", "1", record.IP)
	command.Env = append(os.Environ(), "LC_ALL=C")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	// Graceful stop first, kill after the delay
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 500 * time.Millisecond

	err := command.Run()
	if ctx.Err() != nil {
		return CheckResult{}, ctx.Err()
	}
	if err == nil {
		return CheckResult{stateOnline, "echo reply received"}, nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return CheckResult{}, err
	}
	failure := pingFailure(stderr.String())
	if failure != "target unreachable" && (exit.ExitCode() == 2 || strings.HasPrefix(failure, "local ")) {
		return CheckResult{stateError, failure}, nil
	}
	return CheckResult{stateOffline, failure}, nil
}

// Authenticate without a remote command; online once the control socket appears
func sshLogin(ctx context.Context, record HostRecord, timeout time.Duration) (CheckResult, error) {
	directory, err := os.MkdirTemp("", projectName+"-")
	if err != nil {
		return CheckResult{}, err
	}
	defer os.RemoveAll(directory)
	socket := filepath.Join(directory, "control")

	command := exec.Command("ssh", sshArguments(record, socket, timeout)...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return CheckResult{}, err
	}
	exited := make(chan struct{})
	go func() {
		command.Wait()
		close(exited)
	}()

	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-exited:
			return sshFailure(stderr.String()), nil
		case <-ctx.Done():
			stopProcess(command, exited)
			return CheckResult{}, ctx.Err()
		case <-poll.C:
			if _, err := os.Stat(socket); err == nil {
				stopProcess(command, exited)
				return CheckResult{stateOnline, "SSH authentication succeeded"}, nil
			}
		}
	}
}

// Non-interactive client options with strict host keys
func sshArguments(record HostRecord, socket string, timeout time.Duration) []string {
	connectTimeout := max(1, int(math.Ceil(timeout.Seconds())))
	return []string{
		"-N", "-T", "-M", "-S", socket,
		"-o", "ControlMaster=yes",
		"-o", "ControlPersist=no",
		"-o", "BatchMode=yes",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "NumberOfPasswordPrompts=0",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "ConnectionAttempts=1",
		"-o", "ConnectTimeout=" + strconv.Itoa(connectTimeout),
		"-p", strconv.Itoa(record.SSHPort),
		"-l", record.SSHUser,
		record.IP,
	}
}

// Terminate, then kill after a short grace period
func stopProcess(command *exec.Cmd, exited <-chan struct{}) {
	command.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(500 * time.Millisecond):
		command.Process.Kill()
		<-exited
	}
}

func pingFailure(stderr string) string {
	message := strings.ToLower(stderr)
	switch {
	case strings.Contains(message, "operation not permitted"), strings.Contains(message, "permission denied"):
		return "local ping permission denied"
	case strings.Contains(message, "unreachable"):
		return "target unreachable"
	case strings.Contains(message, "unknown host"), strings.Contains(message, "name or service not known"):
		return "invalid ping target"
	}
	return "no echo reply"
}

func sshFailure(stderr string) CheckResult {
	message := strings.ToLower(stderr)
	contains := func(parts ...string) bool {
		for _, part := range parts {
			if strings.Contains(message, part) {
				return true
			}
		}
		return false
	}
	switch {
	case contains("host key verification failed", "remote host identification has changed"):
		return CheckResult{stateOffline, "host-key verification failed"}
	case contains("permission denied", "no supported authentication methods"):
		return CheckResult{stateOffline, "authentication failed"}
	case contains("connection refused"):
		return CheckResult{stateOffline, "connection refused"}
	case contains("timed out"):
		return CheckResult{stateOffline, "connection timeout"}
	case contains("no route to host", "network is unreachable"):
		return CheckResult{stateOffline, "target unreachable"}
	}
	return CheckResult{stateError, "SSH client failed"}
}
