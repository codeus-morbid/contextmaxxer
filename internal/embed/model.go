package embed

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DECISION: pinned to 1.25.0 which exposes ORT API v25, matching onnxruntime_go@v1.28.0 header requirement.
const onnxRuntimeVersion = "1.25.0"

type ModelSpec struct {
	Name         string
	Dim          int
	MaxTokens    int
	ModelURL     string
	TokenizerURL string
	// ModelSHA256/TokenizerSHA256 pin the expected content of the downloads.
	// DECISION(2026-07): URLs point at immutable HF revision commits and the
	// hash is verified after download — /main/ URLs plus a merely-logged
	// checksum meant a silent upstream change (or truncated download) would
	// be trusted. Empty = unpinned (legacy specs without a known-good local
	// copy to hash).
	ModelSHA256     string
	TokenizerSHA256 string
	InputNames      []string
	// Pooling collapses per-token states to one vector: "mean" (default,
	// encoder models) or "last" (decoder-based embedding models).
	Pooling string
	// QueryPrefix/DocPrefix are prepended to query/passage texts for
	// instruction-tuned models; empty for symmetric encoders.
	QueryPrefix string
	DocPrefix   string
	// EOSTokenID, when >0, is enforced as the final token of every sequence
	// (required for last-token pooling to land on the trained position, and
	// must survive truncation).
	EOSTokenID int64
}

var bgeSmallEnV15 = ModelSpec{
	Name:         "bge-small-en-v1.5",
	Dim:          384,
	MaxTokens:    512,
	ModelURL:     "https://huggingface.co/BAAI/bge-small-en-v1.5/resolve/main/onnx/model.onnx",
	TokenizerURL: "https://huggingface.co/BAAI/bge-small-en-v1.5/resolve/main/tokenizer.json",
	InputNames:   []string{"input_ids", "attention_mask", "token_type_ids"},
}

// DECISION: jina-v2-base-code supports 8192 tokens via ALiBi, but we cap batch processing
// at 512 tokens for memory efficiency. No query/passage prefixes needed (that's v3 only).
// ONNX model is in /onnx/ subdir (642 MB full precision); uses mean pooling + L2 norm same as bge.
var jinaEmbeddingsV2BaseCode = ModelSpec{
	Name:            "jina-embeddings-v2-base-code",
	Dim:             768,
	MaxTokens:       512,
	ModelURL:        "https://huggingface.co/jinaai/jina-embeddings-v2-base-code/resolve/516f4baf13dec4ddddda8631e019b5737c8bc250/onnx/model.onnx",
	TokenizerURL:    "https://huggingface.co/jinaai/jina-embeddings-v2-base-code/resolve/516f4baf13dec4ddddda8631e019b5737c8bc250/tokenizer.json",
	ModelSHA256:     "63363fc178428b74620c6f3780cbc7191883fa5c7f84c0945c45eb5c4256733b",
	TokenizerSHA256: "b01c78a902aa4facb2f47f95449f48e2f7bbfea5d2472ee2f6ce92323c6f86e5",
	InputNames:      []string{"input_ids", "attention_mask"},
}

// DECISION(2026-07): Qwen3-Embedding-0.6B (int8 TEI export) was evaluated as
// a replacement for jina-v2-base-code and REJECTED — the corebench A/B (see
// BENCHMARK.md) measured it worse on quality on both test repos and 12-28x
// slower to index locally. The spec was removed; the Pooling/QueryPrefix/
// EOSTokenID fields above and cmd/corebench+cmd/embprobe remain as the
// evaluation kit for future candidates (e.g. a small code-trained encoder).
// jina-code-embeddings 0.5b/1.5b score higher publicly but are CC-BY-NC.

// jina-v2-code-ft is the locally fine-tuned jina-v2-base-code (CORE-Bench
// issue->edit triplets; training pipeline in _bench/corebench-full/ft, eval
// story in BENCHMARK.md). No download URLs on purpose: the ONNX export is
// produced locally and placed into <cache>/models/jina-v2-code-ft/ by the
// training workflow; ensureModelFiles errors with instructions if absent.
var jinaV2CodeFT = ModelSpec{
	Name:      "jina-v2-code-ft",
	Dim:       768,
	MaxTokens: 512,
	// The optimum export includes token_type_ids (all zeros), unlike the
	// upstream jina ONNX which folded them away.
	InputNames: []string{"input_ids", "attention_mask", "token_type_ids"},
}

