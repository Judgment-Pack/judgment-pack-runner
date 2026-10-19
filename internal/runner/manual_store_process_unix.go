//go:build linux || darwin || freebsd

package runner

import (
	"io"
	"os"
	"syscall"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// This adapter is reachable only from the process-terminating helper entry and
// explicit helper subprocess tests. Ordinary Open/PrepareManualStore never call
// it. cwd stays anchored to the retained directory until this helper exits.
// A private nondefault VFS preserves one exact relative main filename; its
// native callbacks derive DB/journal/WAL/SHM paths relative to that fixed cwd.
func manualProcessDatabaseTarget(lease *ExecutorLease) (string, io.Closer, error) {
	if verifyManualLease(lease) != nil {
		return "", nil, errManualProtocol
	}
	dir, err := lease.state.Open(".")
	if err != nil {
		return "", nil, err
	}
	if err = syscall.Fchdir(int(dir.Fd())); err != nil {
		dir.Close()
		return "", nil, err
	}
	held, err := dir.Stat()
	current, e := os.Stat(".")
	if err != nil || e != nil || !os.SameFile(held, current) {
		dir.Close()
		return "", nil, errManualProtocol
	}
	anchor, err := newManualProcessVFS(dir)
	if err != nil {
		dir.Close()
		return "", nil, err
	}
	return "file:runs.sqlite?vfs=" + anchor.name, anchor, nil
}

type manualProcessVFS struct {
	dir        *os.File
	tls        *libc.TLS
	name       string
	cname, vfs uintptr
}

func newManualProcessVFS(dir *os.File) (*manualProcessVFS, error) {
	a := &manualProcessVFS{dir: dir, tls: libc.NewTLS(), name: id("manual-root-")}
	base := sqlite3.Xsqlite3_vfs_find(a.tls, 0)
	if base == 0 {
		a.tls.Close()
		return nil, errManualProtocol
	}
	var err error
	a.cname, err = libc.CString(a.name)
	if err != nil {
		a.tls.Close()
		return nil, err
	}
	a.vfs = libc.Xmalloc(a.tls, libc.Tsize_t(unsafe.Sizeof(sqlite3.Tsqlite3_vfs{})))
	if a.vfs == 0 {
		libc.Xfree(a.tls, a.cname)
		a.tls.Close()
		return nil, errManualProtocol
	}
	// These live ABI addresses come from SQLite/libc, not caller Go data.
	// Use libc's native-memory ABI and compiler-derived field offsets.
	libc.Xmemcpy(a.tls, a.vfs, base, libc.Tsize_t(unsafe.Sizeof(sqlite3.Tsqlite3_vfs{})))
	libc.AssignPtrUintptr(a.vfs+unsafe.Offsetof(sqlite3.Tsqlite3_vfs{}.FpNext), 0)
	libc.AssignPtrUintptr(a.vfs+unsafe.Offsetof(sqlite3.Tsqlite3_vfs{}.FzName), a.cname)
	// SQLite's documented cFuncPointer pattern (sqlite.go): a nonclosure
	// function declaration has a stable function value; unsafe Rule #1 applies.
	fullPathname := *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uintptr, uintptr, int32, uintptr) int32
	}{manualProcessFullPathname}))
	libc.AssignPtrUintptr(a.vfs+unsafe.Offsetof(sqlite3.Tsqlite3_vfs{}.FxFullPathname), fullPathname)
	// Keep the actual native default intact. Only this DSN selects the clone.
	if sqlite3.Xsqlite3_vfs_register(a.tls, a.vfs, 0) != sqlite3.SQLITE_OK {
		libc.Xfree(a.tls, a.vfs)
		libc.Xfree(a.tls, a.cname)
		a.tls.Close()
		return nil, errManualProtocol
	}
	return a, nil
}
func (a *manualProcessVFS) Close() error {
	// The caller defers this before opening SQLite, so DB/sidecars close first.
	sqlite3.Xsqlite3_vfs_unregister(a.tls, a.vfs)
	libc.Xfree(a.tls, a.vfs)
	libc.Xfree(a.tls, a.cname)
	a.tls.Close()
	return a.dir.Close()
}
func manualProcessFullPathname(tls *libc.TLS, _ uintptr, path uintptr, nOut int32, out uintptr) int32 {
	const name = "runs.sqlite"
	if path == 0 || out == 0 || nOut < int32(len(name)+1) || libc.GoString(path) != name {
		return sqlite3.SQLITE_CANTOPEN
	}
	// Validation established the exact NUL-terminated native input extent;
	// copy it within the native ABI without converting either address to Go.
	libc.Xmemcpy(tls, out, path, libc.Tsize_t(len(name)+1))
	return sqlite3.SQLITE_OK
}
