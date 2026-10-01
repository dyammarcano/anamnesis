//go:build !windows

package winpath

// Refresh is a no-op outside Windows (Windows first; other platforms deferred).
func Refresh() []string { return nil }
