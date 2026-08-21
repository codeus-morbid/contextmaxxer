//go:build !windows

package embed

// cudaRuntimeAvailable is Windows-only for now: elsewhere `auto` stays on CPU,
// so there is no provider choice to inform.
func cudaRuntimeAvailable() bool { return false }
