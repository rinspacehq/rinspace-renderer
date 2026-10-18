package typstadapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealedPackageSnapshotRejectsDriftAndLinks(t *testing.T) {
	root := t.TempDir()
	name := "preview/example/0.1.0/lib.typ"
	body := []byte("#let answer = 42\n")
	file := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	hash := PackageSnapshotHash(map[string][]byte{name: body})
	files, err := ReadPackageSnapshot(root, hash)
	if err != nil || string(files[name]) != string(body) {
		t.Fatalf("sealed snapshot: %v", err)
	}
	if PackageSnapshotHash(nil) != EmptyPackageHash {
		t.Fatal("empty digest drift")
	}
	if _, err := ReadPackageSnapshot("", hash); err == nil {
		t.Fatal("hash without package path was accepted")
	}
	if err := os.WriteFile(file, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPackageSnapshot(root, hash); err == nil {
		t.Fatal("changed package bytes were accepted")
	}
	if err := os.Symlink(file, filepath.Join(root, "link.typ")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPackageSnapshot(root, hash); err == nil {
		t.Fatal("package symlink was accepted")
	}
}

func TestPinnedCompilerUsesOnlySealedPackages(t *testing.T) {
	compiler := pinnedCorpusCompiler(t)
	root := t.TempDir()
	files := map[string][]byte{
		"preview/rin-test/0.1.0/typst.toml": []byte("[package]\nname = \"rin-test\"\nversion = \"0.1.0\"\nentrypoint = \"lib.typ\"\n"),
		"preview/rin-test/0.1.0/lib.typ":    []byte("#let value = [SEALED_PACKAGE_CONTENT]\n"),
	}
	for name, body := range files {
		file := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	compiler.PackagePath, compiler.PackageHash = root, PackageSnapshotHash(files)
	result, err := compiler.Compile(t.Context(), map[string][]byte{"main.typ": []byte("#import \"@preview/rin-test:0.1.0\": value\n#value\n")}, "main.typ", "html")
	if err != nil || !strings.Contains(string(result.Output), "SEALED_PACKAGE_CONTENT") {
		t.Fatalf("sealed package compile: %v %s", err, result.Log)
	}
}
