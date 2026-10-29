package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"openfactory/internal/model"
)

func (s *experimentService) run(parent context.Context, id string) {
	s.mu.Lock()
	request := s.records[id].Request
	receipt := copyReceipt(s.records[id].Receipt)
	s.mu.Unlock()
	finish := func(state, kind, detail string) {
		if parent.Err() != nil && state != "completed" && state != "uncertain" {
			state = "cancelled"
			kind = "cancelled"
			detail = "experiment process group or pre-execution work cancelled"
		}
		receipt.State = state
		receipt.FailureKind = kind
		receipt.Detail = detail
		if receipt.HypothesisOutcome == "" || receipt.HypothesisOutcome == "pending" {
			receipt.HypothesisOutcome = "inconclusive"
		}
		if receipt.ObservationOutcome == "" || receipt.ObservationOutcome == "pending" {
			receipt.ObservationOutcome = "not_evaluated"
		}
		switch state {
		case "uncertain":
			receipt.ExecutionOutcome = "uncertain"
		case "cancelled":
			receipt.ExecutionOutcome = "cancelled"
		case "waiting_prerequisite":
			receipt.ExecutionOutcome = "prerequisite_missing"
		case "failed":
			if receipt.ExecutionOutcome == "" || receipt.ExecutionOutcome == "pending" || receipt.ExecutionOutcome == "running" {
				receipt.ExecutionOutcome = "infrastructure"
			}
		}
		if kind == "output_truncated" {
			receipt.ObservationOutcome = "truncated"
		}
		if kind == "observation_incomplete" {
			receipt.ObservationOutcome = "incomplete"
		}
		now := time.Now().UTC()
		receipt.FinishedAt = &now
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.cancels, id)
		record := s.records[id]
		record.Receipt = receipt
		if err := s.journal.save(id, record); err != nil {
			record.Receipt.State = "uncertain"
			record.Receipt.ExecutionOutcome = "uncertain"
			record.Receipt.HypothesisOutcome = "inconclusive"
			record.Receipt.Conclusive = false
			record.Receipt.Accepted = false
			record.Receipt.FailureKind = "operation_uncertain"
			record.Receipt.Detail = "could not durably record execution outcome"
		}
	}
	if err := s.prerequisite(request.Contract, request.Bundle); err != nil {
		finish("waiting_prerequisite", "prerequisite_missing", err.Error())
		return
	}
	if err := parent.Err(); err != nil {
		finish("cancelled", "cancelled", "cancelled before execution")
		return
	}
	limits := experimentLimits(request.Contract.Limits)
	ctx, cancel := context.WithTimeout(parent, time.Duration(limits.WallSeconds)*time.Second)
	defer cancel()
	execution := filepath.Join(s.journal.root, "runs", id)
	if err := os.MkdirAll(filepath.Dir(execution), 0700); err != nil {
		finish("failed", "infrastructure", "private execution directory unavailable")
		return
	}
	if err := s.blobs.MaterializeBundle(ctx, request.Bundle, execution, model.BundleLimits{MaxFiles: 128, MaxFileBytes: 8 << 20, MaxTotalBytes: 64 << 20}); err != nil {
		finish("failed", "evidence_invalid", "approved bundle could not be verified and copied: "+err.Error())
		return
	}
	home := filepath.Join(s.journal.root, "homes", id)
	if err := os.MkdirAll(home, 0700); err != nil {
		finish("failed", "infrastructure", "private command home unavailable")
		return
	}
	receipt.StartedAt = time.Now().UTC()
	receipt.State = "running"
	receipt.ExecutionOutcome = "running"
	s.mu.Lock()
	s.records[id].Receipt = copyReceipt(receipt)
	err := s.journal.save(id, s.records[id])
	s.mu.Unlock()
	if err != nil {
		finish("uncertain", "operation_uncertain", "could not persist execution start")
		return
	}
	budget := &outputBudget{remaining: limits.MaxOutputBytes, cancel: cancel}
	nonzero := false
	for _, approved := range request.Contract.Commands {
		if ctx.Err() != nil {
			break
		}
		profileName := approved.Profile
		if profileName == "" {
			profileName = request.Contract.Environment.Profile
		}
		profile := s.options.Profiles[profileName]
		argv := append(append([]string(nil), profile.Argv...), approved.Args...)
		binary, lookupErr := exec.LookPath(argv[0])
		if lookupErr != nil {
			finish("waiting_prerequisite", "prerequisite_missing", "approved profile executable is unavailable")
			return
		}
		binary, lookupErr = filepath.EvalSymlinks(binary)
		if lookupErr != nil {
			finish("waiting_prerequisite", "prerequisite_missing", "approved profile executable is unavailable")
			return
		}
		binarySHA, hashErr := binaryIdentity(binary)
		if hashErr != nil {
			finish("failed", "infrastructure", "approved executable identity could not be observed")
			return
		}
		commandReceipt := model.ExperimentCommandReceipt{Argv: argv, BinarySHA: binarySHA, Termination: "unknown"}
		command := exec.CommandContext(ctx, binary, argv[1:]...)
		command.Args = argv
		command.Dir = execution
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home, "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "PYTHONDONTWRITEBYTECODE=1"}
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
		stdout := &experimentOutput{budget: budget}
		stderr := &experimentOutput{budget: budget}
		command.Stdout = stdout
		command.Stderr = stderr
		commandErr := command.Run()
		if command.Process != nil {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		}
		out, outBytes, outTruncated := stdout.snapshot()
		errout, errBytes, errTruncated := stderr.snapshot()
		commandReceipt.StdoutBytes = int64(len(out))
		commandReceipt.StderrBytes = int64(len(errout))
		commandReceipt.StdoutObservedBytes = outBytes
		commandReceipt.StderrObservedBytes = errBytes
		commandReceipt.StdoutTruncated = outTruncated
		commandReceipt.StderrTruncated = errTruncated
		// Captures describe the bounded captured bytes, never pretend to hash an
		// uncaptured suffix. Truncation is recorded separately and is inconclusive.
		outBlob, putErr := s.blobs.Put(context.Background(), out)
		if putErr == nil {
			commandReceipt.StdoutSHA = outBlob.SHA
		}
		errBlob, putErr2 := s.blobs.Put(context.Background(), errout)
		if putErr2 == nil {
			commandReceipt.StderrSHA = errBlob.SHA
		}
		if command.ProcessState != nil {
			code := command.ProcessState.ExitCode()
			if code >= 0 {
				commandReceipt.ExitCode = &code
				commandReceipt.Termination = "exited"
				if code != 0 {
					nonzero = true
				}
			}
			if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				commandReceipt.Signal = status.Signal().String()
				commandReceipt.Termination = "signal"
			}
		}
		receipt.Commands = append(receipt.Commands, commandReceipt)
		if putErr != nil || putErr2 != nil {
			finish("failed", "infrastructure", "command capture could not be persisted")
			return
		}
		if outTruncated || errTruncated {
			finish("failed", "output_truncated", "decisive output was truncated; no hypothesis conclusion")
			return
		}
		if ctx.Err() != nil {
			if parent.Err() != nil {
				finish("cancelled", "cancelled", "experiment process group cancelled")
			} else {
				finish("failed", "timeout", "experiment wall limit exceeded")
			}
			return
		}
		if errors.Is(commandErr, exec.ErrWaitDelay) {
			receipt.ObservationOutcome = "incomplete"
			finish("failed", "capture_incomplete", "command exited but inherited output pipes outlived the capture deadline")
			return
		}
		if commandErr != nil {
			if command.ProcessState == nil {
				finish("waiting_prerequisite", "prerequisite_missing", "approved executable could not be started")
				return
			}
			var exit *exec.ExitError
			if !errors.As(commandErr, &exit) {
				receipt.ObservationOutcome = "incomplete"
				finish("failed", "capture_incomplete", "command ran but complete output capture was not obtained")
				return
			}
		}
		if commandReceipt.ExitCode == nil {
			finish("failed", "unknown_exit", "command termination did not provide a known exit code")
			return
		}
	}
	if ctx.Err() != nil {
		if parent.Err() != nil {
			finish("cancelled", "cancelled", "experiment process group cancelled")
		} else {
			finish("failed", "timeout", "experiment wall limit exceeded")
		}
		return
	}
	receipt.ExecutionOutcome = "completed"
	if nonzero {
		receipt.ExecutionOutcome = "nonzero"
	}
	if err := s.collectObservations(ctx, execution, request.Contract, &receipt, limits); err != nil {
		receipt.ObservationOutcome = "incomplete"
		finish("failed", "observation_incomplete", err.Error())
		return
	}
	receipt.ObservationOutcome = "complete"
	accepted, err := evaluatePredicates(request.Contract, receipt.Observations)
	if err != nil {
		finish("failed", "observation_incomplete", err.Error())
		return
	}
	if accepted && nonzero {
		finish("failed", "nonzero_exit", "approved observation predicates passed, but a command exited nonzero; execution failure is not declared hypothesis falsification")
		return
	}
	receipt.Conclusive = true
	receipt.Accepted = accepted && !nonzero
	receipt.HypothesisOutcome = "falsified"
	if receipt.Accepted {
		receipt.HypothesisOutcome = "supported"
	}
	if !receipt.Accepted {
		finish("completed", "hypothesis_falsified", "known execution and complete typed observations did not satisfy the approved acceptance criteria")
		return
	}
	finish("completed", "", "approved predicates satisfied by experiment-produced observations; independent evaluation is still required")
}
func binaryIdentity(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("binary is not a regular file")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type outputBudget struct {
	mu        sync.Mutex
	remaining int64
	cancel    context.CancelFunc
}
type experimentOutput struct {
	budget    *outputBudget
	buffer    bytes.Buffer
	observed  int64
	truncated bool
}

func (w *experimentOutput) Write(data []byte) (int, error) {
	w.budget.mu.Lock()
	defer w.budget.mu.Unlock()
	n := len(data)
	w.observed += int64(n)
	captured := int64(n)
	if captured > w.budget.remaining {
		captured = w.budget.remaining
		w.truncated = true
		w.budget.cancel()
	}
	_, _ = w.buffer.Write(data[:int(captured)])
	w.budget.remaining -= captured
	return n, nil
}
func (w *experimentOutput) snapshot() ([]byte, int64, bool) {
	w.budget.mu.Lock()
	defer w.budget.mu.Unlock()
	return append([]byte(nil), w.buffer.Bytes()...), w.observed, w.truncated
}

// readOutput opens every path component with O_NOFOLLOW and holds directory
// descriptors. It cannot escape the private run tree through a symlink swap.
func readOutput(rootPath, path string, max int64) ([]byte, error) {
	if !safeMember(path) {
		return nil, errors.New("invalid output path")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	parts := strings.Split(filepath.ToSlash(path), "/")
	current := root
	var parents []*os.Root
	defer func() {
		for _, parent := range parents {
			parent.Close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		file, err := current.OpenFile(part, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.IsDir() {
			file.Close()
			return nil, errors.New("output parent is not a directory")
		} // OpenRoot resolves the held descriptor rather than a mutable agent path.
		next, openErr := os.OpenRoot(fmt.Sprintf("/dev/fd/%d", file.Fd()))
		file.Close()
		if openErr != nil {
			return nil, openErr
		}
		parents = append(parents, next)
		current = next
	}
	file, err := current.OpenFile(parts[len(parts)-1], os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > max {
		return nil, errors.New("output file is not regular or exceeds artifact limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(data)) > max {
		return nil, errors.New("output file exceeds artifact limit")
	}
	after, err := file.Stat()
	if err != nil || before.Size() != after.Size() || before.ModTime() != after.ModTime() || int64(len(data)) != after.Size() {
		return nil, errors.New("output changed during capture")
	}
	return data, nil
}
func (s *experimentService) collectObservations(ctx context.Context, execution string, contract model.ExperimentContract, receipt *model.ExperimentReceipt, limits model.ExperimentLimits) error {
	paths := []string{}
	for _, spec := range contract.Observations {
		paths = append(paths, spec.OutputPath)
	}
	paths = sortedUnique(paths)
	if len(paths) > limits.MaxArtifactFiles {
		return errors.New("artifact file count exceeds approved limit")
	}
	outputs := map[string][]byte{}
	files := []model.ProgramBundleFile{}
	total := int64(0)
	for _, path := range paths {
		data, err := readOutput(execution, path, limits.MaxArtifactBytes-total)
		if err != nil {
			return fmt.Errorf("observation output %q unavailable: %w", path, err)
		}
		total += int64(len(data))
		blob, err := s.blobs.Put(ctx, data)
		if err != nil {
			return err
		}
		outputs[path] = data
		files = append(files, model.ProgramBundleFile{Path: path, ContentSHA: blob.SHA, Bytes: blob.Bytes})
	}
	manifest := struct {
		Files []model.ProgramBundleFile `json:"files"`
	}{files}
	bytes, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	blob, err := s.blobs.Put(ctx, bytes)
	if err != nil {
		return err
	}
	receipt.Artifacts = model.ProgramBundle{ManifestSHA: blob.SHA, Files: files}
	observationBytes := int64(0)
	for _, spec := range contract.Observations {
		value, err := decodeObservation(outputs[spec.OutputPath], spec)
		if err != nil {
			return fmt.Errorf("observation %q: %w", spec.Name, err)
		}
		valueJSON, err := json.Marshal(value)
		if err != nil {
			return err
		}
		observationBytes += int64(len(valueJSON))
		if observationBytes > limits.MaxOutputBytes {
			return errors.New("typed observations exceed bounded receipt output limit")
		}
		var sha string
		for _, file := range files {
			if file.Path == spec.OutputPath {
				sha = file.ContentSHA
				break
			}
		}
		receipt.Observations = append(receipt.Observations, model.ExperimentObservation{Name: spec.Name, Type: spec.Type, Unit: spec.Unit, Value: valueJSON, SourcePath: spec.OutputPath, SourceSHA: sha})
	}
	return nil
}
