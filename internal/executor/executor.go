// Package executor runs repository checks in a credential-free container.
// Its private Unix socket is mounted only into the controller and checker.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"openfactory/internal/model"
)

const outputLimit = 1 << 20

type request struct {
	Workspace string        `json:"workspace"`
	Command   model.Command `json:"command"`
}
type response struct {
	Output         string `json:"output"`
	Error          string `json:"error,omitempty"`
	Infrastructure bool   `json:"infrastructure,omitempty"`
}

// InfrastructureError means no trustworthy check result was obtained. It must
// not trigger another coding attempt as though candidate behavior failed.
type InfrastructureError struct{ Message string }

func (e *InfrastructureError) Error() string        { return e.Message }
func (e *InfrastructureError) Infrastructure() bool { return true }
func unavailable(message string) error              { return &InfrastructureError{Message: message} }

func Run(ctx context.Context, socket, workspace string, command model.Command) (string, error) {
	data, err := json.Marshal(request{workspace, command})
	if err != nil {
		return "", err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 4*time.Hour + 30*time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://checker/run", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", unavailable("isolated check executor unavailable or interrupted")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", unavailable(fmt.Sprintf("isolated check executor HTTP %d", resp.StatusCode))
	}
	var result response
	if err = json.NewDecoder(io.LimitReader(resp.Body, 6*(outputLimit+128)+4096)).Decode(&result); err != nil {
		return "", unavailable("invalid check executor response")
	}
	if len(result.Output) > outputLimit+128 {
		return "", unavailable("check executor output exceeds limit")
	}
	if result.Error != "" {
		if result.Infrastructure {
			return result.Output, unavailable(result.Error)
		}
		return result.Output, errors.New(result.Error)
	}
	return result.Output, nil
}

func Serve(ctx context.Context, socket, root string) error {
	return ServeWithOptions(ctx, socket, root, Options{})
}

// ServeWithOptions enables durable experiments when a private journal and
// operator-enrolled profiles are supplied. The legacy /run route is unchanged.
func ServeWithOptions(ctx context.Context, socket, root string, options Options) error {
	if !filepath.IsAbs(socket) || !filepath.IsAbs(root) {
		return errors.New("executor socket and workspace root must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("executor socket path is not a socket")
		}
		conn, e := net.DialTimeout("unix", socket, time.Second)
		if e == nil {
			conn.Close()
			return errors.New("executor already running")
		}
		if err = os.Remove(socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	experiments, err := newExperimentService(ctx, root, options)
	if err != nil {
		return err
	}
	defer experiments.close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	experiments.routes(mux)
	mux.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
		var input request
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			http.Error(w, "trailing request data", 400)
			return
		}
		output, err := execute(r.Context(), root, input)
		result := response{Output: output}
		if err != nil {
			result.Error = err.Error()
			var failure interface{ Infrastructure() bool }
			result.Infrastructure = errors.As(err, &failure) && failure.Infrastructure()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192, BaseContext: func(net.Listener) context.Context { return ctx }}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-stopped:
		}
	}()
	defer close(stopped)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}

func execute(parent context.Context, root string, input request) (string, error) {
	workspace, err := filepath.EvalSymlinks(input.Workspace)
	if err != nil {
		return "", unavailable("check workspace unavailable")
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", unavailable("executor root unavailable")
	}
	rel, err := filepath.Rel(base, workspace)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", unavailable("check workspace escapes allowed root")
	}
	argv := input.Command.Argv
	if len(argv) == 0 || argv[0] == "" || len(argv) > 128 {
		return "", unavailable("invalid command")
	}
	for _, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return "", unavailable("invalid command argument")
		}
	}
	seconds := input.Command.TimeoutSeconds
	if seconds == 0 {
		seconds = 120
	}
	if seconds < 1 || seconds > 14400 {
		return "", unavailable("invalid check timeout")
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(seconds)*time.Second)
	defer cancel()
	home, err := os.MkdirTemp("", "openfactory-check-home-")
	if err != nil {
		return "", unavailable("executor temporary home unavailable")
	}
	defer os.RemoveAll(home)
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = workspace
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home, "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "GOCACHE=" + filepath.Join(home, "go-cache"), "GOPATH=" + filepath.Join(home, "go"), "GOTELEMETRY=off", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 2 * time.Second
	output := capped{cancel: cancel}
	command.Stdout = &output
	command.Stderr = &output
	err = command.Run()
	if command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	text := output.String()
	if output.Truncated() {
		return text, errors.New("check output exceeded 1 MiB")
	}
	if ctx.Err() != nil {
		return text, errors.New("check timed out or was cancelled")
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return text, unavailable("check executable could not be started")
		}
		return text, errors.New("check command failed")
	}
	return text, nil
}

type capped struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	truncated bool
	cancel    context.CancelFunc
}

func (w *capped) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	remaining := outputLimit - w.buffer.Len()
	if n > remaining {
		if !w.truncated && w.cancel != nil {
			w.cancel()
		}
		w.truncated = true
		p = p[:remaining]
	}
	_, _ = w.buffer.Write(p)
	return n, nil
}
func (w *capped) Truncated() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.truncated }
func (w *capped) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.buffer.String()
	if w.truncated {
		s += "\n[output truncated at 1 MiB]"
	}
	return s
}
