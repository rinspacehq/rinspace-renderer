package orchestration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration/orchestrationtest"
)

func TestProjectSnapshotReadsOnlyVerifiedBoundedFiles(t *testing.T) {
	mainBody := []byte("# Main\n")
	assetBody := []byte("svg")
	graph := snapshotGraph(t, map[string][]byte{"main.md": mainBody, "images/a.svg": assetBody})
	snapshot := orchestration.ProjectSnapshot{Graph: graph, Files: orchestrationtest.MemoryProjectFiles{Files: map[string][]byte{
		"main.md": mainBody, "images/a.svg": assetBody,
	}}}
	got, err := snapshot.ReadFileVerified(context.Background(), "main.md", 32)
	if err != nil || string(got) != string(mainBody) {
		t.Fatalf("verified read: body=%q err=%v", got, err)
	}
	all, err := snapshot.ReadAllVerified(context.Background(), 32, 64)
	if err != nil || len(all) != 2 {
		t.Fatalf("verified read all: files=%d err=%v", len(all), err)
	}
	all["main.md"][0] = 'X'
	got, err = snapshot.ReadFileVerified(context.Background(), "main.md", 32)
	if err != nil || string(got) != string(mainBody) {
		t.Fatal("project reader returned aliased bytes")
	}
}

func TestProjectSnapshotRejectsUnknownOversizedAndTamperedFiles(t *testing.T) {
	body := []byte("# Main\n")
	graph := snapshotGraph(t, map[string][]byte{"main.md": body})
	snapshot := orchestration.ProjectSnapshot{Graph: graph, Files: orchestrationtest.MemoryProjectFiles{Files: map[string][]byte{"main.md": body}}}
	for name, read := range map[string]func() error{
		"unknown":     func() error { _, err := snapshot.ReadFileVerified(context.Background(), "missing.md", 32); return err },
		"unsafe":      func() error { _, err := snapshot.ReadFileVerified(context.Background(), "../main.md", 32); return err },
		"file-limit":  func() error { _, err := snapshot.ReadFileVerified(context.Background(), "main.md", 2); return err },
		"total-limit": func() error { _, err := snapshot.ReadAllVerified(context.Background(), 32, 2); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := read(); err == nil {
				t.Fatal("expected bounded read to fail")
			}
		})
	}
	tampered := snapshot
	tampered.Files = orchestrationtest.MemoryProjectFiles{Files: map[string][]byte{"main.md": []byte("tampered")}}
	if _, err := tampered.ReadFileVerified(context.Background(), "main.md", 32); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("expected tampered file rejection, got %v", err)
	}
	tampered.Files = orchestrationtest.MemoryProjectFiles{Files: map[string][]byte{"main.md": []byte("# Maim\n")}}
	if _, err := tampered.ReadFileVerified(context.Background(), "main.md", 32); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("expected same-size hash rejection, got %v", err)
	}
}

func snapshotGraph(t *testing.T, bodies map[string][]byte) contracts.ProjectGraph {
	t.Helper()
	files := make([]contracts.ProjectGraphFile, 0, len(bodies))
	for projectPath, body := range bodies {
		digest := sha256.Sum256(body)
		role := contracts.ProjectFileRoleAsset
		if projectPath == "main.md" {
			role = contracts.ProjectFileRoleSource
		}
		files = append(files, contracts.ProjectGraphFile{Path: projectPath, SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(body)), Role: role})
	}
	projectHash, err := contracts.ComputeProjectHash(contracts.ContentKindMarkdown, files)
	if err != nil {
		t.Fatal(err)
	}
	return contracts.ProjectGraph{
		SchemaVersion: contracts.ProjectGraphSchemaVersion, ProjectHash: projectHash, ContentKind: contracts.ContentKindMarkdown,
		Entrypoints: []contracts.ProjectEntrypoint{{Path: "main.md", Role: contracts.EntrypointRoleDocument}}, Files: files,
		References: []contracts.ProjectReference{}, Options: map[string]any{},
	}
}
