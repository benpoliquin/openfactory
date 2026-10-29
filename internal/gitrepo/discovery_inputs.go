package gitrepo

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"openfactory/internal/model"
)

const discoveryInputMaxBytes = 64 << 20

// materializeDiscoveryInputs installs controller-owned context before publishing
// the session. Inputs are immutable: retries may reuse exact bytes, never replace
// an unexpected existing member. The workspace receipt and every directory entry
// remain pinned by the same chain used for the session and fingerprint guards.
func materializeDiscoveryInputs(ctx context.Context, c *discoveryChain, session model.DiscoverySession) error {
	files := session.InputFiles
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := discoveryMember(file.Path); err != nil {
			return errors.New("invalid discovery input path")
		}
		if !strings.HasPrefix(file.Path, "inputs/") || strings.ContainsRune(file.Path, '\x00') {
			return errors.New("discovery input must be an inputs/ file")
		}
		if seen[file.Path] {
			return errors.New("duplicate discovery input path")
		}
		seen[file.Path] = true
		if len(file.Data) > discoveryInputMaxBytes {
			return errors.New("discovery input exceeds byte limit")
		}
		sum := sha256.Sum256(file.Data)
		var encoded [sha256.Size * 2]byte
		hex.Encode(encoded[:], sum[:])
		if file.ContentSHA != string(encoded[:]) {
			return errors.New("discovery input SHA mismatch")
		}
		if session.CapabilityToken != "" && bytes.Contains(file.Data, []byte(session.CapabilityToken)) {
			return errors.New("discovery input contains a private capability")
		}
	}
	for _, file := range files {
		for parent := file.Path; ; {
			i := strings.LastIndexByte(parent, '/')
			if i < 0 {
				break
			}
			parent = parent[:i]
			if seen[parent] {
				return errors.New("discovery input path is also a parent")
			}
		}
	}
	for _, file := range files {
		if err := materializeDiscoveryInput(ctx, c, file); err != nil {
			return fmt.Errorf("materialize discovery input: %w", err)
		}
	}
	return c.verify()
}

func materializeDiscoveryInput(ctx context.Context, c *discoveryChain, file model.DiscoveryInputFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	depth := len(c.roots)
	defer func() {
		for len(c.roots) > depth {
			c.roots[len(c.roots)-1].Close()
			c.roots = c.roots[:len(c.roots)-1]
			c.names = c.names[:len(c.names)-1]
			c.infos = c.infos[:len(c.infos)-1]
		}
	}()
	parts := strings.Split(file.Path, "/")
	for _, part := range parts[:len(parts)-1] {
		root := c.roots[len(c.roots)-1]
		if err := root.Mkdir(part, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		if err := c.descend(part); err != nil {
			return err
		}
	}
	root := c.roots[len(c.roots)-1]
	leaf := parts[len(parts)-1]
	if _, err := root.Lstat(leaf); err == nil {
		return verifyDiscoveryInput(ctx, c, leaf, file.Data)
	} else if !os.IsNotExist(err) {
		return err
	}

	// A rooted hard-link install is the portable no-replace equivalent of an
	// exclusive rename. Ordinary Rename could overwrite a raced-in destination.
	// The temporary name is removed before the published file is checked for its
	// required single-link identity; no partially written input is ever visible.
	temp := ".input-" + rand.Text()
	f, err := root.OpenFile(temp, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	defer root.Remove(temp)
	if _, err = f.Write(file.Data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	current, err := root.Lstat(temp)
	if err != nil {
		return err
	}
	if !singleLinkDiscoveryInput(opened) || !singleLinkDiscoveryInput(current) || !os.SameFile(opened, current) {
		return errors.New("discovery input temporary file changed")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = c.verify(); err != nil {
		return err
	}
	if err = root.Link(temp, leaf); err != nil {
		if os.IsExist(err) {
			return verifyDiscoveryInput(ctx, c, leaf, file.Data)
		}
		return err
	}
	if err = root.Remove(temp); err != nil {
		return err
	}
	current, err = root.Lstat(leaf)
	if err != nil {
		return err
	}
	after, err := f.Stat()
	if err != nil {
		return err
	}
	if !singleLinkDiscoveryInput(current) || !singleLinkDiscoveryInput(after) || !os.SameFile(opened, current) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return errors.New("discovery input changed during publication")
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

func singleLinkDiscoveryInput(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && ok && stat.Nlink == 1
}

func verifyDiscoveryInput(ctx context.Context, c *discoveryChain, leaf string, data []byte) error {
	root := c.roots[len(c.roots)-1]
	before, err := root.Lstat(leaf)
	if err != nil {
		return err
	}
	if !singleLinkDiscoveryInput(before) {
		return errors.New("discovery input is not a single-link regular file")
	}
	if before.Size() != int64(len(data)) {
		return errors.New("existing discovery input differs")
	}
	f, err := root.OpenFile(leaf, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !singleLinkDiscoveryInput(opened) || !os.SameFile(before, opened) || opened.Size() != int64(len(data)) {
		return errors.New("discovery input changed while opening")
	}
	var buffer [32 << 10]byte
	offset := 0
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, readErr := f.Read(buffer[:])
		if n > len(data)-offset || !bytes.Equal(buffer[:n], data[offset:offset+n]) {
			return errors.New("existing discovery input differs")
		}
		offset += n
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	after, err := f.Stat()
	if err != nil {
		return err
	}
	current, err := root.Lstat(leaf)
	if err != nil {
		return err
	}
	if offset != len(data) || !singleLinkDiscoveryInput(after) || !singleLinkDiscoveryInput(current) || !os.SameFile(opened, current) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return errors.New("discovery input changed while reading")
	}
	return c.verify()
}
