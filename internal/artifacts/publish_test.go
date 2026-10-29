//go:build linux || darwin

package artifacts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func assertPublishedSingleLink(t *testing.T, root, sha string, data []byte) {
	t.Helper()
	info, err := os.Lstat(filepath.Join(root, sha))
	if err != nil {
		t.Fatal(err)
	}
	if !streamRegular(info) {
		t.Fatalf("published inode is not regular and single-link: %v", info)
	}
	var received bytes.Buffer
	ref, err := New(root).ReadTo(context.Background(), sha, int64(len(data)), int64(len(data)), &received)
	if err != nil || ref.SHA != sha || ref.Bytes != int64(len(data)) || !bytes.Equal(received.Bytes(), data) {
		t.Fatalf("published object is not consumable: %+v %v", ref, err)
	}
}

// Exit immediately on either side of the actual publication syscall, before
// application cleanup, validation, directory sync, or closing the staging fd.
// A hardlink-based implementation leaves the after-syscall case unconsumable.
func TestCASPublicationCrashBoundary(t *testing.T) {
	const rootEnv = "OPENFACTORY_TEST_CAS_CRASH_ROOT"
	const targetEnv = "OPENFACTORY_TEST_CAS_CRASH_TARGET"
	const phaseEnv = "OPENFACTORY_TEST_CAS_CRASH_PHASE"
	data := []byte("complete single-link object across process exit")
	sha := digest(data)
	const temp = ".tmp-crash-boundary"
	if rootPath := os.Getenv(rootEnv); rootPath != "" {
		root, err := openDir(rootPath, false)
		if err != nil {
			t.Fatal(err)
		}
		file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = file.Write(data); err != nil {
			t.Fatal(err)
		}
		if err = file.Sync(); err != nil {
			t.Fatal(err)
		}
		dir, _, err := openRootDirectory(root)
		if err != nil {
			t.Fatal(err)
		}
		targetRoot, err := openDir(os.Getenv(targetEnv), false)
		if err != nil {
			t.Fatal(err)
		}
		targetDir, _, err := openRootDirectory(targetRoot)
		if err != nil {
			t.Fatal(err)
		}
		if os.Getenv(phaseEnv) == "after" {
			if err = renameNoReplace(dir, targetDir, temp, sha); err != nil {
				t.Fatal(err)
			}
		}
		os.Exit(0)
	}
	for _, variant := range []string{"before", "after", "cross-before", "cross-after"} {
		t.Run(variant, func(t *testing.T) {
			phase := strings.TrimPrefix(variant, "cross-")
			root := t.TempDir()
			staging := root
			if strings.HasPrefix(variant, "cross-") {
				staging = filepath.Join(root, "staging")
				if err := os.Mkdir(staging, 0700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestCASPublicationCrashBoundary$")
			cmd.Env = append(os.Environ(), rootEnv+"="+staging, targetEnv+"="+root, phaseEnv+"="+phase)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("publication subprocess: %v\n%s", err, out)
			}
			if phase == "before" {
				if _, err := os.Lstat(filepath.Join(root, sha)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unpublished object exists: %v", err)
				}
				info, err := os.Lstat(filepath.Join(staging, temp))
				if err != nil || !streamRegular(info) {
					t.Fatalf("staging crash state lost single-link inode: %v", err)
				}
				if err = os.Remove(filepath.Join(staging, temp)); err != nil {
					t.Fatal(err)
				}
			} else {
				// Inspect before any writer cleanup or replay can repair the state.
				assertPublishedSingleLink(t, root, sha, data)
				if _, err := os.Lstat(filepath.Join(staging, temp)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("atomic move retained a second name: %v", err)
				}
			}
			s := New(root)
			if _, err := s.Put(context.Background(), data); err != nil {
				t.Fatal(err)
			}
			if _, err := s.PutReader(context.Background(), bytes.NewReader(data), sha, int64(len(data)), int64(len(data))); err != nil {
				t.Fatal(err)
			}
			assertPublishedSingleLink(t, root, sha, data)
			streamAssertNoTemporary(t, root)
			streamAssertNoTemporary(t, staging)
		})
	}
}

func TestAtomicWriteNeverReplacesDestination(t *testing.T) {
	data := []byte("expected immutable content")
	sha := digest(data)
	for _, kind := range []string{"same", "corrupt", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			path := t.TempDir()
			destination := filepath.Join(path, sha)
			outside := filepath.Join(t.TempDir(), "winner")
			winner := data
			if kind == "corrupt" {
				winner = bytes.Repeat([]byte("x"), len(data))
			}
			if err := os.WriteFile(outside, winner, 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(outside, destination)
			case "hardlink":
				err = os.Link(outside, destination)
			default:
				err = os.WriteFile(destination, winner, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(destination)
			if err != nil {
				t.Fatal(err)
			}
			root, err := openDir(path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err = atomicWrite(context.Background(), root, sha, data); !errors.Is(err, os.ErrExist) {
				t.Fatalf("no-replace publication returned %v", err)
			}
			after, err := os.Lstat(destination)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("publication replaced the winning inode")
			}
			for _, name := range []string{outside, destination} {
				got, err := os.ReadFile(name)
				if err != nil || !bytes.Equal(got, winner) {
					t.Fatalf("publication modified winner %s: %v", name, err)
				}
			}
			_, err = New(path).Put(context.Background(), data)
			if kind == "same" && err != nil {
				t.Fatalf("Put did not adopt matching winner: %v", err)
			}
			if kind != "same" && err == nil {
				t.Fatal("Put accepted corrupt or linked winner")
			}
			streamAssertNoTemporary(t, path)
		})
	}
}

func TestPublicationUsesPinnedRootAfterDirectoryReplacement(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "cas")
	root, err := openDir(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	moved := filepath.Join(parent, "moved")
	if err = os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("publish only within the pinned original root")
	sha := digest(data)
	sentinel := []byte("replacement directory must remain untouched")
	if err = os.WriteFile(filepath.Join(path, sha), sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	if err = atomicWrite(context.Background(), root, sha, data); err != nil {
		t.Fatal(err)
	}
	assertPublishedSingleLink(t, moved, sha, data)
	got, err := os.ReadFile(filepath.Join(path, sha))
	if err != nil || !bytes.Equal(got, sentinel) {
		t.Fatal("publication escaped pinned root into replacement pathname")
	}
	streamAssertNoTemporary(t, moved)
	streamAssertNoTemporary(t, path)
}

func TestStreamPublicationRejectsDestinationRaces(t *testing.T) {
	data := []byte("bytes with a competing destination")
	sha := digest(data)
	for _, kind := range []string{"same", "corrupt", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			path := t.TempDir()
			destination := filepath.Join(path, sha)
			outside := filepath.Join(t.TempDir(), "winner")
			winner := data
			if kind == "corrupt" {
				winner = bytes.Repeat([]byte("x"), len(data))
			}
			if err := os.WriteFile(outside, winner, 0600); err != nil {
				t.Fatal(err)
			}
			var before os.FileInfo
			reader := &streamHookReader{reader: bytes.NewReader(data), hook: func() {
				var err error
				switch kind {
				case "symlink":
					err = os.Symlink(outside, destination)
				case "hardlink":
					err = os.Link(outside, destination)
				default:
					err = os.WriteFile(destination, winner, 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				before, err = os.Lstat(destination)
				if err != nil {
					t.Fatal(err)
				}
			}}
			_, err := New(path).PutReader(context.Background(), reader, sha, int64(len(data)), int64(len(data)))
			if kind == "same" && err != nil {
				t.Fatalf("matching concurrent winner rejected: %v", err)
			}
			if kind != "same" && err == nil {
				t.Fatal("corrupt or linked concurrent winner accepted")
			}
			after, err := os.Lstat(destination)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("race winner was replaced")
			}
			got, err := os.ReadFile(outside)
			if err != nil || !bytes.Equal(got, winner) {
				t.Fatal("race modified external target")
			}
			got, err = os.ReadFile(destination)
			if err != nil || !bytes.Equal(got, winner) {
				t.Fatal("race modified destination bytes")
			}
			streamAssertNoTemporary(t, path)
		})
	}
}

func TestPinnedConsumerSurvivesCompetingPublication(t *testing.T) {
	path := t.TempDir()
	s := New(path)
	ctx := context.Background()
	data := bytes.Repeat([]byte("reader pins the winning inode\n"), 4096)
	sha := digest(data)
	if _, err := s.Put(ctx, data); err != nil {
		t.Fatal(err)
	}
	root, err := openDir(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	pinned := make(chan struct{})
	resume := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		writer := &streamHookWriter{hook: func() {
			close(pinned)
			<-resume
		}}
		_, err := s.ReadTo(ctx, sha, int64(len(data)), int64(len(data)), writer)
		result <- err
	}()
	select {
	case <-pinned:
	case err := <-result:
		t.Fatalf("consumer failed before pinning: %v", err)
	}
	// The consumer cannot finish until both actual writer paths compete with
	// its pinned inode. A replacing rename would invalidate that read.
	writeErr := atomicWrite(ctx, root, sha, data)
	_, streamErr := s.PutReader(ctx, bytes.NewReader(data), sha, int64(len(data)), int64(len(data)))
	close(resume)
	readErr := <-result
	if !errors.Is(writeErr, os.ErrExist) {
		t.Fatalf("competing atomic write replaced consumer inode: %v", writeErr)
	}
	if streamErr != nil {
		t.Fatalf("stream writer rejected matching winner: %v", streamErr)
	}
	if readErr != nil {
		t.Fatalf("publication invalidated pinned consumer: %v", readErr)
	}
	assertPublishedSingleLink(t, path, sha, data)
	streamAssertNoTemporary(t, path)
}

func TestConcurrentCASWritersAndConsumer(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	data := bytes.Repeat([]byte("concurrent bounded publication\n"), 4096)
	sha := digest(data)
	const writers = 8
	staged := make(chan struct{}, writers)
	release := make(chan struct{})
	results := make(chan error, writers+1)
	for range writers {
		go func() {
			reader := &streamHookReader{reader: bytes.NewReader(data), hook: func() {
				staged <- struct{}{}
				<-release
			}}
			ref, err := s.PutReader(context.Background(), reader, sha, int64(len(data)), int64(len(data)))
			if err == nil && (ref.SHA != sha || ref.Bytes != int64(len(data))) {
				err = fmt.Errorf("stream writer returned wrong reference: %+v", ref)
			}
			results <- err
		}()
	}
	for range writers {
		select {
		case <-staged:
		case err := <-results:
			close(release)
			t.Fatalf("writer failed before staging barrier: %v", err)
		}
	}
	go func() {
		<-release
		ref, err := s.Put(context.Background(), data)
		if err == nil && (ref.SHA != sha || ref.Bytes != int64(len(data))) {
			err = fmt.Errorf("byte writer returned wrong reference: %+v", ref)
		}
		results <- err
	}()
	close(release)
	for range writers + 1 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent publication failed: %v", err)
		}
		// Other publishers may still have their staging descriptors open. Every
		// observed winning entry must already be a consumable single-link file.
		if _, err := s.ReadTo(context.Background(), sha, int64(len(data)), int64(len(data)), io.Discard); err != nil {
			t.Fatalf("consumer raced with an unconsumable publication: %v", err)
		}
	}
	assertPublishedSingleLink(t, root, sha, data)
	streamAssertNoTemporary(t, root)
}

