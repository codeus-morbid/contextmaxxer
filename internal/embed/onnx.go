package embed

// #cgo linux LDFLAGS: -L${SRCDIR}/../../libs/linux-amd64 -ltokenizers -ldl -lm -lstdc++
// #cgo darwin LDFLAGS: -L${SRCDIR}/../../libs/darwin -ltokenizers -ldl -lm -lstdc++
// #cgo windows LDFLAGS: -L${SRCDIR}/../../libs/windows -ltokenizers -lm -lstdc++ -lws2_32 -luserenv -lntdll
import "C"

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/daulet/tokenizers"
	ort "github.com/yalue/onnxruntime_go"
)

// DECISION: onnxruntime_go uses a global environment (single OrtEnv per process).
// We guard initialization with a sync.Once so multiple OnnxEmbedder instances don't double-init.
var (
	ortOnce    sync.Once
	ortInitErr error
)

type OnnxEmbedder struct {
	session   *ort.DynamicAdvancedSession
	tokenizer *tokenizers.Tokenizer
	spec      ModelSpec
	batchSize int
	log       *slog.Logger
}

type Config struct {
	ModelName string
	CacheDir  string
	BatchSize int
	Log       *slog.Logger
}

func NewOnnxEmbedder(ctx context.Context, cfg Config) (*OnnxEmbedder, error) {
	if cfg.ModelName == "" {
		cfg.ModelName = "jina-embeddings-v2-base-code"
	}
	if cfg.BatchSize <= 0 {
		// DECISION(2026-07): batch stays small on GPU too — measured on
		// DirectML (512-token docs): batch 128 = 12-13 docs/s, batch 16 =
		// 48.7 docs/s. Large batches blow past the DML working set and
		// stall; the throughput lever is length-bucketing + padding
		// shelves (embedAll/runBatch), not batch size.
		cfg.BatchSize = 32
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	spec, ok := modelSpecs[cfg.ModelName]
	if !ok {
		return nil, fmt.Errorf("unknown model: %s", cfg.ModelName)
	}

	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("get user cache dir: %w", err)
		}
		cacheDir = filepath.Join(base, "contextmaxxer")
	}

	if _, err := EnsureONNXEnvironment(cacheDir, cfg.Log); err != nil {
		return nil, err
	}

	modelPath, tokenizerPath, err := ensureModelFiles(cacheDir, spec, cfg.Log)
	if err != nil {
		return nil, fmt.Errorf("ensure model files: %w", err)
	}

	tk, err := tokenizers.FromFile(tokenizerPath)
	if err != nil {
		return nil, fmt.Errorf("load tokenizer: %w", err)
	}

	sessOpts, err := NewSessionOptionsForProvider(cfg.Log)
	if err != nil {
		tk.Close()
		return nil, err
	}
	if sessOpts != nil {
		defer sessOpts.Destroy()
	}

	// DECISION: DynamicAdvancedSession is used because batch size varies per call.
	// Supported encoder models expose last_hidden_state with model-specific input names.
	session, err := ort.NewDynamicAdvancedSession(
		modelPath,
		spec.InputNames,
		[]string{"last_hidden_state"},
		sessOpts,
	)
	if err != nil {
		tk.Close()
		return nil, fmt.Errorf("create onnx session: %w", err)
	}

	cfg.Log.Info("OnnxEmbedder ready", "model", spec.Name, "dim", spec.Dim)
	return &OnnxEmbedder{
		session:   session,
		tokenizer: tk,
		spec:      spec,
		batchSize: cfg.BatchSize,
		log:       cfg.Log,
	}, nil
}

// NewSessionOptionsForProvider returns session options configured for the
// provider selected via CONTEXTMAXXER_ORT_PROVIDER, or nil for plain CPU.
// Callers own the returned options and must Destroy them after session
// creation. With the default "auto" provider a failed GPU attach degrades to
// CPU with a warning; an explicitly requested provider fails hard.
func NewSessionOptionsForProvider(log *slog.Logger) (*ort.SessionOptions, error) {
	if log == nil {
		log = slog.Default()
	}
	provider := OrtProvider()
	if provider == "cpu" {
		return nil, nil
	}

	opts, err := buildProviderSessionOptions(provider, log)
	if err != nil {
		if ortProviderIsExplicit() {
			return nil, err
		}
		// CPU is slower than the DirectML this machine could have used, and the
		// provider cannot be swapped after the runtime is loaded, so say what to
		// set rather than leaving a quiet downgrade.
		hint := ""
		if provider == "cuda" {
			hint = "set CONTEXTMAXXER_ORT_PROVIDER=directml to use the GPU without CUDA"
		}
		log.Warn("GPU execution provider unavailable, falling back to CPU",
			"provider", provider, "err", err, "hint", hint)
		return nil, nil
	}
	return opts, nil
}

