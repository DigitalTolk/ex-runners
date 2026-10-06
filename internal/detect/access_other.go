//go:build !unix

package detect

import "os"

// executable is fs.accessSync(p, X_OK) where X_OK means nothing (Windows):
// Node then only checks that p exists, and so do we.
func executable(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
