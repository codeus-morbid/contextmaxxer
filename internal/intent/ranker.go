package intent

import (
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
)

type Ranker struct{}

type Profile struct {
	Tokens               map[string]struct{}
	ActionHints          map[string]struct{}
	ConstructorLookup    bool
	ActionLookup         bool
	Language             string
	PreferImplementation bool
}

func (r *Ranker) IsIntentRanker() {}

func NewRanker() *Ranker {
	return &Ranker{}
}

// DefaultWeight scales the whole intent score before it is added to the
// ranking score.
//
// DECISION(2026-08): the raw intent score reaches ~+4 (coverage 1.4 +
// constructor 2.0 + language 1.8) while the cross-encoder it is added to
// emits a 0..1 sigmoid — so an unscaled "prior" outranks the calibrated
// model by 4x and becomes the primary ranker. Measured consequence: on the
// gen corpus (queries written in code vocabulary, which the coverage term
// rewards) it looked like +0.05 Hit@1, but across 20 repos of docstring-prose
// queries it cost -0.24 Hit@1 — helping only on the corpus it was tuned on.
// Scaled to a tie-breaker it can still order near-ties without overriding
// the cross-encoder. Swept on both corpora (gen Hit@1/Hit@3, mean
// self-retrieval Hit@1 over 20 repos): 1.0 -> 0.65/0.87, 0.667; 0.15 ->
// 0.65/0.90, 0.839; 0.05 -> 0.64/0.90, 0.880; 0 -> 0.60/0.88, 0.904.
// 0.05 costs ONE gen case (inside the +/-1 noise floor) and buys ~100
// rank-1s across the polyglot suite; going to 0 loses 4 gen cases, so the
// ranker does earn its place - it just must not outvote the model.
// REVISIT IF: the reranker changes scale (a different model or a logit
// output instead of a sigmoid).
const DefaultWeight = 0.05

// Weight is the intent scale, overridable for experiments via
// CONTEXTMAXXER_INTENT_WEIGHT (a sweep knob, not a supported setting).
func Weight() float32 {
	if v := os.Getenv("CONTEXTMAXXER_INTENT_WEIGHT"); v != "" {
		if f, err := strconv.ParseFloat(v, 32); err == nil {
			return float32(f)
		}
	}
	return DefaultWeight
}