func buildProviderSessionOptions(provider string, log *slog.Logger) (*ort.SessionOptions, error) {
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("new session options: %w", err)
	}

	switch provider {
	case "cuda":
		// CUDA additionally requires cuBLAS 12 and cuDNN 9 to be loadable
		// (system ldconfig or LD_LIBRARY_PATH).
		cudaOpts, err := ort.NewCUDAProviderOptions()
		if err != nil {
			opts.Destroy()
			return nil, fmt.Errorf("new cuda provider options: %w", err)
		}
		defer cudaOpts.Destroy()
		if err := cudaOpts.Update(map[string]string{"device_id": "0"}); err != nil {
			opts.Destroy()
			return nil, fmt.Errorf("update cuda provider options (is cuDNN 9 + cuBLAS 12 installed?): %w", err)
		}
		if err := opts.AppendExecutionProviderCUDA(cudaOpts); err != nil {
			opts.Destroy()
			return nil, fmt.Errorf("append cuda execution provider: %w", err)
		}
		log.Info("ONNX sessions will run on CUDA", "device_id", 0)
	case "directml":
		// DECISION(2026-06): per ORT DirectML EP docs, memory pattern must be
		// disabled and execution sequential when DML is attached.
		if err := opts.SetMemPattern(false); err != nil {
			opts.Destroy()
			return nil, fmt.Errorf("disable mem pattern for directml: %w", err)
		}
		if err := opts.SetExecutionMode(ort.ExecutionModeSequential); err != nil {
			opts.Destroy()
			return nil, fmt.Errorf("set sequential execution for directml: %w", err)
		}
		if err := opts.AppendExecutionProviderDirectML(0); err != nil {
			opts.Destroy()
			return nil, fmt.Errorf("append directml execution provider: %w", err)
		}
		log.Info("ONNX sessions will run on DirectML", "device_id", 0)
	default:
		opts.Destroy()
		return nil, fmt.Errorf("unknown ONNX provider %q", provider)
	}
	return opts, nil
}

func EnsureONNXEnvironment(cacheDir string, log *slog.Logger) (string, error) {
	if log == nil {
		log = slog.Default()
	}
	if cacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("get user cache dir: %w", err)
		}
		cacheDir = filepath.Join(base, "contextmaxxer")
	}

	libPath, err := ensureONNXRuntime(cacheDir, log)
	if err != nil {
		return "", fmt.Errorf("ensure onnxruntime: %w", err)
	}

	ortOnce.Do(func() {
		ort.SetSharedLibraryPath(libPath)
		ortInitErr = ort.InitializeEnvironment()
	})
	if ortInitErr != nil {
		return "", fmt.Errorf("init onnxruntime: %w", ortInitErr)
	}
	return libPath, nil
}

func (e *OnnxEmbedder) Dimension() int { return e.spec.Dim }

func (e *OnnxEmbedder) Close() error {
	e.session.Destroy()
	e.tokenizer.Close()
	return nil
}

// Embed embeds passage/document-side texts (the model's DocPrefix, if any,
// is applied). Query-side texts must go through EmbedQueries so instruction-
// tuned models get their query prefix.
func (e *OnnxEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return e.embedAll(ctx, withPrefix(e.spec.DocPrefix, texts))
}

// EmbedQueries embeds query-side texts, applying the model's query
// instruction prefix (no-op for symmetric encoders like jina v2).
func (e *OnnxEmbedder) EmbedQueries(ctx context.Context, texts []string) ([][]float32, error) {
	return e.embedAll(ctx, withPrefix(e.spec.QueryPrefix, texts))
}

func (e *OnnxEmbedder) embedAll(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	// Length-bucketed batching: every batch is padded to its longest member,
	// so embedding in length order collapses the padding waste of mixed-size
	// symbol corpora (20-500 tokens side by side). Byte length is a good
	// enough proxy for token count. Results are restored to input order.
	order := make([]int, len(texts))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return len(texts[order[a]]) < len(texts[order[b]]) })
	sorted := make([]string, len(texts))
	for i, idx := range order {
		sorted[i] = texts[idx]
	}

	result := make([][]float32, len(texts))
	for start := 0; start < len(sorted); start += e.batchSize {
		end := start + e.batchSize
		if end > len(sorted) {
			end = len(sorted)
		}

		vecs, err := e.embedBatch(ctx, sorted[start:end])
		if err != nil {
			return nil, fmt.Errorf("embed batch [%d:%d]: %w", start, end, err)
		}
		for i, v := range vecs {
			result[order[start+i]] = v
		}
	}
	return result, nil
}

