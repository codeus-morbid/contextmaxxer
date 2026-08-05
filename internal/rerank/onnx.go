package rerank

// #cgo linux LDFLAGS: -L${SRCDIR}/../../libs/linux-amd64 -ltokenizers -ldl -lm -lstdc++
// #cgo darwin LDFLAGS: -L${SRCDIR}/../../libs/darwin -ltokenizers -ldl -lm -lstdc++
// #cgo windows LDFLAGS: -L${SRCDIR}/../../libs/windows -ltokenizers -lm -lstdc++ -lws2_32 -luserenv -lntdll
import "C"

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daulet/tokenizers"
	ort "github.com/yalue/onnxruntime_go"

	"github.com/codeus-morbid/contextmaxxer/internal/embed"
	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
)

const (
	NoneName                          = "none"
	JinaRerankerV2BaseMultilingual    = "jina-reranker-v2-base-multilingual"
	JinaRerankerV1TinyEN              = "jina-reranker-v1-tiny-en"
	MxbaiRerankXsmallV1               = "mxbai-rerank-xsmall-v1"
	defaultRerankerMaxTokens          = 1024
	defaultRerankerMaxQueryTokens     = 256
	defaultRerankerBatchSize          = 8
	defaultRerankerBodyChars          = 1600
	jinaRerankerV2BaseMultilingualURL = "https://huggingface.co/jinaai/jina-reranker-v2-base-multilingual/resolve/main/onnx/model_int8.onnx"
	jinaRerankerV2TokenizerURL        = "https://huggingface.co/jinaai/jina-reranker-v2-base-multilingual/resolve/main/tokenizer.json"
	// Revision-pinned + hash-verified (see embed.ModelSpec SHA fields); the
	// default reranker must not trust a mutable /main/ URL.
	jinaRerankerV1TinyENURL          = "https://huggingface.co/jinaai/jina-reranker-v1-tiny-en/resolve/aca45de6945b5dc6399abcd2a9c55ded5dc9111f/onnx/model.onnx"
	jinaRerankerV1TinyENTokenizerURL = "https://huggingface.co/jinaai/jina-reranker-v1-tiny-en/resolve/aca45de6945b5dc6399abcd2a9c55ded5dc9111f/tokenizer.json"
	jinaRerankerV1TinyENSHA256       = "e0e743251c7566e2b1e4f5ad091c681a700d7d7a3d85541ea56ca3acf43d1afa"
	jinaRerankerV1TinyENTokSHA256    = "0046da43cc8c424b317f56b092b0512aaaa65c4f925d2f16af9d9eeb4d0ef902"
	mxbaiRerankXsmallV1URL           = "https://huggingface.co/mixedbread-ai/mxbai-rerank-xsmall-v1/resolve/main/onnx/model.onnx"
	mxbaiRerankXsmallV1TokenizerURL  = "https://huggingface.co/mixedbread-ai/mxbai-rerank-xsmall-v1/resolve/main/tokenizer.json"
)

type Config struct {
	ModelName      string
	CacheDir       string
	BatchSize      int
	MaxTokens      int
	MaxQueryTokens int
	Log            *slog.Logger
}

