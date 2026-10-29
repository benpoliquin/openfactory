package gitrepo

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"openfactory/internal/artifacts"
	"openfactory/internal/model"
)

var _ model.IntakeRepositories = (*Manager)(nil)

// These are rejection bounds, never truncation targets. Callers must disclose the
// complete selected context before dispatch; intake has no implicit filesystem input.
const (
	intakeInputMaxBytes   = 64 << 20
	intakeTotalMaxBytes   = 64 << 20
	intakeMaxFiles        = 1024
	intakeControlMaxBytes = 1 << 20
)

type intakeNode struct{ Device, Inode uint64 }

type intakeReceipt struct {
	Workspace                                      model.IntakeWorkspace
	Project, EnrollmentSHA, Request, Step, Attempt string
	Directories                                    []intakeNode
}

type intakeSourceReceipt struct {
	Project, Source, Common, Base, ConfigHash string
	SourceNode, CommonNode                    intakeNode
}

// InputFiles intentionally contains only hashes. Neither private receipts nor the
// model-visible control file serialize another copy of the disclosed input bytes.
type intakeControl struct {
	Project    string            `json:"project"`
	RequestID  string            `json:"request_id"`
	StepID     string            `json:"step_id"`
	AttemptID  string            `json:"attempt_id"`
	Deadline   time.Time         `json:"deadline"`
	InputFiles map[string]string `json:"input_files"`
}

type intakeSeal struct{ Directories map[string]intakeNode }

func intakeIdentity(info os.FileInfo) (intakeNode, error) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return intakeNode{}, errors.New("intake directory is not a real directory")
	}
	return intakeNode{uint64(s.Dev), uint64(s.Ino)}, nil
}

func intakeEnrollment(p model.Project) string {
	data, _ := json.Marshal(p) // Project contains only JSON-serializable policy fields.
	return intakeBytesSHA(data)
}

func intakeBytesSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (m *Manager) intakeRecords() string                { return filepath.Join(m.cfg.StateDir, "intake-records") }
func (m *Manager) intakeReceiptName(path string) string { return digest(path) + ".json" }

// Private state and the configured volume are operator-owned trust boundaries.
// All descendants are opened component by component, with the discovery chain's
// descriptor/entry identity checks; there is no inherited discovery workspace/cache.
func intakeChain(base string, parts ...string) (*discoveryChain, error) {
	r, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	c := &discoveryChain{roots: []*os.Root{r}}
	for _, part := range parts {
		if err = c.descend(part); err != nil {
			c.close()
			return nil, err
		}
	}
	return c, nil
}

func intakePop(c *discoveryChain, depth int) {
	for len(c.roots) > depth {
		c.roots[len(c.roots)-1].Close()
		c.roots = c.roots[:len(c.roots)-1]
		c.names = c.names[:len(c.names)-1]
		c.infos = c.infos[:len(c.infos)-1]
	}
}

