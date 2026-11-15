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
	return prepareSandboxWorkspaceWithChown(root, credential, os.Chown)
}

func prepareSandboxWorkspaceWithChown(root string, credential *syscall.Credential, chown func(string, int, int) error) error {
	if credential == nil {
		return nil
	}
	paths := make([]string, 0, 16)
	if err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("sandbox workspace contains a symbolic link: %s", path)
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		return fmt.Errorf("prepare sandbox workspace ownership: %w", err)
	}
	// Keep every parent searchable by the current worker until all of its
	// children have been inspected and transferred. A root process without
	// DAC override can otherwise lock itself out after chowning a 0700 parent.
	for index := len(paths) - 1; index >= 0; index-- {
		if err := chown(paths[index], int(credential.Uid), int(credential.Gid)); err != nil {
			return fmt.Errorf("prepare sandbox workspace ownership: %w", err)
		}
	}
	return nil
}

func restoreSandboxWorkspace(root string, credential *syscall.Credential) error {
	return restoreSandboxWorkspaceWithChown(root, credential, os.Lchown)
}

func restoreSandboxWorkspaceWithChown(root string, credential *syscall.Credential, chown func(string, int, int) error) error {
	if credential == nil {
		return nil
	}
	uid, gid := os.Geteuid(), os.Getegid()
	// The sandbox identity owns 0700 directories while the compiler runs. A
	// root worker with CAP_DAC_OVERRIDE removed cannot even traverse those
	// directories after wait(2), so restore the root first and each directory
	// before Walk attempts to read its children.
	var unsafeLink error
	var restore func(string) error
	restore = func(path string) error {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 && unsafeLink == nil {
			unsafeLink = fmt.Errorf("sandbox workspace contains a symbolic link: %s", path)
		}
		if err := chown(path, uid, gid); err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		// Read a directory only after its ownership has been restored. This is
		// intentionally not filepath.Walk: Walk reads the directory before its
		// callback on current Go versions, which fails for 0700 sandbox paths.
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := restore(filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	err := restore(root)
	if err != nil {
		return fmt.Errorf("restore sandbox workspace ownership: %w", err)
	}
	if unsafeLink != nil {
		return unsafeLink
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
