package embed

import (
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
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

func TestDownloadRefusesAnUnpinnedArtifact(t *testing.T) {
	// An empty checksum used to mean "skip the check". It now means "this
	// artifact has no pin", and the download is refused before any request is
	// made — so this test needs no network.
	t.Setenv(unverifiedDownloadEnv, "")
	err := downloadFileWithProgress("https://example.invalid/native.zip",
		filepath.Join(t.TempDir(), "out.bin"), "", slog.Default())
	if err == nil {
		t.Fatal("expected a refusal for an artifact with no pinned sha256")
	}
	if !strings.Contains(err.Error(), unverifiedDownloadEnv) {
		t.Fatalf("refusal should name the override env; got: %v", err)
	}
}

func TestRuntimeForThisPlatformIsPinned(t *testing.T) {
	// CI builds on Linux and Windows, so between them this covers the runtimes
	// users actually download. A new platform or a version bump lands here as a
	// failure until its hash is filled in, which is the point of the table.
	for _, provider := range []string{"cpu", "directml", "cuda"} {
		t.Setenv("CONTEXTMAXXER_ORT_PROVIDER", provider)
		info, err := onnxRuntimePlatformInfo()
		if err != nil {
			continue // provider/arch combination this build does not support
		}
		if info.sha256 == "" {
			t.Errorf("provider %s: runtime archive %q has no pinned sha256", provider, info.platform)
		}
		for _, comp := range info.companions {
			if comp.sha256 == "" {
				t.Errorf("provider %s: companion %q has no pinned sha256", provider, comp.outName)
			}
		}
	}
}