func withPrefix(prefix string, texts []string) []string {
	if prefix == "" {
		return texts
	}
	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = prefix + t
	}
	return out
}

// runBatch tokenizes a batch, runs the encoder, and returns a process-owned
// copy of the per-token hidden states ([batch*seqLen*dim], row-major) together
// with the flattened attention mask. Splitting this out lets both the pooled
// production path (embedBatch) and the experimental late-interaction path
// (EmbedTokens) share one model invocation.
func (e *OnnxEmbedder) runBatch(texts []string) (hidden []float32, attMask []int64, seqLen, batchSize int, err error) {
	batchSize = len(texts)
	maxLen := e.spec.MaxTokens

	type encoded struct {
		ids  []int64
		mask []int64
		tt   []int64
	}
	encs := make([]encoded, batchSize)

	for i, text := range texts {
		enc := e.tokenizer.EncodeWithOptions(text, true,
			tokenizers.WithReturnAttentionMask(),
			tokenizers.WithReturnTypeIDs(),
		)
		ids := toInt64(enc.IDs)
		mask := toInt64(enc.AttentionMask)
		tt := toInt64(enc.TypeIDs)

		// truncate to maxLen
		if len(ids) > maxLen {
			ids = ids[:maxLen]
			mask = mask[:maxLen]
			tt = tt[:maxLen]
		}

		// Last-token-pooling models are trained to summarize into the EOS
		// token; it must be present and must survive truncation.
		if eos := e.spec.EOSTokenID; eos > 0 {
			if len(ids) == 0 || ids[len(ids)-1] != eos {
				if len(ids) < maxLen {
					ids = append(ids, eos)
					mask = append(mask, 1)
					tt = append(tt, 0)
				} else {
					ids[len(ids)-1] = eos
				}
			}
		}

		if len(ids) > seqLen {
			seqLen = len(ids)
		}
		encs[i] = encoded{ids: ids, mask: mask, tt: tt}
	}

	if seqLen == 0 {
		seqLen = 1
	}

	// Quantize the padded length to a few fixed shelves. DirectML (and CUDA
	// graph capture) compile a kernel per (batch, seqLen) shape — with
	// length-bucketed batches every batch would otherwise present a NEW shape
	// and recompile, which measured 2x SLOWER than no bucketing at all.
	// A handful of shelves keeps shape reuse AND most of the padding win.
	for _, shelf := range []int{64, 128, 256, 384} {
		if seqLen <= shelf {
			seqLen = shelf
			break
		}
	}
	if seqLen > 384 {
		seqLen = e.spec.MaxTokens
	}

	flatSize := batchSize * seqLen
	inputIDs := make([]int64, flatSize)
	attMask = make([]int64, flatSize)
	typeIDs := make([]int64, flatSize)

	for i, enc := range encs {
		base := i * seqLen
		copy(inputIDs[base:], enc.ids)
		copy(attMask[base:], enc.mask)
		copy(typeIDs[base:], enc.tt)
	}

	shape := ort.NewShape(int64(batchSize), int64(seqLen))

	tInputIDs, err := ort.NewTensor(shape, inputIDs)
	if err != nil {
		return nil, nil, 0, 0, fmt.Errorf("new input_ids tensor: %w", err)
	}
	defer tInputIDs.Destroy()

	tAttMask, err := ort.NewTensor(shape, attMask)
	if err != nil {
		return nil, nil, 0, 0, fmt.Errorf("new attention_mask tensor: %w", err)
	}
	defer tAttMask.Destroy()

	tTypeIDs, err := ort.NewTensor(shape, typeIDs)
	if err != nil {
		return nil, nil, 0, 0, fmt.Errorf("new token_type_ids tensor: %w", err)
	}
	defer tTypeIDs.Destroy()

	var tPosIDs *ort.Tensor[int64]
	if e.usesPositionIDs() {
		// Right padding + causal attention: pad positions can't influence real
		// tokens, so a plain 0..seqLen-1 ramp per row is correct.
		posIDs := make([]int64, flatSize)
		for b := 0; b < batchSize; b++ {
			for s := 0; s < seqLen; s++ {
				posIDs[b*seqLen+s] = int64(s)
			}
		}
		tPosIDs, err = ort.NewTensor(shape, posIDs)
		if err != nil {
			return nil, nil, 0, 0, fmt.Errorf("new position_ids tensor: %w", err)
		}
		defer tPosIDs.Destroy()
	}

	// output: [batch, seq_len, hidden]
	outShape := ort.NewShape(int64(batchSize), int64(seqLen), int64(e.spec.Dim))
	outData := make([]float32, batchSize*seqLen*e.spec.Dim)
	tOutput, err := ort.NewTensor(outShape, outData)
	if err != nil {
		return nil, nil, 0, 0, fmt.Errorf("new output tensor: %w", err)
	}
	defer tOutput.Destroy()

	inputs := []ort.Value{tInputIDs, tAttMask}
	if e.usesTokenTypeIDs() {
		inputs = append(inputs, tTypeIDs)
	}
	if tPosIDs != nil {
		inputs = append(inputs, tPosIDs)
	}

	if err := e.session.Run(inputs, []ort.Value{tOutput}); err != nil {
		return nil, nil, 0, 0, fmt.Errorf("onnx run: %w", err)
	}

	// GetData aliases the tensor's backing store, which is freed on Destroy;
	// copy into a Go-owned slice before returning.
	raw := tOutput.GetData()
	hidden = make([]float32, len(raw))
	copy(hidden, raw)
	return hidden, attMask, seqLen, batchSize, nil
}