func (r *Ranker) Rank(query string, candidates []retrieve.ScoredResult) []retrieve.ScoredResult {
	profile := Analyze(query)
	out := append([]retrieve.ScoredResult(nil), candidates...)
	// DECISION(2026-08): the window stays 15, and the list it returns is NOT
	// guaranteed sorted past that point. Both facts are deliberate.
	//
	// The pool can exceed 15 even at the served max_results=5, because the
	// protections append candidates past the limit (appendMissingTopVectorSeeds,
	// appendMissingTopPPR). Scoring those too was tried: measured on the 20-repo
	// gate it cost csharp-newtonsoft 0.03 Hit@1 and c-postgres 0.01, and gained
	// nothing anywhere. The protected tail is there to survive INTO the pool,
	// not to be re-ranked against the candidates that earned their place.
	//
	// The unsorted tail is real — the order breaks at the 14/15 boundary, a
	// penalised candidate at 0.911 above an untouched 0.985 — and unreachable:
	// runPipeline truncates to max_results (5) right after, and the confidence
	// gap reads top-1 against top-2. Fixing what cannot surface is what the
	// measurement above priced. REVISIT IF: a caller starts reading the pool
	// beyond max_results, or releasecfg.RerankK changes.
	window := 15
	if window > len(out) {
		window = len(out)
	}
	w := Weight()
	for i := 0; i < window; i++ {
		// Only the categorical signals are scaled. fieldScore's coefficients
		// (0.005-0.02) were already sized as a tie-breaker against a 0..1
		// score; scaling them too would erase the sibling-disambiguation
		// signal instead of taming the priors that drown the cross-encoder.
		out[i].Score += w*score(profile, out[i]) + fieldScore(out[i])
		if out[i].Why == "" {
			out[i].Why = "intent_rank"
		} else if !strings.Contains(out[i].Why, "intent_rank") {
			out[i].Why += "+intent_rank"
		}
	}
	sort.SliceStable(out[:window], func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

func Analyze(query string) Profile {
	tokens := tokenize(query)
	p := Profile{Tokens: tokens, ActionHints: make(map[string]struct{})}
	if hasAny(tokens, "new", "constructor", "create", "created", "open", "opened", "init", "initialized") {
		p.ConstructorLookup = true
	}
	if hasAny(tokens, "where", "how", "find", "search", "build", "extract", "store", "insert", "walk", "pack", "embed") {
		p.ActionLookup = true
		p.PreferImplementation = true
	}
	// A query that LEADS with an action verb ("retry the ai analysis...",
	// "encrypt a provider key...") wants the code that does it, not the data
	// types around it. Position matters: "browser wrapper that calls backend"
	// leads with a noun and may legitimately target a class, so only the
	// first meaningful token counts.
	if leadsWithActionVerb(query) {
		p.ActionLookup = true
		p.PreferImplementation = true
	}
	for _, hint := range []string{"new", "start", "get", "upsert", "fetch", "parse", "create", "update", "delete"} {
		if hasAny(tokens, hint) {
			p.ActionHints[hint] = struct{}{}
		}
	}
	switch {
	case hasAny(tokens, "go", "golang"):
		p.Language = "go"
	case hasAny(tokens, "python", "py"):
		p.Language = "python"
	case hasAny(tokens, "typescript", "ts", "tsx"):
		p.Language = "typescript"
	}
	return p
}

func score(profile Profile, c retrieve.ScoredResult) float32 {
	var s float32
	candidateTokens := candidateTokenSet(c)

	coverage := tokenCoverage(profile.Tokens, candidateTokens)
	s += 1.4 * coverage

	if profile.ConstructorLookup {
		if isConstructor(c) {
			s += 2.0
		} else {
			s -= 0.4
		}
		if isPassiveKind(c.Kind) {
			s -= 0.9
		}
	}

	if profile.PreferImplementation {
		if c.Kind == "function" || c.Kind == "method" {
			s += 0.35
		}
		if isPassiveKind(c.Kind) {
			s -= 0.9
		}
	}

	if profile.Language != "" {
		if matchesLanguage(profile.Language, c) {
			s += 1.8
		} else if isLanguageSpecific(c) {
			s -= 1.2
		}
	}

	for hint := range profile.ActionHints {
		if _, ok := candidateTokens[hint]; ok {
			s += 0.45
		} else if hint == "new" || hint == "start" || hint == "get" || hint == "upsert" || hint == "fetch" || hint == "parse" {
			s -= 0.2
		}
	}
	for _, noisy := range []string{"admin", "debug", "mock", "test", "manual", "prediction"} {
		if _, hasNoisy := candidateTokens[noisy]; hasNoisy {
			if _, queryWantsNoisy := profile.Tokens[noisy]; !queryWantsNoisy {
				s -= 0.2
			}
		}
	}
	if isTestInfraPath(c.File) && !queryWantsTestInfra(profile.Tokens) {
		s -= 0.7
	}
	return s
}

// Verbs that, when leading the query, signal "find the code that DOES this".
// Stored raw; matched through normalize on both sides.
var actionVerbLexicon = []string{
	"aggregate", "apply", "buy", "cache", "calculate", "classify", "compare",
	"compute", "convert", "decide", "decode", "decrypt", "delete", "detect",
	"dispatch", "download", "encode", "encrypt", "execute", "export", "fetch",
	"filter", "format", "generate", "get", "handle", "hash", "import", "load",
	"log", "match", "merge", "notify", "parse", "place", "process", "publish",
	"rank", "read", "record", "render", "resolve", "retry", "route", "run",
	"save", "schedule", "score", "sell", "send", "serialize", "settle", "sort",
	"sync", "translate", "update", "upload", "validate", "verify", "write",
}

var actionVerbs = func() map[string]struct{} {
	m := make(map[string]struct{}, len(actionVerbLexicon))
	for _, v := range actionVerbLexicon {
		m[normalize(v)] = struct{}{}
	}
	return m
}()

// leadsWithActionVerb reports whether the first meaningful query token is an
// action verb. Leading adverbs ("automatically buy...") and interrogative /
// filler words are skipped; anything else (a noun lead) returns false.
func leadsWithActionVerb(query string) bool {
	skip := map[string]struct{}{
		"a": {}, "an": {}, "the": {}, "and": {}, "also": {}, "then": {},
		"please": {}, "what": {}, "which": {}, "who": {}, "when": {},
		"where": {}, "how": {}, "why": {}, "do": {}, "does": {}, "can": {},
		"should": {}, "we": {}, "i": {}, "you": {},
	}
	for _, raw := range strings.Fields(strings.ToLower(query)) {
		tok := strings.TrimFunc(raw, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if tok == "" {
			continue
		}
		if _, ok := skip[tok]; ok {
			continue
		}
		if strings.HasSuffix(tok, "ly") { // adverb: "automatically", "eagerly"
			continue
		}
		_, isVerb := actionVerbs[normalize(tok)]
		return isVerb
	}
	return false
}

// Test-infra path segments and file patterns. Deliberately NOT including
// cmd/ or scripts/: in tool-heavy repos those hold real targets (two holdout
// cases expect symbols under cmd/), while test scaffolding is near-never the
// answer unless the query says so.
var testInfraSegments = map[string]struct{}{
	"test": {}, "tests": {}, "testdata": {}, "testutil": {}, "testutils": {},
	"mock": {}, "mocks": {}, "fixtures": {}, "e2e": {},
	"bench": {}, "benchmarks": {},
	// Simulation and load-harness trees. Measured on cockroach: 2 of 5
	// production questions returned simulator code at rank 1 —
	// asim/queue.splitQueue.shouldSplit for "where is a range split by load"
	// (the real one is pkg/kv/kvserver/replica_split_load.go) and
	// storerebalancer.simulatorReplica.AdminTransferLease for "transfer the
	// lease" (really pkg/kv/kvserver/replica_range_lease.go:952). These trees
	// mirror production APIs symbol-for-symbol, so semantics alone cannot
	// separate them; only the path can.
	"asim": {}, "simulation": {}, "simulator": {}, "roachtest": {},
}

func isTestInfraPath(file string) bool {
	norm := strings.ReplaceAll(file, "\\", "/")
	for _, seg := range strings.Split(norm, "/") {
		if _, ok := testInfraSegments[strings.ToLower(seg)]; ok {
			return true
		}
	}
	base := strings.ToLower(path.Base(norm))
	if name, ok := strings.CutSuffix(base, path.Ext(base)); ok && name != "" {
		if strings.HasSuffix(name, "_test") || strings.HasPrefix(name, "test_") ||
			strings.HasSuffix(name, ".spec") || strings.HasSuffix(name, ".test") {
			return true
		}
	}
	return false
}

func queryWantsTestInfra(tokens map[string]struct{}) bool {
	return hasAny(tokens, "test", "tests", "mock", "mocks", "fixture", "bench", "benchmark",
		"e2e", "spec", "coverage", "simulation", "simulator", "simulate", "asim", "roachtest")
}

func candidateTokenSet(c retrieve.ScoredResult) map[string]struct{} {
	text := c.QualifiedName + " " + c.File + " " + c.Kind
	return tokenize(text)
}

func tokenize(text string) map[string]struct{} {
	expanded := splitCamel(text)
	out := make(map[string]struct{})
	for _, tok := range strings.FieldsFunc(strings.ToLower(expanded), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(tok) < 2 {
			continue
		}
		out[normalize(tok)] = struct{}{}
	}
	return out
}

func splitCamel(text string) string {
	var b strings.Builder
	var prev rune
	for i, r := range text {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(prev) || unicode.IsDigit(prev)) {
			b.WriteRune(' ')
		}
		b.WriteRune(r)
		prev = r
	}
	return b.String()
}

func normalize(tok string) string {
	switch tok {
	case "employees":
		return "employee"
	case "children":
		return "child"
	case "indices":
		return "index"
	case "symbols":
		return "symbol"
	case "edges":
		return "edge"
	case "files":
		return "file"
	case "opened", "open":
		return "new"
	case "created", "create":
		return "new"
	case "initialized", "init":
		return "new"
	case "construct", "constructed", "instantiate", "instantiated":
		return "new"
	case "golang":
		return "go"
	case "ts":
		return "typescript"
	}
	if len(tok) > 4 && strings.HasSuffix(tok, "ies") {
		return strings.TrimSuffix(tok, "ies") + "y"
	}
	for _, suffix := range []string{"ing", "ed", "es", "s"} {
		if len(tok) > len(suffix)+3 && strings.HasSuffix(tok, suffix) {
			return strings.TrimSuffix(tok, suffix)
		}
	}
	return tok
}

func tokenCoverage(queryTokens, candidateTokens map[string]struct{}) float32 {
	if len(queryTokens) == 0 {
		return 0
	}
	var hits int
	for tok := range queryTokens {
		if _, ok := candidateTokens[tok]; ok {
			hits++
		}
	}
	return float32(hits) / float32(len(queryTokens))
}

func hasAny(tokens map[string]struct{}, values ...string) bool {
	for _, v := range values {
		if _, ok := tokens[normalize(v)]; ok {
			return true
		}
	}
	return false
}

func isConstructor(c retrieve.ScoredResult) bool {
	tokens := candidateTokenSet(c)
	_, hasNew := tokens["new"]
	return c.Features.IsConstructor > 0 || hasNew
}

func fieldScore(c retrieve.ScoredResult) float32 {
	f := c.Features
	return 0.02*f.ShortNameOverlap +
		0.01*f.NameOverlap +
		0.01*f.SignatureOverlap +
		0.005*f.PathOverlap +
		0.005*f.KindOverlap
}

func isPassiveKind(kind string) bool {
	return kind == "class" || kind == "interface" || kind == "type"
}

func matchesLanguage(language string, c retrieve.ScoredResult) bool {
	tokens := languageIndicatorTokens(c)
	switch language {
	case "go":
		_, hasGo := tokens["go"]
		_, hasGolang := tokens["golang"]
		return hasGo || hasGolang
	case "python":
		_, hasPython := tokens["python"]
		_, hasPy := tokens["py"]
		return hasPython || hasPy
	case "typescript":
		_, hasTypescript := tokens["typescript"]
		_, hasTS := tokens["ts"]
		return hasTypescript || hasTS
	default:
		return false
	}
}

func isLanguageSpecific(c retrieve.ScoredResult) bool {
	tokens := languageIndicatorTokens(c)
	return containsAny(tokens, "go", "golang", "python", "py", "typescript", "ts")
}

func languageIndicatorTokens(c retrieve.ScoredResult) map[string]struct{} {
	fileBase := path.Base(strings.ReplaceAll(c.File, "\\", "/"))
	if dot := strings.LastIndex(fileBase, "."); dot >= 0 {
		fileBase = fileBase[:dot]
	}
	return tokenize(c.QualifiedName + " " + fileBase)
}

func containsAny(tokens map[string]struct{}, values ...string) bool {
	for _, v := range values {
		if _, ok := tokens[normalize(v)]; ok {
			return true
		}
	}
	return false
}
