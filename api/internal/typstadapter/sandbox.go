package typstadapter

import (
	"context"
	"os/exec"
	"path/filepath"
)

const FragmentIsolationVersion = "typst-bwrap/v1"

// No host root, credentials, network, Docker socket or package cache enters the
// compiler namespace. The worker's resource limits and context deadline still
// apply. Namespace creation failures are errors, never an unsandboxed fallback.
func sandboxCommand(ctx context.Context, compiler Compiler, project, font, packages, output, entrypoint, format string) *exec.Cmd {
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
	return command
}
