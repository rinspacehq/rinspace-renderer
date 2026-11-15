package typstadapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSandboxCredentialDropsOnlyRoot(t *testing.T) {
	if credential := sandboxCredential(1000); credential != nil {
		t.Fatalf("non-root worker unexpectedly changed identity: %+v", credential)
	}
	credential := sandboxCredential(0)
	if credential == nil || credential.Uid != fragmentSandboxUID || credential.Gid != fragmentSandboxGID || !credential.NoSetGroups {
		t.Fatalf("root worker did not select the bounded sandbox identity: %+v", credential)
	}
}

func TestSandboxWorkspaceRejectsLinksBeforeExecution(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("missing", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	credential := &syscall.Credential{Uid: uint32(os.Geteuid()), Gid: uint32(os.Getegid()), NoSetGroups: true}
	if err := prepareSandboxWorkspace(root, credential); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("linked workspace was accepted: %v", err)
	}
}

func TestSandboxWorkspaceTransfersChildrenBeforeParents(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "project", "blocks")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(nested, "lesson.typ")
	if err := os.WriteFile(file, []byte("bounded lesson"), 0o600); err != nil {
		t.Fatal(err)
	}
	credential := &syscall.Credential{Uid: fragmentSandboxUID, Gid: fragmentSandboxGID, NoSetGroups: true}
	transferred := []string{}
	err := prepareSandboxWorkspaceWithChown(root, credential, func(path string, uid, gid int) error {
		if uid != fragmentSandboxUID || gid != fragmentSandboxGID {
			t.Fatalf("unexpected sandbox identity: %d:%d", uid, gid)
		}
		transferred = append(transferred, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	position := map[string]int{}
	for index, path := range transferred {
		position[path] = index
	}
	for _, child := range []string{file, nested, filepath.Dir(nested)} {
		parent := filepath.Dir(child)
		if position[child] >= position[parent] {
			t.Fatalf("child %s was transferred after parent %s: %v", child, parent, transferred)
		}
	}
	if transferred[len(transferred)-1] != root {
		t.Fatalf("workspace root was not transferred last: %v", transferred)
	}
}

func TestSandboxWorkspaceRestoresParentsBeforeChildren(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "output", "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(nested, "output.html")
	if err := os.WriteFile(file, []byte("<p>bounded</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	credential := &syscall.Credential{Uid: fragmentSandboxUID, Gid: fragmentSandboxGID, NoSetGroups: true}
	restored := []string{}
	err := restoreSandboxWorkspaceWithChown(root, credential, func(path string, uid, gid int) error {
		if uid != os.Geteuid() || gid != os.Getegid() {
			t.Fatalf("unexpected worker identity: %d:%d", uid, gid)
		}
		restored = append(restored, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	position := map[string]int{}
	for index, path := range restored {
		position[path] = index
	}
	if restored[0] != root {
		t.Fatalf("workspace root was not restored first: %v", restored)
	}
	for _, child := range []string{filepath.Join(root, "output"), nested, file} {
		parent := filepath.Dir(child)
		if position[parent] >= position[child] {
			t.Fatalf("parent %s was restored after child %s: %v", parent, child, restored)
		}
	}
}

func TestSandboxWorkspaceRestoreRejectsGeneratedLinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("missing", filepath.Join(root, "output.html")); err != nil {
		t.Fatal(err)
	}
	credential := &syscall.Credential{Uid: fragmentSandboxUID, Gid: fragmentSandboxGID, NoSetGroups: true}
	if err := restoreSandboxWorkspaceWithChown(root, credential, func(string, int, int) error { return nil }); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("generated link was accepted: %v", err)
	}
}

func TestSandboxBindsPinnedCompilerOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	compiler := Compiler{
		BinaryPath:  filepath.Join(root, "pinned", "typst"),
		SandboxPath: filepath.Join(root, "bwrap"),
	}
	for _, path := range []string{compiler.BinaryPath, compiler.SandboxPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("pinned"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	workspace := filepath.Join(root, "workspace")
	project := filepath.Join(workspace, "project")
	font := filepath.Join(workspace, "fonts")
	packages := filepath.Join(workspace, "packages")
	output := filepath.Join(workspace, "output")
	for _, path := range []string{project, font, packages, output} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	command, err := sandboxCommand(t.Context(), compiler, workspace, project, font, packages, output, "main.typ", "html")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command.Args, "\x00")
	if !strings.Contains(joined, "--ro-bind\x00"+compiler.BinaryPath+"\x00/typst") {
		t.Fatalf("sandbox did not bind the pinned compiler directly: %q", command.Args)
	}
	if strings.Contains(joined, workspace+string(os.PathSeparator)+"compiler") {
		t.Fatalf("sandbox copied the compiler into the noexec workspace: %q", command.Args)
	}
}

func configureTestSandbox(t *testing.T, compiler Compiler) Compiler {
	t.Helper()
	compiler.SandboxPath = os.Getenv("RIN_TYPST_TEST_SANDBOX")
	if compiler.SandboxPath == "" {
		t.Skip("set RIN_TYPST_TEST_SANDBOX to the actual reviewed bubblewrap")
	}
	file, err := os.ReadFile(compiler.SandboxPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(file)
	compiler.SandboxHash = hex.EncodeToString(digest[:])
	return compiler
}

func TestSandboxHidesHostFilesEnvironmentAndNetwork(t *testing.T) {
	compiler := configureTestSandbox(t, Compiler{Timeout: 10 * time.Second, MaxOutput: 1 << 20})
	root := t.TempDir()
	binary := filepath.Join(root, "probe")
	build := exec.Command("go", "build", "-o", binary, "testdata/sandbox_probe.go")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOMAXPROCS=2")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build static sandbox probe: %v %s", err, output)
	}
	file, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(file)
	compiler.BinaryPath = binary
	compiler.BinaryHash = hex.EncodeToString(digest[:])
	compiler.FontPath = filepath.Join(root, "synthetic-font")
	if err := os.WriteFile(compiler.FontPath, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	digest = sha256.Sum256([]byte("synthetic"))
	compiler.FontHash = hex.EncodeToString(digest[:])
	canary := filepath.Join(root, "host-only-canary")
	if err := os.WriteFile(canary, []byte("not a credential"), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	input, _ := json.Marshal(map[string]string{"Canary": canary, "Endpoint": listener.Addr().String()})
	t.Setenv("RIN_SYNTHETIC_HOST_CANARY", "host-only")
	result, err := compiler.Compile(t.Context(), map[string][]byte{"main.typ": input}, "main.typ", "html")
	if err != nil {
		t.Fatalf("isolated static probe: %v %s", err, result.Log)
	}
	var report struct {
		CanReadCanary, CanReachHost       bool
		NetworkNamespace, HostEnvironment string
	}
	if err := json.Unmarshal(result.Output, &report); err != nil {
		t.Fatal(err)
	}
	hostNamespace, _ := os.Readlink("/proc/self/ns/net")
	if report.CanReadCanary || report.CanReachHost || report.NetworkNamespace == "" || report.NetworkNamespace == hostNamespace || report.HostEnvironment != "" {
		t.Fatalf("sandbox exposed host state: %+v", report)
	}
}

func TestPinnedSandboxCompilesSemanticMathAndFailsClosed(t *testing.T) {
	compiler := configureTestSandbox(t, pinnedCorpusCompiler(t))
	result, err := CompileFragments(t.Context(), compiler, []FragmentBlock{{ID: "one", Source: "Hello $x+1$"}})
	if err != nil || !strings.Contains(result.Document.Pages[0].HTML, "<math") {
		t.Fatalf("isolated native compiler failed: %v %s", err, result.Log)
	}
	plain, err := CompileFragments(t.Context(), compiler, []FragmentBlock{{ID: "plain", Source: "只有普通文字的回答也必须产生 HTML。"}})
	if err != nil || !strings.Contains(plain.Document.Pages[0].HTML, "只有普通文字") {
		t.Fatalf("isolated plain-text fragment failed: %v %s", err, plain.Log)
	}
	compiler.SandboxHash = strings.Repeat("0", 64)
	if _, err := CompileFragments(t.Context(), compiler, []FragmentBlock{{ID: "one", Source: "Hello"}}); err == nil {
		t.Fatal("unreviewed sandbox silently fell back")
	}
}
