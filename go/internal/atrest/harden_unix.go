//go:build !windows

package atrest

// hardenKeyFile is a no-op on Unix: the loadOrCreateKey path already
// opens the file with mode 0600, which the kernel enforces. Nothing
// further to do here -- this file exists only so the Windows build
// can compile its sibling implementation.
func hardenKeyFile(_ string) error {
	return nil
}
