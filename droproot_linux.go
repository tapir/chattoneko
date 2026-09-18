//go:build linux

package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// The uid/gid the app runs as once it has dropped root: the pair the image's
// data directory is owned by, and the one `docker run --user 1000` picks.
const (
	runUID = 1000
	runGID = 1000
)

// dropRoot makes dir writable by runUID and drops the whole process to it.
//
// The container starts as root for exactly one reason: a bind-mounted data
// directory arrives with host ownership, and without this the DB open fails
// with EACCES and the user has to chown by hand. Everything after this call
// runs unprivileged, which matters because ffmpeg parses untrusted media inside
// this process. A no-op when the container was started with --user.
//
// syscall.Setuid is process-wide on Linux since Go 1.16 (it sets every thread),
// so this needs no re-exec through a su-exec-like helper and leaves no window
// where a goroutine is still root.
func dropRoot(dir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	// Best effort, like the shell entrypoint this replaces: a read-only mount
	// or an unmapped uid (rootless docker) fails here instead, and the DB open
	// then reports the real problem.
	_ = os.MkdirAll(dir, 0o755)
	_ = filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		return os.Chown(p, runUID, runGID)
	})
	// Supplementary groups first: Setuid cannot drop them afterwards.
	_ = syscall.Setgroups(nil)
	if err := syscall.Setgid(runGID); err != nil {
		return fmt.Errorf("setgid(%d): %w", runGID, err)
	}
	if err := syscall.Setuid(runUID); err != nil {
		return fmt.Errorf("setuid(%d): %w", runUID, err)
	}
	if os.Geteuid() != runUID {
		return fmt.Errorf("still running as uid %d after the drop", os.Geteuid())
	}
	return nil
}
