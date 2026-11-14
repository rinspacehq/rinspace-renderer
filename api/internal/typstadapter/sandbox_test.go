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
	compiler.SandboxHash = strings.Repeat("0", 64)
	if _, err := CompileFragments(t.Context(), compiler, []FragmentBlock{{ID: "one", Source: "Hello"}}); err == nil {
		t.Fatal("unreviewed sandbox silently fell back")
	}
}