func TestPublicationRejectsHardlinkedStaging(t *testing.T) {
	root := t.TempDir()
	data := []byte("a staging inode must have only one name")
	sha := digest(data)
	alias := filepath.Join(t.TempDir(), "staging-alias")
	reader := &streamHookReader{reader: bytes.NewReader(data), hook: func() {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".tmp-") {
				if err = os.Link(filepath.Join(root, entry.Name()), alias); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
		t.Fatal("no staging file found")
	}}
	if _, err := New(root).PutReader(context.Background(), reader, sha, int64(len(data)), int64(len(data))); err == nil {
		t.Fatal("hardlinked staging inode was published")
	}
	if _, err := os.Lstat(filepath.Join(root, sha)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hardlinked staging created CAS entry: %v", err)
	}
	streamAssertNoTemporary(t, root)
}

func TestPublishStagedBetweenReplacedDirectoryPaths(t *testing.T) {
	parent := t.TempDir()
	sourcePath := filepath.Join(parent, "source")
	targetPath := filepath.Join(parent, "target")
	sourceRoot, err := openDir(sourcePath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRoot.Close()
	targetRoot, err := openDir(targetPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer targetRoot.Close()
	data := []byte("same child name moved between independently pinned roots")
	name := digest(data)
	file, err := sourceRoot.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	defer sourceRoot.Remove(name)
	original, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write(data); err != nil {
		t.Fatal(err)
	}
	movedSource := filepath.Join(parent, "moved-source")
	movedTarget := filepath.Join(parent, "moved-target")
	for _, paths := range [][2]string{{sourcePath, movedSource}, {targetPath, movedTarget}} {
		if err = os.Rename(paths[0], paths[1]); err != nil {
			t.Fatal(err)
		}
		if err = os.Mkdir(paths[0], 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(paths[0], name), []byte("replacement sentinel"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = PublishStaged(context.Background(), sourceRoot, targetRoot, name, name, file, original, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if _, err = sourceRoot.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cross-directory move retained source name: %v", err)
	}
	assertPublishedSingleLink(t, movedTarget, name, data)
	for _, replacement := range []string{sourcePath, targetPath} {
		got, err := os.ReadFile(filepath.Join(replacement, name))
		if err != nil || string(got) != "replacement sentinel" {
			t.Fatalf("publication used replaced directory pathname: %s: %v", replacement, err)
		}
	}
}

func TestPublishStagedAcrossDirectoriesNeverReplacesWinner(t *testing.T) {
	data := []byte("cross-directory no-replace staging content")
	name := digest(data)
	for _, kind := range []string{"same", "corrupt", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			sourceRoot, err := openDir(filepath.Join(parent, "source"), true)
			if err != nil {
				t.Fatal(err)
			}
			defer sourceRoot.Close()
			defer sourceRoot.Remove(name)
			targetPath := filepath.Join(parent, "target")
			targetRoot, err := openDir(targetPath, true)
			if err != nil {
				t.Fatal(err)
			}
			defer targetRoot.Close()
			file, err := sourceRoot.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			original, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if _, err = file.Write(data); err != nil {
				t.Fatal(err)
			}
			winner := data
			if kind == "corrupt" {
				winner = bytes.Repeat([]byte("x"), len(data))
			}
			outside := filepath.Join(parent, "outside")
			if err = os.WriteFile(outside, winner, 0600); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(targetPath, name)
			switch kind {
			case "symlink":
				err = os.Symlink(outside, destination)
			case "hardlink":
				err = os.Link(outside, destination)
			default:
				err = os.WriteFile(destination, winner, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := targetRoot.Lstat(name)
			if err != nil {
				t.Fatal(err)
			}
			if err = PublishStaged(context.Background(), sourceRoot, targetRoot, name, name, file, original, int64(len(data))); !errors.Is(err, os.ErrExist) {
				t.Fatalf("cross-directory no-replace returned %v", err)
			}
			after, err := targetRoot.Lstat(name)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("cross-directory publication replaced winning inode")
			}
			staged, err := sourceRoot.Lstat(name)
			if err != nil || !streamRegular(staged) || !os.SameFile(original, staged) {
				t.Fatalf("failed cross-directory publication changed source: %v", err)
			}
			for _, path := range []string{destination, outside} {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, winner) {
					t.Fatalf("cross-directory publication changed winner: %s: %v", path, err)
				}
			}
		})
	}
}
