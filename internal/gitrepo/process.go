package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"openfactory/internal/executor"
	"openfactory/internal/model"
)

const outputLimit = 1024 * 1024

type cappedBuffer struct {
	mu        sync.Mutex
	b         bytes.Buffer
	truncated bool
	cancel    context.CancelFunc
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := outputLimit - b.b.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
		if b.cancel != nil {
			b.cancel()
		}
	}
	_, _ = b.b.Write(p)
	return n, nil
}
func (b *cappedBuffer) output() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.b.String()
	if b.truncated {
		s += "\n[output truncated]"
	}
	return s
}

// Deliberately do not inherit controller, provider, SSH-agent or Git credentials.
func cleanEnv(home string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	return []string{"PATH=" + path, "HOME=" + home, "LANG=C.UTF-8", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1"}
}

// Git stdout is a protocol (SHAs, NUL-delimited paths, remote refs), not a log.
// Successful stderr warnings must never contaminate that protocol.
func runGit(ctx context.Context, dir string, argv, extra, secrets []string, seconds int) (string, error) {
	return runCommand(ctx, dir, argv, extra, secrets, seconds, true)
}

func run(ctx context.Context, dir string, argv, extra, secrets []string, seconds int) (string, error) {
	return runCommand(ctx, dir, argv, extra, secrets, seconds, false)
}

func runCommand(ctx context.Context, dir string, argv, extra, secrets []string, seconds int, structured bool) (string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return "", errors.New("empty command")
	}
	if seconds < 0 || seconds > 14400 {
		return "", errors.New("command timeout must be between zero (default) and four hours")
	}
	if seconds == 0 {
		seconds = 120
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	home, err := os.MkdirTemp("", "openfactory-command-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(home)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(home), extra...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	out := cappedBuffer{cancel: cancel}
	stderr := cappedBuffer{cancel: cancel}
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err = cmd.Run()
	// Reap descendants even when their parent exits without waiting for them.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	text := out.output() + stderr.output()
	if structured && err == nil && ctx.Err() == nil && !out.truncated && !stderr.truncated {
		return out.output(), nil
	}
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, "[REDACTED]")
		}
	}
	if ctx.Err() != nil {
		return text, fmt.Errorf("command interrupted: %w", ctx.Err())
	}
	if err != nil {
		return text, fmt.Errorf("command failed: %w: %s", err, text)
	}
	if out.truncated || stderr.truncated {
		return text, errors.New("command output exceeded limit")
	}
	return text, nil
}

func (m *Manager) Check(ctx context.Context, workspace string, command model.Command) (string, error) {
	r, err := m.workspace(ctx, workspace)
	if err != nil {
		return "", err
	}
	enrolled := false
	for _, approved := range m.cfg.Projects[r.Project].Checks {
		if reflect.DeepEqual(command, approved) {
			enrolled = true
			break
		}
	}
	if !enrolled {
		return "", errors.New("check command is not an enrolled profile")
	}
	if m.cfg.CheckSocket != "" {
		out, e := executor.Run(ctx, m.cfg.CheckSocket, workspace, command)
		detail := ""
		if e != nil {
			detail = e.Error()
		}
		for _, secret := range m.secretValues() {
			if secret != "" {
				out = strings.ReplaceAll(out, secret, "[REDACTED]")
				detail = strings.ReplaceAll(detail, secret, "[REDACTED]")
			}
		}
		if e != nil {
			return out, &checkError{cause: e, detail: detail}
		}
		return out, nil
	}
	return run(ctx, workspace, command.Argv, nil, m.secretValues(), command.TimeoutSeconds)
}

type checkError struct {
	cause  error
	detail string
}

func (e *checkError) Error() string { return e.detail }
func (e *checkError) Unwrap() error { return e.cause }
func (e *checkError) Infrastructure() bool {
	var infrastructure interface{ Infrastructure() bool }
	return errors.As(e.cause, &infrastructure) && infrastructure.Infrastructure()
}

func safeFile(path string, private bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("secret must be a regular non-symlink file")
	}
	if private && info.Mode().Perm()&0077 != 0 {
		return errors.New("secret file must not be accessible by group or others")
	}
	if info.Mode().Perm()&0022 != 0 {
		return errors.New("authentication file must not be group or world writable")
	}
	if !filepath.IsAbs(path) {
		return errors.New("secret path must be absolute")
	}
	return nil
}
