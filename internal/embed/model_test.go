package embed

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOrtProviderRespectsExplicitChoice(t *testing.T) {
	// The explicit setting must win over the probe in both directions: a machine
	// with CUDA can be pinned to DirectML, and one without can be told to try
	// CUDA anyway (and fail loudly rather than degrade).
	for _, want := range []string{"cpu", "cuda", "directml"} {
		t.Setenv("CONTEXTMAXXER_ORT_PROVIDER", want)
		require.Equal(t, want, OrtProvider())
		require.True(t, ortProviderIsExplicit())
	}
}

func TestOrtProviderAutoIsGPUOnWindowsAndCPUElsewhere(t *testing.T) {
	t.Setenv("CONTEXTMAXXER_ORT_PROVIDER", "")
	got := OrtProvider()
	require.False(t, ortProviderIsExplicit())
	if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" {
		// Which of the two depends on whether the CUDA stack is installed here,
		// but auto must never land on CPU where a GPU path exists.
		require.Contains(t, []string{"cuda", "directml"}, got)
		require.Equal(t, got == "cuda", cudaRuntimeAvailable(),
			"auto must pick cuda exactly when its runtime loads")
	} else {
		require.Equal(t, "cpu", got)
	}
}

// TestOrtProviderProbeIsObservable is a diagnostic: run it with and without the
// CUDA libraries on PATH to confirm the probe actually flips, since the
// equivalence assertion above holds either way.
func TestOrtProviderProbeIsObservable(t *testing.T) {
	t.Setenv("CONTEXTMAXXER_ORT_PROVIDER", "")
	t.Logf("cudaRuntimeAvailable=%v provider=%q", cudaRuntimeAvailable(), OrtProvider())
}
