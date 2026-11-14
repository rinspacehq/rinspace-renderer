package typstadapter

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

const (
	FragmentIsolationVersion = "typst-bwrap/v2"
	fragmentSandboxUID       = 65534
	fragmentSandboxGID       = 65534
)

func sandboxCredential(euid int) *syscall.Credential {
	if euid != 0 {
		return nil
	}
	return &syscall.Credential{
		Uid:         fragmentSandboxUID,
		Gid:         fragmentSandboxGID,
		NoSetGroups: true,
	}
}

func prepareSandboxWorkspace(root string, credential *syscall.Credential) error {
	if credential == nil {
		return nil
	}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("sandbox workspace contains a symbolic link: %s", path)
		}
		return os.Chown(path, int(credential.Uid), int(credential.Gid))
	}); err != nil {
		return fmt.Errorf("prepare sandbox workspace ownership: %w", err)
	}
	return nil
}

// No host root, credentials, network, Docker socket or package cache enters the
// compiler namespace. The worker's resource limits and context deadline still
// apply. Namespace creation failures are errors, never an unsandboxed fallback.
func sandboxCommand(ctx context.Context, compiler Compiler, workspace, project, font, packages, output, entrypoint, format string) (*exec.Cmd, error) {
	args := []string{"--die-with-parent", "--new-session", "--unshare-all", "--cap-drop", "ALL",
		"--ro-bind", compiler.BinaryPath, "/typst", "--ro-bind", project, "/workspace",
		"--ro-bind", font, "/fonts", "--ro-bind", packages, "/packages", "--bind", output, "/output",
		"--tmpfs", "/tmp", "--proc", "/proc", "--dev", "/dev", "--dir", "/tmp/package-cache",
		"--clearenv", "--setenv", "HOME", "/tmp", "--setenv", "TMPDIR", "/tmp", "--setenv", "LANG", "C.UTF-8",
		"--setenv", "SOURCE_DATE_EPOCH", "0", "--chdir", "/workspace", "/typst", "compile", "--format", format,
		"--root", "/workspace", "--ignore-system-fonts", "--font-path", "/fonts", "--package-path", "/packages",
		"--package-cache-path", "/tmp/package-cache", "--jobs", "1", "--creation-timestamp", "0", "--diagnostic-format", "short"}
	if format == "html" {
		args = append(args, "--features", "html")
	}
	args = append(args, "/workspace/"+filepath.ToSlash(entrypoint), "/output/output."+format)
	command := exec.CommandContext(ctx, compiler.SandboxPath, args...)
	command.Dir = project
	command.Env = []string{"LANG=C.UTF-8", "PATH=/usr/bin:/bin"}
	credential := sandboxCredential(os.Geteuid())
	if err := prepareSandboxWorkspace(workspace, credential); err != nil {
		return nil, err
	}
	if credential != nil {
		command.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	}
	return command, nil
}