// jina-v2-code-ft2 is round 2 of the fine-tune: trained ONLY on
// SWE-Bench-plus-plus (zero repo overlap with the 253-repo eval set), so the
// FULL CORE-Bench L2 evaluation of this model is contamination-free.
var jinaV2CodeFT2 = ModelSpec{
	Name:       "jina-v2-code-ft2",
	Dim:        768,
	MaxTokens:  512,
	InputNames: []string{"input_ids", "attention_mask", "token_type_ids"},
}

// jina-v2-code-ft3 is round 3: plus-plus triplets plus Rewrite-LEVEL-2
// paraphrased queries. Experimental — stays out of user-facing defaults
// unless it beats ft2 on the full eval.
var jinaV2CodeFT3 = ModelSpec{
	Name:       "jina-v2-code-ft3",
	Dim:        768,
	MaxTokens:  512,
	InputNames: []string{"input_ids", "attention_mask", "token_type_ids"},
}

var modelSpecs = map[string]ModelSpec{
	"bge-small-en-v1.5":            bgeSmallEnV15,
	"jina-embeddings-v2-base-code": jinaEmbeddingsV2BaseCode,
	"jina-v2-code-ft":              jinaV2CodeFT,
	"jina-v2-code-ft2":             jinaV2CodeFT2,
	"jina-v2-code-ft3":             jinaV2CodeFT3,
}

func GetModel(name string) (ModelSpec, error) {
	spec, ok := modelSpecs[name]
	if !ok {
		return ModelSpec{}, fmt.Errorf("unknown model %q; available: bge-small-en-v1.5, jina-embeddings-v2-base-code", name)
	}
	return spec, nil
}

func EnsureModelFiles(cacheDir string, spec ModelSpec, log *slog.Logger) (modelPath, tokenizerPath string, err error) {
	return ensureModelFiles(cacheDir, spec, log)
}

// OrtProvider resolves the ONNX Runtime execution provider from the
// CONTEXTMAXXER_ORT_PROVIDER env (cpu|cuda|directml|auto, default auto).
// DECISION(2026-06): auto = DirectML on windows/amd64 (works on any DX12 GPU
// with no extra installs; the DML runtime build still contains the CPU EP, so
// a failed DML attach falls back to CPU at session creation), plain CPU
// elsewhere. CUDA stays explicit-only: it needs system cuBLAS/cuDNN and a
// 250MB runtime download, too heavy to trigger silently.
func OrtProvider() string {
	p := strings.ToLower(os.Getenv("CONTEXTMAXXER_ORT_PROVIDER"))
	switch p {
	case "cuda", "directml", "cpu":
		return p
	}
	// auto (default)
	if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" {
		return "directml"
	}
	return "cpu"
}

// ortProviderIsExplicit reports whether the provider was forced via env
// (failures are then hard errors instead of silent CPU fallback).
func ortProviderIsExplicit() bool {
	switch strings.ToLower(os.Getenv("CONTEXTMAXXER_ORT_PROVIDER")) {
	case "cuda", "directml", "cpu":
		return true
	}
	return false
}

// directMLVersion pins the Microsoft.AI.DirectML redistributable compatible
// with the pinned onnxruntime build.
const directMLVersion = "1.15.4"

// directMLOrtVersion pins the Microsoft.ML.OnnxRuntime.DirectML package,
// which trails the main onnxruntime release line.
const directMLOrtVersion = "1.24.4"

// onnxCompanionDownload is an extra archive whose single file must land next
// to the main runtime library (e.g. DirectML.dll).
type onnxCompanionDownload struct {
	url     string
	zipPath string
	outName string
}

type onnxRuntimePlatform struct {
	url     string
	libName string
	// zipPath, when set, selects the archive entry by full path suffix instead
	// of by basename (NuGet packages carry the same dll for several arches).
	zipPath  string
	platform string
	// extraLibs are additional shared libraries (execution providers) that
	// must be extracted next to the main library.
	extraLibs  []string
	companions []onnxCompanionDownload
}