func New(ctx context.Context, cfg Config) (retrieve.Reranker, io.Closer, error) {
	if cfg.ModelName == "" || cfg.ModelName == NoneName {
		return nil, nil, nil
	}
	r, err := NewOnnxReranker(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return r, r, nil
}

type modelSpec struct {
	modelURL     string
	tokenizerURL string
	modelSHA256  string
	tokSHA256    string
	useBERTPair  bool
	// tokenTypeIDs: the ONNX graph requires the token_type_ids input (optimum
	// exports of fine-tuned checkpoints keep it; upstream jina exports fold it
	// away). Jina-tiny uses a RobertaTokenizer which emits ALL-ZERO segment
	// ids — training never saw segment 1, so zeros are the ONLY faithful
	// input. Feeding BERT-style 0/1 segments tanked NDCG@10 3x (2026-07).
	tokenTypeIDs bool
	// maxTokens caps pair length for models whose exported graph bakes the
	// ALiBi bias at a fixed size (traced at config.max_position_embeddings);
	// longer sequences crash the broadcast at inference.
	maxTokens int
}

// JinaTinyFT is the locally fine-tuned jina-reranker-v1-tiny-en (CORE-Bench
// issue->edit pairs; no download URLs — produced by the local ft pipeline).
const JinaTinyFT = "jina-tiny-ft"

func specForModel(name string) (modelSpec, error) {
	switch name {
	case JinaTinyFT:
		// Exported with max_position_embeddings=512 (the full-8192 ALiBi
		// trace blows the 2GiB protobuf limit), so pairs must not exceed it.
		// Roberta pair format (<s>q</s></s>d</s>), like the upstream tiny.
		return modelSpec{useBERTPair: false, tokenTypeIDs: true, maxTokens: 512}, nil
	case JinaRerankerV2BaseMultilingual:
		return modelSpec{
			modelURL:     jinaRerankerV2BaseMultilingualURL,
			tokenizerURL: jinaRerankerV2TokenizerURL,
			useBERTPair:  false,
		}, nil
	case JinaRerankerV1TinyEN:
		// DECISION(2026-07): jina-v1-tiny is JinaBert but ships a
		// RobertaTokenizer, so the HF pair format is <s>q</s></s>d</s>
		// (double separator), NOT the BERT [CLS] q [SEP] d [SEP] this spec
		// used before. The old single-sep encoding still ranked usefully
		// (holdout Hit@1 0.79) but was unfaithful to training. REVISIT IF:
		// rerank quality regresses on the agent A/B after this change.
		return modelSpec{
			modelURL:     jinaRerankerV1TinyENURL,
			tokenizerURL: jinaRerankerV1TinyENTokenizerURL,
			modelSHA256:  jinaRerankerV1TinyENSHA256,
			tokSHA256:    jinaRerankerV1TinyENTokSHA256,
			useBERTPair:  false,
		}, nil
	case MxbaiRerankXsmallV1:
		// DECISION: mxbai-rerank-xsmall ships a DebertaV2Tokenizer ([CLS]/
		// [SEP]), so the single-sep BERT pair encoding IS its native format
		// (verified against HF tokenizer_config 2026-07). Output is logits
		// [batch,1], same shape as jina-v2.
		return modelSpec{
			modelURL:     mxbaiRerankXsmallV1URL,
			tokenizerURL: mxbaiRerankXsmallV1TokenizerURL,
			useBERTPair:  true,
		}, nil
	default:
		return modelSpec{}, fmt.Errorf("unknown reranker %q; available: none, %s, %s, %s, %s",
			name, JinaRerankerV2BaseMultilingual, JinaRerankerV1TinyEN, MxbaiRerankXsmallV1, JinaTinyFT)
	}
}

type OnnxReranker struct {
	session        *ort.DynamicAdvancedSession
	tokenizer      *tokenizers.Tokenizer
	batchSize      int
	maxTokens      int
	maxQueryTokens int
	useBERTPair    bool
	tokenTypeIDs   bool
	log            *slog.Logger
}

func NewOnnxReranker(_ context.Context, cfg Config) (*OnnxReranker, error) {
	// DECISION: tiny is the default. Holdout ablation showed full-tiny matches
	// full-jinav2 on Hit@1=0.79 and R@5=1.00 at 3.7x lower latency
	// (720ms p95 vs 2707ms p95). NDCG@5 trades 0.02 (0.889 vs 0.905) — within noise.
	if cfg.ModelName == "" {
		cfg.ModelName = JinaRerankerV1TinyEN
	}
	spec, err := specForModel(cfg.ModelName)
	if err != nil {
		return nil, err
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultRerankerBatchSize
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = defaultRerankerMaxTokens
	}
	if spec.maxTokens > 0 && cfg.MaxTokens > spec.maxTokens {
		cfg.MaxTokens = spec.maxTokens
	}
	if cfg.MaxQueryTokens <= 0 {
		cfg.MaxQueryTokens = defaultRerankerMaxQueryTokens
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("get user cache dir: %w", err)
		}
		cacheDir = filepath.Join(base, "contextmaxxer")
	}

	if _, err := embed.EnsureONNXEnvironment(cacheDir, cfg.Log); err != nil {
		return nil, err
	}

	modelPath, tokenizerPath, err := embed.EnsureModelFiles(cacheDir, embed.ModelSpec{
		Name:            cfg.ModelName,
		Dim:             1,
		MaxTokens:       cfg.MaxTokens,
		ModelURL:        spec.modelURL,
		TokenizerURL:    spec.tokenizerURL,
		ModelSHA256:     spec.modelSHA256,
		TokenizerSHA256: spec.tokSHA256,
		InputNames:      []string{"input_ids", "attention_mask"},
	}, cfg.Log)
	if err != nil {
		return nil, fmt.Errorf("ensure reranker files: %w", err)
	}

	tk, err := tokenizers.FromFile(tokenizerPath)
	if err != nil {
		return nil, fmt.Errorf("load reranker tokenizer: %w", err)
	}

	sessOpts, err := embed.NewSessionOptionsForProvider(cfg.Log)
	if err != nil {
		tk.Close()
		return nil, err
	}
	if sessOpts != nil {
		defer sessOpts.Destroy()
	}

	inputNames := []string{"input_ids", "attention_mask"}
	if spec.tokenTypeIDs {
		inputNames = append(inputNames, "token_type_ids")
	}
	session, err := ort.NewDynamicAdvancedSession(
		modelPath,
		inputNames,
		[]string{"logits"},
		sessOpts,
	)
	if err != nil {
		tk.Close()
		return nil, fmt.Errorf("create reranker onnx session: %w", err)
	}

	cfg.Log.Info("OnnxReranker ready", "model", cfg.ModelName, "max_tokens", cfg.MaxTokens)
	return &OnnxReranker{
		session:        session,
		tokenizer:      tk,
		batchSize:      cfg.BatchSize,
		maxTokens:      cfg.MaxTokens,
		maxQueryTokens: cfg.MaxQueryTokens,
		useBERTPair:    spec.useBERTPair,
		tokenTypeIDs:   spec.tokenTypeIDs,
		log:            cfg.Log,
	}, nil
}

