package embed

import (
	"sync"
	"syscall"
)

// cudaRuntimeLibs are the libraries the ONNX Runtime CUDA execution provider
// links against. cuDNN 9 is split into several objects; the graph library is
// the one whose absence a cudnn64_9.dll alone does not reveal.
var cudaRuntimeLibs = []string{
	"cudart64_12.dll",
	"cublas64_12.dll",
	"cublasLt64_12.dll",
	"cufft64_11.dll",
	"curand64_10.dll",
	"cudnn64_9.dll",
	"cudnn_graph64_9.dll",
}

var cudaProbe struct {
	sync.Once
	ok bool
}

// cudaRuntimeAvailable reports whether every CUDA library the provider needs
// can actually be loaded.
//
// DECISION(2026-08): probe by loading, not by finding files on PATH. The
// provider choice is made before the ONNX runtime is downloaded — the CUDA and
// DirectML builds are different libraries — so there is no second chance inside
// a run: a wrong guess degrades to CPU, which is slower than the DirectML we
// would have picked. Loading catches a half-installed or wrong-architecture
// stack that a file-existence check happily accepts.
// ASSUMES: a library that loads will also attach as an execution provider.
// REVISIT IF: users report auto landing on CPU with CUDA installed.
func cudaRuntimeAvailable() bool {
	cudaProbe.Do(func() {
		for _, name := range cudaRuntimeLibs {
			if err := syscall.NewLazyDLL(name).Load(); err != nil {
				return
			}
		}
		cudaProbe.ok = true
	})
	return cudaProbe.ok
}
