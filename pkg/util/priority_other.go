//go:build !windows

package util

// SetBackgroundPriority is a no-op on non-Windows platforms.
// The project only targets Windows for real use; this stub keeps builds green on CI/Linux/macOS.
func SetBackgroundPriority() error {
	return nil
}
