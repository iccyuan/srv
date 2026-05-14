//go:build windows

package moshx

import "os"

// Windows doesn't have SIGWINCH; console resize events come through a
// different API (GetConsoleScreenBufferInfo polling or
// ENABLE_WINDOW_INPUT). For v1 we return a never-firing channel so
// the client loop compiles cleanly; remote rendering stays at the
// initial window size for the session's lifetime on a Windows client.
func registerSigwinch() <-chan os.Signal {
	return make(chan os.Signal, 1)
}
