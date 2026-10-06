//go:build unix

package detect

import "syscall"

// xOK is X_OK from <unistd.h> (1 on every POSIX system).
const xOK = 0x1

// executable is fs.accessSync(p, X_OK): the real user may execute p. As in
// the TS, a searchable directory passes too — its --version then fails and
// is reported as "found but not working".
func executable(p string) bool { return syscall.Access(p, xOK) == nil }