func (e *OnnxEmbedder) embedBatch(_ context.Context, texts []string) ([][]float32, error) {
	hidden, attMask, seqLen, batchSize, err := e.runBatch(texts)
	if err != nil {
		return nil, err
	}

	result := make([][]float32, batchSize)
	for b := 0; b < batchSize; b++ {
		vec := make([]float32, e.spec.Dim)
		if e.spec.Pooling == "last" {
			// Last-token pooling: the trained summary position is the final
			// non-padding token (EOS, enforced in runBatch).
			for s := seqLen - 1; s >= 0; s-- {
				if attMask[b*seqLen+s] == 1 {
					base := (b*seqLen + s) * e.spec.Dim
					copy(vec, hidden[base:base+e.spec.Dim])
					break
				}
			}
		} else {
			// DECISION: mean-pool over non-padding tokens using attention mask,
			// then L2-normalize — required for cosine similarity to work correctly.
			var maskSum float32
			for s := 0; s < seqLen; s++ {
				if attMask[b*seqLen+s] == 0 {
					continue
				}
				maskSum++
				base := (b*seqLen + s) * e.spec.Dim
				for d := 0; d < e.spec.Dim; d++ {
					vec[d] += hidden[base+d]
				}
			}
			if maskSum > 0 {
				for d := range vec {
					vec[d] /= maskSum
				}
			}
		}
		l2Normalize(vec)
		result[b] = vec
	}
	return result, nil
}

// EmbedTokens returns the per-token, L2-normalized hidden states for each input
// text (padding tokens excluded). This is the multi-vector representation used
// by late-interaction (ColBERT-style MaxSim) experiments; the production
// retrieval path uses Embed, which mean-pools to a single vector. Kept separate
// so the index format and hot path are unaffected.
func (e *OnnxEmbedder) EmbedTokens(ctx context.Context, texts []string) ([][][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][][]float32, len(texts))
	for start := 0; start < len(texts); start += e.batchSize {
		end := start + e.batchSize
		if end > len(texts) {
			end = len(texts)
		}
		hidden, attMask, seqLen, batchSize, err := e.runBatch(texts[start:end])
		if err != nil {
			return nil, fmt.Errorf("embed tokens batch [%d:%d]: %w", start, end, err)
		}
		for b := 0; b < batchSize; b++ {
			var toks [][]float32
			for s := 0; s < seqLen; s++ {
				if attMask[b*seqLen+s] == 0 {
					continue
				}
				base := (b*seqLen + s) * e.spec.Dim
				tok := make([]float32, e.spec.Dim)
				copy(tok, hidden[base:base+e.spec.Dim])
				l2Normalize(tok)
				toks = append(toks, tok)
			}
			out[start+b] = toks
		}
	}
	return out, nil
}

func (e *OnnxEmbedder) usesTokenTypeIDs() bool {
	for _, name := range e.spec.InputNames {
		if name == "token_type_ids" {
			return true
		}
	}
	return false
}

func (e *OnnxEmbedder) usesPositionIDs() bool {
	for _, name := range e.spec.InputNames {
		if name == "position_ids" {
			return true
		}
	}
	return false
}

func toInt64(u []uint32) []int64 {
	out := make([]int64, len(u))
	for i, v := range u {
		out[i] = int64(v)
	}
	return out
}

func l2Normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := float32(math.Sqrt(sum))
	if norm < 1e-9 {
		return
	}
	for i := range v {
		v[i] /= norm
	}
}
