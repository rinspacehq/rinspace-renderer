package typstadapter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const EmptyPackageHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// ReadPackageSnapshot seals exact package file bytes before execution. The
// compiler never receives a mutable host cache or permission to download a
// missing package. The digest covers paths, content hashes AND sizes, not just
// package names/versions; modifying a dependency invalidates the render profile.
func ReadPackageSnapshot(directory, expected string) (map[string][]byte, error) {
	files := map[string][]byte{}
	if directory == "" {
		if expected != "" && expected != EmptyPackageHash {
			return nil, fmt.Errorf("Typst package hash requires a sealed package directory")
		}
		return files, nil
	}
	info, err := os.Lstat(directory)
	if !filepath.IsAbs(directory) || err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || len(expected) != 64 {
		return nil, fmt.Errorf("Typst sealed package profile is invalid")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var size int64
	err = fs.WalkDir(root.FS(), ".", func(name string, item fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if item.IsDir() {
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 4<<20 || len(files) >= 2000 || strings.ContainsAny(name, "\x00\\\r\n") {
			return fmt.Errorf("Typst package file is invalid: %s", name)
		}
		size += info.Size()
		if size > 64<<20 {
			return fmt.Errorf("Typst package set is too large")
		}
		body, err := root.ReadFile(name)
		if err != nil {
			return err
		}
		if int64(len(body)) != info.Size() {
			return fmt.Errorf("Typst package changed during snapshot")
		}
		files[name] = body
		return nil
	})
	if err != nil {
		return nil, err
	}
	if PackageSnapshotHash(files) != expected {
		return nil, fmt.Errorf("Typst package set does not match its pinned hash")
	}
	return files, nil
}

func PackageSnapshotHash(files map[string][]byte) string {
	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, name := range paths {
		body := files[name]
		digest := sha256.Sum256(body)
		fmt.Fprintf(hash, "%s\x00%x\x00%d\n", name, digest, len(body))
	}
	return hex.EncodeToString(hash.Sum(nil))
}