func (r *OnnxReranker) Close() error {
	r.session.Destroy()
	r.tokenizer.Close()
	return nil
}

func (r *OnnxReranker) Rerank(ctx context.Context, query string, candidates []retrieve.ScoredResult) ([]retrieve.ScoredResult, error) {
	if len(candidates) <= 1 {
		return candidates, nil
	}
	docs := make([]string, len(candidates))
	for i, c := range candidates {
		docs[i] = candidateText(c)
	}

	scores, err := r.score(ctx, query, docs)
	if err != nil {
		return nil, err
	}

	out := append([]retrieve.ScoredResult(nil), candidates...)
	for i := range out {
		out[i].Score = scores[i]
		// Keep the raw verdict: downstream ranking adds bonuses to Score, so
		// this is the only surviving "how good a match is this, absolutely".
		out[i].Relevance = scores[i]
		if out[i].Why == "" {
			out[i].Why = "rerank"
		} else if !strings.Contains(out[i].Why, "rerank") {
			out[i].Why += "+rerank"
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out, nil
}

func (r *OnnxReranker) score(_ context.Context, query string, docs []string) ([]float32, error) {
	scores := make([]float32, len(docs))
	for start := 0; start < len(docs); start += r.batchSize {
		end := start + r.batchSize
		if end > len(docs) {
			end = len(docs)
		}
		batchScores, err := r.scoreBatch(query, docs[start:end])
		if err != nil {
			return nil, fmt.Errorf("rerank batch [%d:%d]: %w", start, end, err)
		}
		copy(scores[start:end], batchScores)
	}
	return scores, nil
}

func (r *OnnxReranker) scoreBatch(query string, docs []string) ([]float32, error) {
	type encoded struct {
		ids  []int64
		mask []int64
	}

	encs := make([]encoded, len(docs))
	seqLen := 0
	for i, doc := range docs {
		ids := r.encodePair(query, doc)
		mask := make([]int64, len(ids))
		for j := range mask {
			mask[j] = 1
		}
		if len(ids) > seqLen {
			seqLen = len(ids)
		}
		encs[i] = encoded{ids: ids, mask: mask}
	}
	if seqLen == 0 {
		seqLen = 1
	}

	batchSize := len(docs)
	flatSize := batchSize * seqLen
	inputIDs := make([]int64, flatSize)
	attMask := make([]int64, flatSize)
	for i, enc := range encs {
		base := i * seqLen
		copy(inputIDs[base:], enc.ids)
		copy(attMask[base:], enc.mask)
	}

	shape := ort.NewShape(int64(batchSize), int64(seqLen))
	tInputIDs, err := ort.NewTensor(shape, inputIDs)
	if err != nil {
		return nil, fmt.Errorf("new input_ids tensor: %w", err)
	}
	defer tInputIDs.Destroy()

	tAttMask, err := ort.NewTensor(shape, attMask)
	if err != nil {
		return nil, fmt.Errorf("new attention_mask tensor: %w", err)
	}
	defer tAttMask.Destroy()

	inputs := []ort.Value{tInputIDs, tAttMask}
	if r.tokenTypeIDs {
		// All zeros: Roberta tokenizers never emit segment 1, so zeros are
		// what the model saw in (pre)training. See modelSpec.tokenTypeIDs.
		typeIDs := make([]int64, flatSize)
		tTypeIDs, err := ort.NewTensor(shape, typeIDs)
		if err != nil {
			return nil, fmt.Errorf("new token_type_ids tensor: %w", err)
		}
		defer tTypeIDs.Destroy()
		inputs = append(inputs, tTypeIDs)
	}

	outData := make([]float32, batchSize)
	tOutput, err := ort.NewTensor(ort.NewShape(int64(batchSize), 1), outData)
	if err != nil {
		return nil, fmt.Errorf("new logits tensor: %w", err)
	}
	defer tOutput.Destroy()

	if err := r.session.Run(inputs, []ort.Value{tOutput}); err != nil {
		return nil, fmt.Errorf("onnx run: %w", err)
	}

	logits := tOutput.GetData()
	scores := make([]float32, batchSize)
	for i := range scores {
		scores[i] = sigmoid(logits[i])
	}
	return scores, nil
}

// encodePair returns the combined query+doc pair ids in the model's native
// pair format (see specForModel for which model uses which).
func (r *OnnxReranker) encodePair(query, doc string) []int64 {
	queryEnc := r.tokenizer.EncodeWithOptions(query, true)
	docEnc := r.tokenizer.EncodeWithOptions(doc, true)
	if r.useBERTPair {
		// DECISION: [CLS]/[SEP]-style tokenizers (mxbai's DebertaV2) already
		// insert specials via AddSpecialTokens=true, so we concatenate the two
		// sequences while stripping the trailing [SEP] of the query and the
		// leading [CLS] of the document to form [CLS] q [SEP] d [SEP].
		return buildBERTPairIDs(queryEnc.IDs, docEnc.IDs, r.maxTokens, r.maxQueryTokens)
	}
	// Roberta/XLM-R style: <s> q </s></s> d </s> (double separator).
	return buildXLMRobertaPairIDs(queryEnc.IDs, docEnc.IDs, r.maxTokens, r.maxQueryTokens)
}

func buildBERTPairIDs(queryIDs, docIDs []uint32, maxTokens, maxQueryTokens int) []int64 {
	if maxTokens <= 0 {
		maxTokens = defaultRerankerMaxTokens
	}
	if maxQueryTokens <= 0 || maxQueryTokens > maxTokens-3 {
		maxQueryTokens = min(maxTokens/4, defaultRerankerMaxQueryTokens)
	}
	if len(queryIDs) == 0 && len(docIDs) == 0 {
		return []int64{0}
	}

	// queryIDs is [CLS] q [SEP]; docIDs is [CLS] d [SEP]
	// Combined: [CLS] q [SEP] d [SEP] — drop leading [CLS] of docIDs
	queryPart := truncateIDs(queryIDs, maxQueryTokens)
	remaining := maxTokens - len(queryPart)
	if remaining < 1 {
		return toInt64(truncateIDs(queryPart, maxTokens))
	}
	docPart := docIDs
	if len(docPart) > 1 {
		docPart = docPart[1:] // drop leading [CLS]
	}
	docPart = truncateIDs(docPart, remaining)

	combined := make([]uint32, 0, len(queryPart)+len(docPart))
	combined = append(combined, queryPart...)
	combined = append(combined, docPart...)
	return toInt64(combined)
}

func buildXLMRobertaPairIDs(queryIDs, docIDs []uint32, maxTokens, maxQueryTokens int) []int64 {
	if maxTokens <= 0 {
		maxTokens = defaultRerankerMaxTokens
	}
	if maxQueryTokens <= 0 || maxQueryTokens > maxTokens-3 {
		maxQueryTokens = min(maxTokens/4, defaultRerankerMaxQueryTokens)
	}
	if len(queryIDs) == 0 && len(docIDs) == 0 {
		return []int64{0}
	}
	if len(queryIDs) == 0 {
		return toInt64(truncateIDs(docIDs, maxTokens))
	}
	if len(docIDs) == 0 {
		return toInt64(truncateIDs(queryIDs, maxTokens))
	}

	sep := queryIDs[len(queryIDs)-1]
	queryPart := truncateIDs(queryIDs, maxQueryTokens)
	remaining := maxTokens - len(queryPart) - 1
	if remaining < 1 {
		return toInt64(truncateIDs(queryPart, maxTokens))
	}

	docPart := docIDs
	if len(docPart) > 1 {
		docPart = docPart[1:]
	}
	docPart = truncateIDs(docPart, remaining)

	combined := make([]uint32, 0, len(queryPart)+1+len(docPart))
	combined = append(combined, queryPart...)
	combined = append(combined, sep)
	combined = append(combined, docPart...)
	return toInt64(combined)
}

func truncateIDs(ids []uint32, maxLen int) []uint32 {
	if maxLen <= 0 || len(ids) <= maxLen {
		return append([]uint32(nil), ids...)
	}
	out := append([]uint32(nil), ids[:maxLen]...)
	out[len(out)-1] = ids[len(ids)-1]
	return out
}

func candidateText(c retrieve.ScoredResult) string {
	var b strings.Builder
	if c.QualifiedName != "" {
		b.WriteString("symbol: ")
		b.WriteString(c.QualifiedName)
		b.WriteString("\n")
	}
	if c.Kind != "" {
		b.WriteString("kind: ")
		b.WriteString(c.Kind)
		b.WriteString("\n")
	}
	if c.File != "" {
		b.WriteString("file: ")
		b.WriteString(c.File)
		b.WriteString("\n")
	}
	if c.Signature != "" {
		b.WriteString("signature: ")
		b.WriteString(c.Signature)
		b.WriteString("\n")
	}
	if c.Docstring != "" {
		b.WriteString("summary: ")
		b.WriteString(c.Docstring)
		b.WriteString("\n")
	}
	if c.Body != "" {
		b.WriteString("body:\n")
		b.WriteString(truncateBody(c.Body, defaultRerankerBodyChars))
	}
	return b.String()
}

func truncateBody(body string, maxChars int) string {
	if maxChars <= 0 || len(body) <= maxChars {
		return body
	}
	return body[:maxChars] + "\n...[truncated]"
}

func toInt64(u []uint32) []int64 {
	out := make([]int64, len(u))
	for i, v := range u {
		out[i] = int64(v)
	}
	return out
}

func sigmoid(x float32) float32 {
	if x >= 0 {
		z := float32(math.Exp(float64(-x)))
		return 1 / (1 + z)
	}
	z := float32(math.Exp(float64(x)))
	return z / (1 + z)
}
