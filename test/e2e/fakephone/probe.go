package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// procFields are the /proc/<pid>/status fields proc prints: who a process
// runs as and what privilege it holds.
var procFields = []string{"Name", "Uid", "Gid", "Groups", "CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb", "NoNewPrivs", "Seccomp"}

// findProcesses returns, for every process under procDir whose command line
// starts with argv (argv[0] compared by base name), its pid and the status
// fields in procFields, one "Field: value" line each.
func findProcesses(procDir string, argv []string) (map[string]string, error) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		pid := e.Name()
		if strings.Trim(pid, "0123456789") != "" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(procDir, pid, "cmdline")) //nolint:gosec // G304: a pid directory under procDir.
		if err != nil || len(raw) == 0 {
			continue // gone, or a kernel thread
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if len(args) < len(argv) || filepath.Base(args[0]) != argv[0] {
			continue
		}
		match := true
		for i := 1; i < len(argv); i++ {
			match = match && args[i] == argv[i]
		}
		if !match {
			continue
		}
		status, err := os.ReadFile(filepath.Join(procDir, pid, "status")) //nolint:gosec // G304: a pid directory under procDir.
		if err != nil {
			continue
		}
		var b strings.Builder
		for line := range strings.SplitSeq(string(status), "\n") {
			k, _, _ := strings.Cut(line, ":")
			for _, f := range procFields {
				if k == f {
					b.WriteString(line + "\n")
				}
			}
		}
		out[pid] = b.String()
	}
	return out, nil
}

func newProcCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "proc <argv0> [arg]...",
		Short: "Print the credentials of every process whose command line starts so",
		Long: `Scan /proc for processes whose command line starts with the arguments
(the first compared by base name: "proc patchbay agent" finds "/patchbay
agent --listen=..."), and print each one's uids, gids, groups, capability
sets and no_new_privs from /proc/<pid>/status, after a "=== pid <n>" line.
Run in a pod with hostPID, it shows what the kernel enforces on another
pod's containers. Exit 0 when at least one process matched.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			found, err := findProcesses("/proc", args)
			if err != nil {
				return err
			}
			if len(found) == 0 {
				return fmt.Errorf("no process runs %q", strings.Join(args, " "))
			}
			for pid, status := range found {
				fmt.Fprintf(cmd.OutOrStdout(), "=== pid %s\n%s", pid, status)
			}
			return nil
		},
	}
}

// readFile tries to read path and says what happened in one line.
func readFile(path string) string {
	f, err := os.Open(path) //nolint:gosec // G304: reading the caller's path is the point.
	if err != nil {
		return fmt.Sprintf("read %s: error: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(f, 1<<20))
	if err != nil {
		return fmt.Sprintf("read %s: error: %v", path, err)
	}
	return fmt.Sprintf("read %s: ok bytes=%d", path, n)
}

func newReadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "read <path>...",
		Short: "Try to read each file and print whether it worked (never its content)",
		Long: `Open and read each path as this process's user and print one line per
path: "read <path>: ok bytes=<n>" or "read <path>: error: <why>". The
content is never printed. With the pod's identity set to another
container's, it shows which files that container can read. Always exits 0:
the caller judges the lines.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			groups, _ := os.Getgroups()
			fmt.Fprintf(cmd.OutOrStdout(), "uid=%d gid=%d groups=%v\n", os.Getuid(), os.Getgid(), groups)
			for _, p := range args {
				fmt.Fprintln(cmd.OutOrStdout(), readFile(p))
			}
			return nil
		},
	}
}