func onnxRuntimePlatformInfo() (onnxRuntimePlatform, error) {
	ver := onnxRuntimeVersion
	if OrtProvider() == "cuda" {
		switch {
		case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
			return onnxRuntimePlatform{
				url:      fmt.Sprintf("https://github.com/microsoft/onnxruntime/releases/download/v%s/onnxruntime-linux-x64-gpu-%s.tgz", ver, ver),
				libName:  "libonnxruntime.so." + ver,
				platform: "linux-x64-gpu",
				extraLibs: []string{
					"libonnxruntime_providers_shared.so",
					"libonnxruntime_providers_cuda.so",
				},
			}, nil
		case runtime.GOOS == "windows" && runtime.GOARCH == "amd64":
			// DECISION(2026-07): NVIDIA-on-Windows users were stuck with the
			// slower DirectML path; CUDA stays explicit-only (needs system
			// CUDA 12 + cuDNN 9 on PATH) but is now available.
			return onnxRuntimePlatform{
				url:      fmt.Sprintf("https://github.com/microsoft/onnxruntime/releases/download/v%s/onnxruntime-win-x64-gpu-%s.zip", ver, ver),
				libName:  "onnxruntime.dll",
				platform: "win-x64-gpu",
				extraLibs: []string{
					"onnxruntime_providers_shared.dll",
					"onnxruntime_providers_cuda.dll",
				},
			}, nil
		default:
			return onnxRuntimePlatform{}, fmt.Errorf("CUDA provider is only supported on linux/amd64 and windows/amd64 (got %s/%s)", runtime.GOOS, runtime.GOARCH)
		}
	}
	switch {
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		return onnxRuntimePlatform{
			url:      fmt.Sprintf("https://github.com/microsoft/onnxruntime/releases/download/v%s/onnxruntime-linux-x64-%s.tgz", ver, ver),
			libName:  "libonnxruntime.so." + ver,
			platform: "linux-x64",
		}, nil
	case runtime.GOOS == "linux" && runtime.GOARCH == "arm64":
		return onnxRuntimePlatform{
			url:      fmt.Sprintf("https://github.com/microsoft/onnxruntime/releases/download/v%s/onnxruntime-linux-aarch64-%s.tgz", ver, ver),
			libName:  "libonnxruntime.so." + ver,
			platform: "linux-aarch64",
		}, nil
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return onnxRuntimePlatform{
			url:      fmt.Sprintf("https://github.com/microsoft/onnxruntime/releases/download/v%s/onnxruntime-osx-arm64-%s.tgz", ver, ver),
			libName:  "libonnxruntime." + ver + ".dylib",
			platform: "osx-arm64",
		}, nil
	case runtime.GOOS == "darwin" && runtime.GOARCH == "amd64":
		return onnxRuntimePlatform{
			url:      fmt.Sprintf("https://github.com/microsoft/onnxruntime/releases/download/v%s/onnxruntime-osx-x86_64-%s.tgz", ver, ver),
			libName:  "libonnxruntime." + ver + ".dylib",
			platform: "osx-x86_64",
		}, nil
	case runtime.GOOS == "windows" && runtime.GOARCH == "amd64":
		if OrtProvider() == "directml" {
			// DECISION(2026-06): the DML-enabled onnxruntime.dll ships only via
			// the Microsoft.ML.OnnxRuntime.DirectML NuGet package (the mainline
			// GitHub zip does NOT compile the DML EP in — verified empirically:
			// AppendExecutionProviderDirectML returns "provider is not supported").
			// The DML package trails the main release line (1.24.4 vs 1.25), which
			// requires the Go binding pinned to ORT_API_VERSION<=24
			// (onnxruntime_go v1.27.0). DirectML.dll itself comes from
			// Microsoft.AI.DirectML and is placed next to the runtime so the
			// delay-load resolves to the pinned version, not the inbox System32 one.
			return onnxRuntimePlatform{
				url:      fmt.Sprintf("https://api.nuget.org/v3-flatcontainer/microsoft.ml.onnxruntime.directml/%s/microsoft.ml.onnxruntime.directml.%s.nupkg", directMLOrtVersion, directMLOrtVersion),
				zipPath:  "runtimes/win-x64/native/onnxruntime.dll",
				libName:  "onnxruntime.dll",
				platform: "win-x64-directml-" + directMLOrtVersion,
				companions: []onnxCompanionDownload{{
					url:     "https://api.nuget.org/v3-flatcontainer/microsoft.ai.directml/" + directMLVersion + "/microsoft.ai.directml." + directMLVersion + ".nupkg",
					zipPath: "bin/x64-win/DirectML.dll",
					outName: "DirectML.dll",
				}},
			}, nil
		}
		return onnxRuntimePlatform{
			url:      fmt.Sprintf("https://github.com/microsoft/onnxruntime/releases/download/v%s/onnxruntime-win-x64-%s.zip", ver, ver),
			libName:  "onnxruntime.dll",
			platform: "win-x64",
		}, nil
	default:
		return onnxRuntimePlatform{}, fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

func ensureONNXRuntime(cacheDir string, log *slog.Logger) (string, error) {
	info, err := onnxRuntimePlatformInfo()
	if err != nil {
		return "", err
	}

	dir := filepath.Join(cacheDir, "onnxruntime", onnxRuntimeVersion, info.platform)
	libPath := filepath.Join(dir, info.libName)

	libOK := false
	if _, err := os.Stat(libPath); err == nil {
		libOK = true
	}
	companionsOK := true
	for _, comp := range info.companions {
		if _, err := os.Stat(filepath.Join(dir, comp.outName)); err != nil {
			companionsOK = false
		}
	}
	if libOK && companionsOK {
		return libPath, nil
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create onnxruntime cache dir: %w", err)
	}

	if libOK {
		// Main library cached; only companions are missing (e.g. provider was
		// switched to directml after a plain CPU run populated the cache).
		for _, comp := range info.companions {
			if _, err := os.Stat(filepath.Join(dir, comp.outName)); err == nil {
				continue
			}
			compArchive := filepath.Join(dir, "companion.archive")
			log.Info("downloading ONNX Runtime companion", "url", comp.url)
			if err := downloadFileWithProgress(comp.url, compArchive, "", log); err != nil {
				return "", fmt.Errorf("download companion %s: %w", comp.outName, err)
			}
			err := extractFromZipPath(compArchive, dir, comp.zipPath, comp.outName)
			os.Remove(compArchive)
			if err != nil {
				return "", fmt.Errorf("extract companion %s: %w", comp.outName, err)
			}
		}
		return libPath, nil
	}

	log.Info("downloading ONNX Runtime", "version", onnxRuntimeVersion, "platform", info.platform, "url", info.url)

	archivePath := filepath.Join(dir, "onnxruntime.archive")
	if err := downloadFileWithProgress(info.url, archivePath, "", log); err != nil {
		return "", fmt.Errorf("download onnxruntime: %w", err)
	}
	defer os.Remove(archivePath)

	isZip := strings.HasSuffix(info.url, ".zip") || info.zipPath != ""
	if info.zipPath != "" {
		if err := extractFromZipPath(archivePath, dir, info.zipPath, info.libName); err != nil {
			return "", fmt.Errorf("extract onnxruntime package (%s): %w", info.zipPath, err)
		}
	} else {
		wanted := append([]string{info.libName}, info.extraLibs...)
		for _, name := range wanted {
			if isZip {
				if err := extractFromZip(archivePath, dir, name); err != nil {
					return "", fmt.Errorf("extract onnxruntime zip (%s): %w", name, err)
				}
			} else {
				if err := extractFromTar(archivePath, dir, name); err != nil {
					return "", fmt.Errorf("extract onnxruntime tgz (%s): %w", name, err)
				}
			}
		}
	}

	for _, comp := range info.companions {
		compArchive := filepath.Join(dir, "companion.archive")
		log.Info("downloading ONNX Runtime companion", "url", comp.url)
		if err := downloadFileWithProgress(comp.url, compArchive, "", log); err != nil {
			return "", fmt.Errorf("download companion %s: %w", comp.outName, err)
		}
		err := extractFromZipPath(compArchive, dir, comp.zipPath, comp.outName)
		os.Remove(compArchive)
		if err != nil {
			return "", fmt.Errorf("extract companion %s: %w", comp.outName, err)
		}
	}

	if _, err := os.Stat(libPath); err != nil {
		return "", fmt.Errorf("onnxruntime lib not found after extraction: %s", libPath)
	}
	log.Info("ONNX Runtime ready", "path", libPath)
	return libPath, nil
}

func ensureModelFiles(cacheDir string, spec ModelSpec, log *slog.Logger) (modelPath, tokenizerPath string, err error) {
	modelDir := filepath.Join(cacheDir, "models", spec.Name)
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		return "", "", fmt.Errorf("create model cache dir: %w", err)
	}

	modelPath = filepath.Join(modelDir, "model.onnx")
	tokenizerPath = filepath.Join(modelDir, "tokenizer.json")

	if _, err := os.Stat(modelPath); err != nil {
		if spec.ModelURL == "" {
			return "", "", fmt.Errorf("local model %q: place model.onnx and tokenizer.json into %s (produced by the local training/export pipeline)", spec.Name, modelDir)
		}
		log.Info("downloading model", "name", spec.Name, "url", spec.ModelURL)
		if err := downloadFileWithProgress(spec.ModelURL, modelPath, spec.ModelSHA256, log); err != nil {
			return "", "", fmt.Errorf("download model: %w", err)
		}
	}

	if _, err := os.Stat(tokenizerPath); err != nil {
		if spec.TokenizerURL == "" {
			return "", "", fmt.Errorf("local model %q: place tokenizer.json into %s", spec.Name, modelDir)
		}
		log.Info("downloading tokenizer", "name", spec.Name, "url", spec.TokenizerURL)
		if err := downloadFileWithProgress(spec.TokenizerURL, tokenizerPath, spec.TokenizerSHA256, log); err != nil {
			return "", "", fmt.Errorf("download tokenizer: %w", err)
		}
	}

	return modelPath, tokenizerPath, nil
}

func downloadFileWithProgress(url, dest, expectedSHA256 string, log *slog.Logger) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("http get %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d for %s", resp.StatusCode, url)
	}

	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create tmp file: %w", err)
	}

	total := resp.ContentLength
	var downloaded int64
	const logEvery = 10 * 1024 * 1024

	h := sha256.New()
	buf := make([]byte, 32*1024)
	nextLog := int64(logEvery)

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(tmp)
				return fmt.Errorf("write: %w", werr)
			}
			h.Write(buf[:n])
			downloaded += int64(n)
			if downloaded >= nextLog {
				if total > 0 {
					log.Info("download progress", "downloaded_mb", downloaded/1024/1024, "total_mb", total/1024/1024)
				} else {
					log.Info("download progress", "downloaded_mb", downloaded/1024/1024)
				}
				nextLog = downloaded + logEvery
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("read response: %w", err)
		}
	}
	f.Close()

	checksum := hex.EncodeToString(h.Sum(nil))
	if expectedSHA256 != "" && checksum != expectedSHA256 {
		os.Remove(tmp)
		return fmt.Errorf("download %s: sha256 mismatch (got %s, want %s) — upstream file changed or download corrupted; refusing to install", url, checksum, expectedSHA256)
	}
	log.Info("download complete", "sha256", checksum[:16]+"...", "bytes", downloaded)

	return os.Rename(tmp, dest)
}