func intakeDirectories(c *discoveryChain) ([]intakeNode, error) {
	var result []intakeNode
	for _, info := range c.infos {
		node, err := intakeIdentity(info)
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, c.verify()
}

func intakeRead(ctx context.Context, c *discoveryChain, name string, limit int64, mode os.FileMode) ([]byte, error) {
	root := c.roots[len(c.roots)-1]
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !singleLinkDiscoveryInput(before) || before.Size() > limit || before.Mode().Perm() != mode {
		return nil, errors.New("invalid intake private/control file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, opened) || !singleLinkDiscoveryInput(opened) {
		return nil, errors.New("intake file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, limit+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	current, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit || int64(len(data)) != opened.Size() || !singleLinkDiscoveryInput(current) || !singleLinkDiscoveryInput(after) || !os.SameFile(opened, current) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || current.Mode() != opened.Mode() {
		return nil, errors.New("intake file changed while reading")
	}
	return data, c.verify()
}

// Publish atomically without replacing a concurrent winner or exposing a
// two-link crash boundary. Existing files are always verified before adoption.
func intakePublish(ctx context.Context, c *discoveryChain, name string, data []byte, mode os.FileMode, staging ...*discoveryChain) error {
	root := c.roots[len(c.roots)-1]
	stage := root
	temp := ".intake-" + rand.Text()
	if len(staging) != 0 {
		stage = staging[0].roots[len(staging[0].roots)-1]
		// Recover only this publication's controller-private staging slot.
		temp = ".intake-" + digest(root.Name()+"\x00"+name)
		if old, e := stage.Lstat(temp); e == nil {
			if singleLinkDiscoveryInput(old) {
				if e = stage.Remove(temp); e != nil {
					return e
				}
			} else {
				return errors.New("intake staging identity mismatch")
			}
		} else if !os.IsNotExist(e) {
			return e
		}
	}
	if info, err := root.Lstat(name); err == nil {
		if mode == 0400 {
			if info.Mode().Perm() != mode {
				return errors.New("invalid immutable intake input permissions")
			}
			return verifyDiscoveryInput(ctx, c, name, data)
		}
		old, err := intakeRead(ctx, c, name, int64(len(data)), mode)
		if err != nil {
			return err
		}
		if !bytes.Equal(old, data) {
			return errors.New("immutable intake file differs")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := stage.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	defer stage.Remove(temp)
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	current, err := stage.Lstat(temp)
	if err != nil {
		return err
	}
	if !singleLinkDiscoveryInput(opened) || !singleLinkDiscoveryInput(current) || !os.SameFile(opened, current) {
		return errors.New("intake publication raced")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = c.verify(); err != nil {
		return err
	}
	if err = artifacts.PublishStaged(ctx, stage, root, temp, name, f, opened, int64(len(data))); err != nil {
		if os.IsExist(err) {
			return intakePublish(ctx, c, name, data, mode, staging...)
		}
		return err
	}
	current, err = root.Lstat(name)
	if err != nil {
		return err
	}
	if !singleLinkDiscoveryInput(current) || !os.SameFile(opened, current) {
		return errors.New("intake publication changed")
	}
	if err = f.Close(); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return c.verify()
}

func intakeSave(ctx context.Context, c *discoveryChain, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > intakeControlMaxBytes {
		return errors.New("intake receipt exceeds byte bound")
	}
	return intakePublish(ctx, c, name, data, 0600)
}

func (m *Manager) intakePrivate() (*discoveryChain, error) {
	return intakeChain(m.cfg.StateDir, "intake-records")
}

type intakeDirectoryReceipt struct {
	Raw   []string
	Node  intakeNode
	Stage string `json:",omitempty"`
}

func intakeSyncDirectory(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// The staging inode is durably recorded before it is published at its final
// namespace entry. A lost final receipt can therefore be completed without ever
// enrolling an arbitrary directory that happened to occupy the desired path.
func intakeEnsureDirectory(ctx context.Context, private, c *discoveryChain, path, leaf string, raw []string) error {
	name := "namespace-" + digest(path) + ".json"
	root := c.roots[len(c.roots)-1]
	var receipt intakeDirectoryReceipt
	data, err := intakeRead(ctx, private, name, intakeControlMaxBytes, 0600)
	if err == nil {
		if err = json.Unmarshal(data, &receipt); err != nil {
			return err
		}
		if !reflect.DeepEqual(receipt.Raw, raw) {
			return errors.New("intake namespace prefix collision")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		intentName := "directory-intent-" + digest(path) + ".json"
		intent, readErr := intakeRead(ctx, private, intentName, intakeControlMaxBytes, 0600)
		if readErr == nil {
			var identity []string
			if err = json.Unmarshal(intent, &identity); err != nil {
				return err
			}
			if !reflect.DeepEqual(identity, raw) {
				return errors.New("intake namespace prefix collision")
			}
		} else if !os.IsNotExist(readErr) {
			return readErr
		} else {
			if _, err = root.Lstat(leaf); err == nil {
				return errors.New("unrecorded intake namespace")
			} else if !os.IsNotExist(err) {
				return err
			}
			if err = intakeSave(ctx, private, intentName, raw); err != nil {
				return err
			}
		}
		stageName := "directory-stage-" + digest(path) + ".json"
		stageData, stageErr := intakeRead(ctx, private, stageName, intakeControlMaxBytes, 0600)
		if stageErr == nil {
			if err = json.Unmarshal(stageData, &receipt); err != nil {
				return err
			}
			if !reflect.DeepEqual(receipt.Raw, raw) || !strings.HasPrefix(receipt.Stage, ".intake-stage-") || strings.ContainsAny(receipt.Stage, "/\\") {
				return errors.New("invalid intake directory intent")
			}
		} else if !os.IsNotExist(stageErr) {
			return stageErr
		} else {
			if _, err = root.Lstat(leaf); err == nil {
				return errors.New("unrecorded intake namespace")
			} else if !os.IsNotExist(err) {
				return err
			}
			stage := ".intake-stage-" + rand.Text()
			if err = root.Mkdir(stage, 0700); err != nil {
				return err
			}
			// If cancellation occurs before this receipt is durable, only an
			// unpublished random staging directory exists, not a blocked namespace.
			published := false
			defer func() {
				if !published {
					root.Remove(stage)
				}
			}()
			if err = intakeSyncDirectory(root); err != nil {
				return err
			}
			info, e := root.Lstat(stage)
			if e != nil {
				return e
			}
			node, e := intakeIdentity(info)
			if e != nil {
				return e
			}
			receipt = intakeDirectoryReceipt{Raw: raw, Node: node, Stage: stage}
			if err = intakeSave(ctx, private, stageName, receipt); err != nil {
				return err
			}
			published = true
		}
		if info, e := root.Lstat(leaf); e == nil {
			node, e := intakeIdentity(info)
			if e != nil || node != receipt.Node {
				return errors.New("intake directory intent inode mismatch")
			}
		} else if !os.IsNotExist(e) {
			return e
		} else {
			info, e := root.Lstat(receipt.Stage)
			if e != nil {
				return e
			}
			node, e := intakeIdentity(info)
			if e != nil || node != receipt.Node {
				return errors.New("intake staging directory replaced")
			}
			if err = c.verify(); err != nil {
				return err
			}
			if err = root.Rename(receipt.Stage, leaf); err != nil {
				return err
			}
			if err = intakeSyncDirectory(root); err != nil {
				return err
			}
		}
		if err = intakeSave(ctx, private, name, intakeDirectoryReceipt{Raw: raw, Node: receipt.Node}); err != nil {
			return err
		}
	}
	if err = c.descend(leaf); err != nil {
		return err
	}
	node, err := intakeIdentity(c.infos[len(c.infos)-1])
	if err != nil {
		return err
	}
	if node != receipt.Node {
		return errors.New("intake namespace directory replaced")
	}
	return c.verify()
}

func (m *Manager) PrepareIntake(ctx context.Context, name string, p model.Project, attempt, request, step string) (model.IntakeWorkspace, error) {
	defer m.lock("intake-prepare")()
	var zero model.IntakeWorkspace
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := m.enrolled(name, p); err != nil {
		return zero, err
	}
	if name == "" || attempt == "" || request == "" || step == "" {
		return zero, errors.New("intake identity required")
	}
	if err := os.MkdirAll(m.cfg.WorkspaceDir, 0700); err != nil {
		return zero, err
	}
	root, err := filepath.EvalSymlinks(m.cfg.WorkspaceDir)
	if err != nil {
		return zero, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return zero, err
	}
	path := filepath.Join(root, "intake", digest(name)[:32], digest(request)[:32], digest(step)[:32], digest(attempt)[:32])
	// OMP encodes an absolute cwd as -- + cwd[1:] with flattened separators + --.
	if len(path)+3 > 255 {
		return zero, &model.ProgramError{Code: "prerequisite_missing", Message: "intake cwd exceeds OMP's 255-byte flattened session directory limit; shorten the operator WorkspaceDir"}
	}
	if err = os.MkdirAll(m.cfg.StateDir, 0700); err != nil {
		return zero, err
	}
	state, err := filepath.EvalSymlinks(m.cfg.StateDir)
	if err != nil {
		return zero, err
	}
	state, err = filepath.Abs(state)
	if err != nil {
		return zero, err
	}
	if state == root || strings.HasPrefix(state, root+string(filepath.Separator)) {
		return zero, errors.New("intake state must be private")
	}
	private, err := intakeChain(state)
	if err != nil {
		return zero, err
	}
	defer private.close()
	if err = private.roots[0].Mkdir("intake-records", 0700); err != nil && !os.IsExist(err) {
		return zero, err
	}
	if err = private.descend("intake-records"); err != nil {
		return zero, err
	}
	data, err := intakeRead(ctx, private, m.intakeReceiptName(path), intakeControlMaxBytes, 0600)
	if err == nil {
		var receipt intakeReceipt
		if err = json.Unmarshal(data, &receipt); err != nil {
			return zero, err
		}
		if receipt.Project != name || receipt.EnrollmentSHA != intakeEnrollment(p) || receipt.Attempt != attempt || receipt.Request != request || receipt.Step != step || receipt.Workspace.Path != path {
			return zero, errors.New("intake identity or enrollment changed")
		}
		c, _, err := m.openIntake(ctx, receipt.Workspace)
		if err != nil {
			return zero, err
		}
		defer c.close()
		if err = intakeSave(ctx, private, "complete-"+digest(path)+".json", receipt); err != nil {
			return zero, err
		}
		return receipt.Workspace, c.verify()
	}
	if !os.IsNotExist(err) {
		return zero, err
	}
	completeName := "complete-" + digest(path) + ".json"
	if _, e := intakeRead(ctx, private, completeName, intakeControlMaxBytes, 0600); e == nil {
		return zero, errors.New("intake workspace receipt missing")
	} else if !os.IsNotExist(e) {
		return zero, e
	}
	if err = intakeSave(ctx, private, "workspace-intent-"+digest(path)+".json", []string{name, intakeEnrollment(p), request, step, attempt}); err != nil {
		return zero, err
	}
	source, base, err := m.prepareIntakeSource(ctx, private, state, name, p)
	if err != nil {
		return zero, err
	}
	shared, err := intakeChain(root)
	if err != nil {
		return zero, err
	}
	defer shared.close()
	parts := []string{"intake", digest(name)[:32], digest(request)[:32], digest(step)[:32], digest(attempt)[:32]}
	rawParts := []string{"intake", name, request, step, attempt}
	prefix := root
	for i, part := range parts {
		prefix = filepath.Join(prefix, part)
		if err = intakeEnsureDirectory(ctx, private, shared, prefix, part, rawParts[:i+1]); err != nil {
			return zero, err
		}
	}
	nodes, err := intakeDirectories(shared)
	if err != nil {
		return zero, err
	}
	w := model.IntakeWorkspace{Path: path, ProjectRoot: source, BaseSHA: base}
	receipt := intakeReceipt{Workspace: w, Project: name, EnrollmentSHA: intakeEnrollment(p), Request: request, Step: step, Attempt: attempt, Directories: nodes}
	if err = intakeSave(ctx, private, m.intakeReceiptName(path), receipt); err != nil {
		return zero, err
	}
	if err = intakeSave(ctx, private, completeName, receipt); err != nil {
		return zero, err
	}
	return w, nil
}

func (m *Manager) prepareIntakeSource(ctx context.Context, private *discoveryChain, state, name string, p model.Project) (string, string, error) {
	source := filepath.Join(state, "intake-sources", digest(name))
	common := filepath.Join(state, "intake-git", digest(name)+".git")
	filename := "source-" + digest(name) + ".json"
	data, err := intakeRead(ctx, private, filename, intakeControlMaxBytes, 0600)
	var receipt intakeSourceReceipt
	if err == nil {
		if err = json.Unmarshal(data, &receipt); err != nil {
			return "", "", err
		}
		if receipt.Project != name || receipt.Source != source || receipt.Common != common || !shaPattern.MatchString(receipt.Base) {
			return "", "", errors.New("intake source identity changed")
		}
	} else if !os.IsNotExist(err) {
		return "", "", err
	} else {
		if _, e := intakeRead(ctx, private, "source-complete-"+digest(name)+".json", intakeControlMaxBytes, 0600); e == nil {
			return "", "", errors.New("intake source receipt missing")
		} else if !os.IsNotExist(e) {
			return "", "", e
		}
		if err = intakeSave(ctx, private, "source-intent-"+digest(name)+".json", []string{name, source, common}); err != nil {
			return "", "", err
		}
		c, e := intakeChain(state)
		if e != nil {
			return "", "", e
		}
		defer c.close()
		for _, parent := range []string{"intake-sources", "intake-git"} {
			if e = intakeEnsureDirectory(ctx, private, c, filepath.Join(state, parent), parent, []string{parent}); e != nil {
				return "", "", e
			}
			leaf := digest(name)
			if parent == "intake-git" {
				leaf += ".git"
			}
			if e = intakeEnsureDirectory(ctx, private, c, filepath.Join(state, parent, leaf), leaf, []string{parent, name}); e != nil {
				return "", "", e
			}
			intakePop(c, 1)
		}
		if _, err = m.git(ctx, "", "init", "--bare", common); err != nil {
			return "", "", err
		}
		tree, e := m.git(ctx, common, "mktree")
		if e != nil {
			return "", "", e
		}
		base, e := m.git(ctx, common, "-c", "user.name=OpenFactory", "-c", "user.email=openfactory@localhost", "commit-tree", strings.TrimSpace(tree), "-m", "Empty intake baseline")
		if e != nil {
			return "", "", e
		}
		receipt = intakeSourceReceipt{Project: name, Source: source, Common: common, Base: strings.TrimSpace(base)}
		if !shaPattern.MatchString(receipt.Base) {
			return "", "", errors.New("invalid intake baseline")
		}
		if _, err = m.git(ctx, common, "update-ref", "refs/intake/baseline", receipt.Base); err != nil {
			return "", "", err
		}
		if receipt.ConfigHash, err = configHash(common); err != nil {
			return "", "", err
		}
		for path, node := range map[string]*intakeNode{source: &receipt.SourceNode, common: &receipt.CommonNode} {
			info, e := os.Lstat(path)
			if e != nil {
				return "", "", e
			}
			if *node, e = intakeIdentity(info); e != nil {
				return "", "", e
			}
		}
		if err = intakeSave(ctx, private, filename, receipt); err != nil {
			return "", "", err
		}
	}
	for _, item := range []struct {
		parent, leaf string
		node         intakeNode
		empty        bool
	}{{"intake-sources", digest(name), receipt.SourceNode, true}, {"intake-git", digest(name) + ".git", receipt.CommonNode, false}} {
		c, e := intakeChain(state, item.parent, item.leaf)
		if e != nil {
			return "", "", e
		}
		e = func() error {
			defer c.close()
			node, e := intakeIdentity(c.infos[len(c.infos)-1])
			if e != nil {
				return e
			}
			if node != item.node {
				return errors.New("intake source directory replaced")
			}
			if item.empty {
				entries, e := intakeEntries(c)
				if e != nil {
					return e
				}
				if len(entries) != 0 {
					return errors.New("intake source is not empty")
				}
			}
			return c.verify()
		}()
		if e != nil {
			return "", "", e
		}
	}
	if err = verifyConfig(common, receipt.ConfigHash); err != nil {
		return "", "", err
	}
	if err = intakeSave(ctx, private, "source-complete-"+digest(name)+".json", receipt); err != nil {
		return "", "", err
	}
	return source, receipt.Base, private.verify()
}

func (m *Manager) openIntake(ctx context.Context, w model.IntakeWorkspace) (*discoveryChain, intakeReceipt, error) {
	return m.openIntakeReceipt(ctx, w, true)
}

// ReopenIntake recovers execution identity, not permission for a new dispatch.
// Only a complete private receipt can authorize an existing backend lookup/stop.
func (m *Manager) ReopenIntake(ctx context.Context, path, project, attempt, request, step string) (model.IntakeWorkspace, error) {
	private, err := m.intakePrivate()
	if err != nil {
		return model.IntakeWorkspace{}, err
	}
	defer private.close()
	data, err := intakeRead(ctx, private, m.intakeReceiptName(path), intakeControlMaxBytes, 0600)
	if err != nil {
		return model.IntakeWorkspace{}, err
	}
	var receipt intakeReceipt
	if err = json.Unmarshal(data, &receipt); err != nil {
		return model.IntakeWorkspace{}, err
	}
	if receipt.Workspace.Path != path || receipt.Project != project || receipt.Attempt != attempt || receipt.Request != request || receipt.Step != step || !shaPattern.MatchString(receipt.EnrollmentSHA) {
		return model.IntakeWorkspace{}, errors.New("intake persisted identity mismatch")
	}
	c, _, err := m.openIntakeReceipt(ctx, receipt.Workspace, false)
	if err != nil {
		return model.IntakeWorkspace{}, err
	}
	defer c.close()
	return receipt.Workspace, c.verify()
}

func (m *Manager) openIntakeReceipt(ctx context.Context, w model.IntakeWorkspace, currentEnrollment bool) (*discoveryChain, intakeReceipt, error) {
	var receipt intakeReceipt
	private, err := m.intakePrivate()
	if err != nil {
		return nil, receipt, err
	}
	defer private.close()
	data, err := intakeRead(ctx, private, m.intakeReceiptName(w.Path), intakeControlMaxBytes, 0600)
	if err != nil {
		return nil, receipt, err
	}
	if err = json.Unmarshal(data, &receipt); err != nil {
		return nil, receipt, err
	}
	p, ok := m.cfg.Projects[receipt.Project]
	if receipt.Workspace != w || (currentEnrollment && (!ok || intakeEnrollment(p) != receipt.EnrollmentSHA)) {
		return nil, receipt, errors.New("intake workspace receipt mismatch")
	}
	root, relative, err := m.sharedRoot(w.Path)
	if err != nil {
		return nil, receipt, err
	}
	c := &discoveryChain{roots: []*os.Root{root}}
	parts := strings.Split(relative, string(filepath.Separator))
	want := []string{"intake", digest(receipt.Project)[:32], digest(receipt.Request)[:32], digest(receipt.Step)[:32], digest(receipt.Attempt)[:32]}
	if !reflect.DeepEqual(parts, want) {
		c.close()
		return nil, receipt, errors.New("intake namespace receipt mismatch")
	}
	for _, part := range parts {
		if err = c.descend(part); err != nil {
			c.close()
			return nil, receipt, err
		}
	}
	nodes, err := intakeDirectories(c)
	if err != nil || !reflect.DeepEqual(nodes, receipt.Directories) {
		c.close()
		return nil, receipt, errors.New("intake workspace directory replaced")
	}
	state, err := filepath.EvalSymlinks(m.cfg.StateDir)
	if err == nil {
		state, err = filepath.Abs(state)
	}
	if err != nil {
		c.close()
		return nil, receipt, err
	}
	source, base, err := m.prepareIntakeSource(ctx, private, state, receipt.Project, p)
	if err != nil || source != w.ProjectRoot || base != w.BaseSHA {
		c.close()
		return nil, receipt, errors.New("intake source receipt mismatch")
	}
	return c, receipt, nil
}

func intakeMember(name string) error {
	if err := discoveryMember(name); err != nil {
		return err
	}
	if !strings.HasPrefix(name, "inputs/") || !utf8.ValidString(name) {
		return errors.New("intake input must be an inputs/ file")
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") || strings.IndexFunc(part, unicode.IsControl) >= 0 {
			return errors.New("intake input contains hidden or control component")
		}
	}
	return nil
}

func intakeSessionControl(s model.IntakeSession) ([]byte, []string, error) {
	if s.Deadline.IsZero() {
		return nil, nil, errors.New("intake deadline required")
	}
	if len(s.InputFiles) > intakeMaxFiles {
		return nil, nil, errors.New("intake input file count exceeds 1024")
	}
	control := intakeControl{Project: s.Project, RequestID: s.RequestID, StepID: s.StepID, AttemptID: s.AttemptID, Deadline: s.Deadline, InputFiles: make(map[string]string, len(s.InputFiles))}
	var total int64
	var names []string
	for name, data := range s.InputFiles {
		if err := intakeMember(name); err != nil {
			return nil, nil, err
		}
		if len(data) > intakeInputMaxBytes {
			return nil, nil, errors.New("intake input exceeds 64 MiB")
		}
		total += int64(len(data))
		if total > intakeTotalMaxBytes {
			return nil, nil, errors.New("intake complete context exceeds 64 MiB")
		}
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return nil, nil, errors.New("intake inputs must be UTF-8 text without NUL")
		}
		control.InputFiles[name] = intakeBytesSHA(data)
		names = append(names, name)
	}
	for name := range s.InputFiles {
		for parent := name; strings.Contains(parent, "/"); {
			parent = parent[:strings.LastIndexByte(parent, '/')]
			if _, ok := s.InputFiles[parent]; ok {
				return nil, nil, errors.New("intake input path is also a parent")
			}
		}
	}
	sort.Strings(names)
	data, err := json.Marshal(control)
	if err == nil && len(data) > intakeControlMaxBytes {
		err = errors.New("intake control exceeds 1 MiB")
	}
	return data, names, err
}

func (m *Manager) ConfigureIntakeSession(ctx context.Context, w model.IntakeWorkspace, s model.IntakeSession) error {
	defer m.lock("intake:" + w.Path)()
	if err := ctx.Err(); err != nil {
		return err
	}
	data, names, err := intakeSessionControl(s)
	if err != nil {
		return err
	}
	var control intakeControl
	if err = json.Unmarshal(data, &control); err != nil {
		return err
	}
	c, receipt, err := m.openIntake(ctx, w)
	if err != nil {
		return err
	}
	defer c.close()
	if s.Project != receipt.Project || s.RequestID != receipt.Request || s.StepID != receipt.Step || s.AttemptID != receipt.Attempt {
		return errors.New("intake session identity mismatch")
	}
	private, err := m.intakePrivate()
	if err != nil {
		return err
	}
	defer private.close()
	// Persist immutable intent first: an interrupted setup cannot enroll different
	// bytes or a fresh deadline under the same attempt on its next invocation.
	if err = intakePublish(ctx, private, "session-"+digest(w.Path)+".json", data, 0600); err != nil {
		return err
	}
	sealName := "seal-" + digest(w.Path) + ".json"
	var oldSeal intakeSeal
	sealData, sealErr := intakeRead(ctx, private, sealName, intakeControlMaxBytes, 0600)
	if sealErr == nil {
		if err = json.Unmarshal(sealData, &oldSeal); err != nil {
			return err
		}
		if err = intakeVerifyControl(ctx, c, data); err != nil {
			return err
		}
		if _, dirs, err := intakeTree(ctx, c, control.InputFiles); err != nil {
			return err
		} else if !reflect.DeepEqual(dirs, oldSeal.Directories) {
			return errors.New("intake input directory replaced")
		}
		return c.verify()
	} else if !os.IsNotExist(sealErr) {
		return sealErr
	}
	// Staging stays on the workspace filesystem, outside the walked input tree.
	workspaceRoot := c.roots[len(c.roots)-1]
	if err = workspaceRoot.Mkdir(".openfactory", 0700); err != nil && !os.IsExist(err) {
		return err
	}
	if err = c.descend(".openfactory"); err != nil {
		return err
	}
	stage, err := intakeChain(c.roots[len(c.roots)-1].Name())
	intakePop(c, len(c.roots)-1)
	if err != nil {
		return err
	}
	defer stage.close()
	depth := len(c.roots)
	for _, name := range names {
		parts := strings.Split(name, "/")
		for _, part := range parts[:len(parts)-1] {
			root := c.roots[len(c.roots)-1]
			if err = root.Mkdir(part, 0700); err != nil && !os.IsExist(err) {
				return err
			}
			if err = c.descend(part); err != nil {
				return err
			}
		}
		if err = intakePublish(ctx, c, parts[len(parts)-1], s.InputFiles[name], 0400, stage); err != nil {
			return fmt.Errorf("materialize intake input: %w", err)
		}
		intakePop(c, depth)
	}
	root := c.roots[len(c.roots)-1]
	if err = root.Mkdir(".openfactory", 0700); err != nil && !os.IsExist(err) {
		return err
	}
	if err = c.descend(".openfactory"); err != nil {
		return err
	}
	if err = intakePublish(ctx, c, "intake-session.json", data, 0600); err != nil {
		return err
	}
	intakePop(c, depth)
	_, dirs, err := intakeTree(ctx, c, control.InputFiles)
	if err != nil {
		return err
	}
	if err = intakeSave(ctx, private, sealName, intakeSeal{Directories: dirs}); err != nil {
		return err
	}
	return c.verify()
}

func intakeVerifyControl(ctx context.Context, c *discoveryChain, data []byte) error {
	depth := len(c.roots)
	if err := c.descend(".openfactory"); err != nil {
		return err
	}
	defer intakePop(c, depth)
	current, err := intakeRead(ctx, c, "intake-session.json", intakeControlMaxBytes, 0600)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, data) {
		return errors.New("immutable intake control differs")
	}
	return nil
}

func intakeEntries(c *discoveryChain) ([]os.DirEntry, error) {
	f, err := c.roots[len(c.roots)-1].Open(".")
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, c.verify()
}

// Controller-private publication slots are excluded; control, input bytes,
// permissions and directory identities participate in the fingerprint.
func intakeTree(ctx context.Context, c *discoveryChain, expected map[string]string) (string, map[string]intakeNode, error) {
	h := sha256.New()
	enc := json.NewEncoder(h)
	dirs := map[string]intakeNode{}
	var total int64
	count := 0
	files := 0
	buffer := make([]byte, 32<<10)
	var walk func(string) error
	walk = func(prefix string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := intakeEntries(c)
		if err != nil {
			return err
		}
		root := c.roots[len(c.roots)-1]
		for _, entry := range entries {
			name := entry.Name()
			path := name
			if prefix != "" {
				path = prefix + "/" + name
			}
			if prefix == ".openfactory" && strings.HasPrefix(name, ".intake-") {
				continue
			}
			count++
			if count > 4096 {
				return errors.New("intake fingerprint member bound exceeded")
			}
			info, err := root.Lstat(name)
			if err != nil {
				return err
			}
			if path != "inputs" && path != ".openfactory" && path != ".openfactory/intake-session.json" {
				if err = intakeMember(path); err != nil {
					return err
				}
			}
			if err = enc.Encode(struct {
				Path string
				Mode uint32
			}{path, uint32(info.Mode())}); err != nil {
				return err
			}
			if info.IsDir() {
				if path == ".openfactory/intake-session.json" {
					return errors.New("intake control is a directory")
				}
				node, err := intakeIdentity(info)
				if err != nil {
					return err
				}
				dirs[path] = node
				if err = enc.Encode(node); err != nil {
					return err
				}
				depth := len(c.roots)
				if err = c.descend(name); err != nil {
					return err
				}
				err = walk(path)
				intakePop(c, depth)
				if err != nil {
					return err
				}
				continue
			}
			if !singleLinkDiscoveryInput(info) {
				return errors.New("intake member must be a single-link regular file")
			}
			limit := int64(intakeInputMaxBytes)
			if path == ".openfactory/intake-session.json" {
				limit = intakeControlMaxBytes
				if info.Mode().Perm() != 0600 {
					return errors.New("invalid intake control permissions")
				}
			} else if path == "inputs" || path == ".openfactory" || info.Mode().Perm()&0222 != 0 {
				return errors.New("invalid intake input permissions")
			}
			if info.Size() > limit {
				return errors.New("intake fingerprint member exceeds byte bound")
			}
			f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			err = func() error {
				defer f.Close()
				opened, err := f.Stat()
				if err != nil {
					return err
				}
				if !singleLinkDiscoveryInput(opened) || !os.SameFile(info, opened) {
					return errors.New("intake fingerprint member raced")
				}
				content := sha256.New()
				n, err := io.CopyBuffer(content, io.LimitReader(contextReader{ctx, f}, limit+1), buffer)
				if err != nil {
					return err
				}
				if path != ".openfactory/intake-session.json" {
					total += n
				}
				if n > limit || total > intakeTotalMaxBytes {
					return errors.New("intake fingerprint exceeds complete context bound")
				}
				after, err := f.Stat()
				if err != nil {
					return err
				}
				current, err := root.Lstat(name)
				if err != nil {
					return err
				}
				if !singleLinkDiscoveryInput(after) || !singleLinkDiscoveryInput(current) || !os.SameFile(opened, current) || opened.Mode() != current.Mode() || n != opened.Size() || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
					return errors.New("intake fingerprint member changed")
				}
				sum := hex.EncodeToString(content.Sum(nil))
				if expected != nil && path != ".openfactory/intake-session.json" {
					if want, ok := expected[path]; !ok || want != sum {
						return errors.New("intake input differs from disclosed immutable context")
					}
					files++
				}
				return enc.Encode(sum)
			}()
			if err != nil {
				return err
			}
		}
		final, err := intakeEntries(c)
		if err != nil {
			return err
		}
		if len(entries) != len(final) {
			return errors.New("intake fingerprint directory changed")
		}
		for i := range entries {
			if entries[i].Name() != final[i].Name() {
				return errors.New("intake fingerprint directory changed")
			}
		}
		return c.verify()
	}
	if err := walk(""); err != nil {
		return "", nil, err
	}
	if expected != nil && files != len(expected) {
		return "", nil, errors.New("intake immutable input missing")
	}
	return hex.EncodeToString(h.Sum(nil)), dirs, nil
}

func (m *Manager) FingerprintIntake(ctx context.Context, w model.IntakeWorkspace) (string, error) {
	defer m.lock("intake:" + w.Path)()
	c, _, err := m.openIntake(ctx, w)
	if err != nil {
		return "", err
	}
	defer c.close()
	fingerprint, dirs, err := intakeTree(ctx, c, nil)
	if err != nil {
		return "", err
	}
	private, err := m.intakePrivate()
	if err != nil {
		return "", err
	}
	defer private.close()
	data, err := intakeRead(ctx, private, "seal-"+digest(w.Path)+".json", intakeControlMaxBytes, 0600)
	if err == nil {
		var seal intakeSeal
		if err = json.Unmarshal(data, &seal); err != nil {
			return "", err
		}
		if !reflect.DeepEqual(dirs, seal.Directories) {
			return "", errors.New("intake input directory replaced")
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return fingerprint, c.verify()
}