func extractFromTar(archivePath, destDir, targetFileName string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		base := filepath.Base(hdr.Name)
		if base == targetFileName && hdr.Typeflag == tar.TypeReg {
			out, err := os.OpenFile(filepath.Join(destDir, base), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			out.Close()
			return err
		}
	}
	return fmt.Errorf("file %s not found in archive", targetFileName)
}

// extractFromZipPath extracts the archive entry whose path ends with
// pathSuffix into destDir under outName. Needed for NuGet packages where the
// same file name exists for several architectures.
func extractFromZipPath(archivePath, destDir, pathSuffix, outName string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		if strings.HasSuffix(strings.ReplaceAll(f.Name, "\\", "/"), pathSuffix) {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			out, err := os.OpenFile(filepath.Join(destDir, outName), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err != nil {
				rc.Close()
				return err
			}
			_, err = io.Copy(out, rc)
			rc.Close()
			out.Close()
			return err
		}
	}
	return fmt.Errorf("entry %s not found in %s", pathSuffix, archivePath)
}

func extractFromZip(archivePath, destDir, targetFileName string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		if filepath.Base(f.Name) == targetFileName {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			out, err := os.OpenFile(filepath.Join(destDir, targetFileName), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err != nil {
				rc.Close()
				return err
			}
			_, err = io.Copy(out, rc)
			rc.Close()
			out.Close()
			return err
		}
	}
	return fmt.Errorf("file %s not found in zip", targetFileName)
}
