package retrieve

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/codeus-morbid/contextmaxxer/internal/embed"
	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

const rrfK = 60

// DefaultFTSWeight scales the FTS list's contribution in hybrid RRF fusion
// (1.0 = classic equal-weight RRF). Package-level so eval can sweep it;
// promote to a Request field once the value is settled.
var DefaultFTSWeight float32 = 1.0

// DECISION: 0 means no cap. Agent doing manual exploration spends
// 10-30k tokens on Glob+Read+Grep — even our verbose answer mode
// (~12k tokens) saves 50%+. We default to no cap and let the
// agent throttle if it wants via explicit BudgetTokens.
const defaultBudgetTokens = 32000

func runPipeline(ctx context.Context, r *Retriever, req Request) (Result, error) {
	if req.BudgetTokens == 0 {
		req.BudgetTokens = defaultBudgetTokens
	}
	if req.SeedK == 0 {
		req.SeedK = 20
	}
	if req.MaxResults == 0 {
		req.MaxResults = 30
	}

	start := time.Now()
	var stats Stats

	t0 := time.Now()
	var qvec []float32
	vecs, err := embed.EmbedQueries(ctx, r.embedder, []string{req.Query})
	if err != nil {
		// Same resilience rule as the reranker: an embedder failure degrades
		// to lexical seeding (FTS + graph still answer) instead of failing
		// the request. Vector-only mode has nothing to fall back to.
		if req.Mode == ModeVectorOnly {
			return Result{}, fmt.Errorf("embed query: %w", err)
		}
		r.log.Warn("embed query failed; serving FTS-only seeds", "err", err)
	} else {
		qvec = vecs[0]
	}
	stats.EmbedDuration = time.Since(t0)

	tSeed := time.Now()
	var vSeeds []store.ScoredSymbol
	if qvec != nil {
		vSeeds, err = r.store.SearchByVectorScored(ctx, qvec, req.SeedK)
		if err != nil {
			return Result{}, fmt.Errorf("seed search: %w", err)
		}
	}

	type seedOrigin struct {
		score float32
		fromV bool
		fromF bool
	}

	var seeds []int64
	vectorScores := make(map[int64]float32)
	originMap := make(map[int64]*seedOrigin)

	if req.Mode == ModeVectorOnly {
		seeds = make([]int64, 0, len(vSeeds))
		for _, s := range vSeeds {
			seeds = append(seeds, s.ID)
			vectorScores[s.ID] = s.Score
			originMap[s.ID] = &seedOrigin{score: s.Score, fromV: true}
		}
	} else {
		fSeeds, err := r.store.SearchByText(ctx, req.Query, req.SeedK)
		if err != nil {
			return Result{}, fmt.Errorf("fts seed search: %w", err)
		}

		rrfScores := make(map[int64]float32)
		for rank, s := range vSeeds {
			rrfScores[s.ID] += 1.0 / float32(rrfK+rank+1)
			if _, ok := originMap[s.ID]; !ok {
				originMap[s.ID] = &seedOrigin{}
			}
			originMap[s.ID].fromV = true
			vectorScores[s.ID] = s.Score
		}
		// DECISION(2026-06): FTS gets a reduced vote in RRF. Equal-weight fusion
		// systematically demoted correct vector candidates on paraphrastic
		// queries (gen-corpus fb_g_encrypt_key: vector rank 3 -> hybrid rank 14;
		// ctx_g_alpha_balance: 13 -> out of pool). FTS still boosts identifier
		// matches, but cannot outvote strong semantic evidence alone.
		// REVISIT IF: lexical/identifier slices regress on the gen corpus.
		ftsW := DefaultFTSWeight
		for rank, s := range fSeeds {
			rrfScores[s.ID] += ftsW / float32(rrfK+rank+1)
			if _, ok := originMap[s.ID]; !ok {
				originMap[s.ID] = &seedOrigin{}
			}
			originMap[s.ID].fromF = true
		}

		type rrfEntry struct {
			id    int64
			score float32
		}
		all := make([]rrfEntry, 0, len(rrfScores))
		for id, sc := range rrfScores {
			all = append(all, rrfEntry{id, sc})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })

		limit := req.SeedK
		if limit > len(all) {
			limit = len(all)
		}
		seeds = make([]int64, limit)
		for i := 0; i < limit; i++ {
			id := all[i].id
			seeds[i] = id
			vectorScores[id] = rrfScores[id]
			originMap[id].score = rrfScores[id]
		}
	}
	stats.SeedDuration = time.Since(tSeed)
	stats.SeedCount = len(seeds)

	seedSet := make(map[int64]bool, len(seeds))
	seedScores := make(map[int64]float32, len(seeds))
	for _, id := range seeds {
		seedSet[id] = true
		seedScores[id] = vectorScores[id]
	}

	t1 := time.Now()
	symbolIDs, err := r.store.ListAllSymbolIDs(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("list symbol ids: %w", err)
	}
	edges, err := r.store.ListAllEdges(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("list edges: %w", err)
	}
	g := BuildGraph(symbolIDs, edges)
	stats.GraphDuration = time.Since(t1)
	stats.GraphNodes = g.NumNodes
	stats.GraphEdges = len(edges)

	t2 := time.Now()
	ranks, iters := PersonalizedPageRank(g, seedScores, 0.85, 30)
	stats.PPRDuration = time.Since(t2)
	stats.PPRIterations = iters

	// DECISION: weighted combination final_score = alpha*norm_seed + (1-alpha)*norm_ppr.
	// The default alpha is query-adaptive: precise direct matches get more seed weight,
	// while weak/scattered seeds still allow the graph to help.
	alpha := effectiveAlpha(req, vSeeds)
	stats.EffectiveAlpha = alpha

	var maxSeed float32
	for _, sc := range seedScores {
		if sc > maxSeed {
			maxSeed = sc
		}
	}
	var maxPPR float32
	for _, sc := range ranks {
		if sc > maxPPR {
			maxPPR = sc
		}
	}
	var maxDegree int
	for _, out := range g.OutEdges {
		if len(out) > maxDegree {
			maxDegree = len(out)
		}
	}
	seedRank := make(map[int64]int, len(seeds))
	for i, id := range seeds {
		seedRank[id] = i + 1
	}
	type pprEntry struct {
		id    int64
		score float32
	}
	pprEntries := make([]pprEntry, 0, len(ranks))
	for id, sc := range ranks {
		pprEntries = append(pprEntries, pprEntry{id: id, score: sc})
	}
	sort.Slice(pprEntries, func(i, j int) bool { return pprEntries[i].score > pprEntries[j].score })
	pprRank := make(map[int64]int, len(pprEntries))
	for i, entry := range pprEntries {
		pprRank[entry.id] = i + 1
	}

	type ranked struct {
		id    int64
		score float32
	}
	allRanked := make([]ranked, 0, len(ranks))
	fusedScores := make(map[int64]float32, len(ranks))
	for id, pprSc := range ranks {
		var normSeed float32
		if maxSeed > 0 {
			if sc, isSeed := seedScores[id]; isSeed {
				normSeed = sc / maxSeed
			}
		}
		var normPPR float32
		if maxPPR > 0 {
			normPPR = pprSc / maxPPR
		}
		fused := alpha*normSeed + (1-alpha)*normPPR
		fusedScores[id] = fused
		allRanked = append(allRanked, ranked{id, fused})
	}
	sort.Slice(allRanked, func(i, j int) bool { return allRanked[i].score > allRanked[j].score })

	limit := req.MaxResults
	if r.reranker != nil && req.RerankK > limit {
		limit = req.RerankK
	}
	if limit > len(allRanked) {
		limit = len(allRanked)
	}
	topIDs := make([]int64, limit)
	for i := 0; i < limit; i++ {
		topIDs[i] = allRanked[i].id
	}
	qTokens := tokenizeForOverlap(req.Query)
	if !req.AlphaSet {
		if r.reranker != nil {
			topIDs = appendMissingTopVectorSeeds(topIDs, vSeeds, limit, qTokens)
		} else {
			topIDs = protectTopVectorSeeds(topIDs, vSeeds, 1)
		}
	}

	// DECISION: Re-protect top-PPR-ranked candidates (Bug B fix). FTS in hybrid
	// mode crowds out PPR-only graph candidates with low seed score; this ensures
	// strong PPR candidates always reach the reranker pool.
	const protectPPRCount = 5
	pprIDsByRank := make([]int64, 0, len(pprEntries))
	for _, e := range pprEntries {
		pprIDsByRank = append(pprIDsByRank, e.id)
	}
	topIDs = appendMissingTopPPR(topIDs, pprIDsByRank, protectPPRCount)

	topScores := make(map[int64]float32, len(topIDs))
	for _, id := range topIDs {
		topScores[id] = fusedScores[id]
	}

	syms, err := r.store.GetSymbolsByIDs(ctx, topIDs)
	if err != nil {
		return Result{}, fmt.Errorf("hydrate symbols: %w", err)
	}

	fileIDSet := make(map[int64]bool, len(syms))
	for _, s := range syms {
		fileIDSet[s.FileID] = true
	}
	fileIDs := make([]int64, 0, len(fileIDSet))
	for id := range fileIDSet {
		fileIDs = append(fileIDs, id)
	}
	filePaths, err := r.store.GetFilesByIDs(ctx, fileIDs)
	if err != nil {
		return Result{}, fmt.Errorf("resolve file paths: %w", err)
	}

	// DECISION: GetSymbolsByIDs returns rows in DB order (IN clause), so we
	// build an index by ID then iterate topIDs to preserve PPR rank order.
	symIdx := make(map[int64]int, len(syms))
	for i, s := range syms {
		symIdx[s.ID] = i
	}

	scored := make([]ScoredResult, 0, len(topIDs))
	for _, id := range topIDs {
		i, ok := symIdx[id]
		if !ok {
			continue
		}
		s := syms[i]

		body := s.BodyExcerpt
		if body == "" {
			body = s.Signature
		}

		// DECISION: micro-symbol filter runs post-PPR so graph connectivity is
		// preserved (trivial consts still influence PPR propagation), but single-line
		// constants/vars with tiny bodies are excluded from the result list to prevent
		// them from crowding out substantive symbols in top-5.
		if !req.IncludeTrivial {
			lineSpan := s.EndLine - s.StartLine
			if len(body) < 50 && lineSpan < 2 {
				continue
			}
		}

		sc := topScores[id]
		features := RankingFeatures{
			PPR:              normalizedScore(ranks[id], maxPPR),
			EffectiveAlpha:   alpha,
			SeedScore:        seedScores[id],
			PPRScore:         ranks[id],
			SeedRank:         seedRank[id],
			PPRRank:          pprRank[id],
			ShortNameOverlap: overlapRatio(qTokens, s.Name),
			NameOverlap:      overlapRatio(qTokens, s.QualifiedName),
			PathOverlap:      overlapRatio(qTokens, filePaths[s.FileID]),
			KindOverlap:      overlapRatio(qTokens, s.Kind),
			SignatureOverlap: overlapRatio(qTokens, s.Signature),
			BodyOverlap:      overlapRatio(qTokens, body),
			HubPenalty:       degreePenalty(g, id, maxDegree),
		}
		if o, isSeed := originMap[id]; isSeed {
			if o.fromV {
				features.VectorSeed = 1
			}
			if o.fromF {
				features.FTSSeed = 1
			}
		}
		if strings.HasPrefix(s.Name, "New") || strings.HasSuffix(s.QualifiedName, ".New") || strings.HasPrefix(body, "func New") {
			features.IsConstructor = 1
		}
		switch s.Kind {
		case "function":
			features.KindFunction = 1
		case "method":
			features.KindMethod = 1
		}

		// DECISION: 1.15x boost for constructors — small enough not to override
		// semantic ranking, but enough to surface New* above neighbouring micro-symbols
		// when PPR scores are close. Constructor bodies are typically substantive
		// (init logic, deps wiring) so they're rarely filtered by the micro-symbol check.
		if strings.HasPrefix(s.Name, "New") || strings.HasSuffix(s.QualifiedName, ".New") {
			sc *= 1.15
		} else if strings.HasPrefix(body, "func New") {
			sc *= 1.15
		}

		why := "ppr_neighbor"
		if o, isSeed := originMap[id]; isSeed && seedSet[id] {
			if o.fromV && o.fromF {
				why = "hybrid_seed"
			} else if o.fromF {
				why = "fts_seed"
			} else {
				why = "vector_seed"
			}
		}

		scored = append(scored, ScoredResult{
			SymbolID:      s.ID,
			File:          filePaths[s.FileID],
			QualifiedName: s.QualifiedName,
			Kind:          s.Kind,
			Signature:     s.Signature,
			Docstring:     s.Docstring,
			StartLine:     s.StartLine,
			EndLine:       s.EndLine,
			Score:         sc,
			Why:           why,
			Body:          body,
			AlsoVia:       nil,
			Features:      features,
		})
	}

	if !req.SkipRerank && r.reranker != nil && len(scored) > 1 && !shouldSkipRerank(req, scored) {
		// DECISION(2026-06): lazy mode inverts the default — the cross-encoder
		// (~1s, 83% of total latency after the vector cache) runs only when the
		// fused ranking is ambiguous. Confident fused rankings are served as-is.
		if req.LazyRerank && !rankingAmbiguous(scored) {
			stats.RerankLazySkipped = true
		} else {
			t3 := time.Now()
			reranked, err := r.reranker.Rerank(ctx, req.Query, scored)
			if err != nil {
				// A reranker failure (seen live: transient DML 80004005 on
				// GPU) must degrade to the fused ranking, not kill the whole
				// query — the fusion order is already a good answer.
				r.log.Warn("rerank failed; serving fused ranking", "err", err)
			} else {
				scored = reranked
			}
			stats.RerankDuration = time.Since(t3)
		}
	}
	applyIntent := func() {
		if r.ranker == nil || len(scored) <= 1 {
			return
		}
		if req.SkipIntent {
			if _, ok := r.ranker.(IntentRanker); ok {
				return
			}
		}
		tRank := time.Now()
		scored = r.ranker.Rank(req.Query, scored)
		if _, ok := r.ranker.(IntentRanker); ok {
			stats.IntentDuration += time.Since(tRank)
		}
	}
	applyIntent()

	// DECISION(2026-06): escalation — when the primary (fast) ranking looks
	// ambiguous, rerun the candidate pool through a stronger reranker. On the
	// gen corpus jina-v2 beats tiny by +0.08 Hit@1 but costs ~5.7s p95; paying
	// that only on low-confidence queries keeps the common path fast.
	// REVISIT IF: escalation rate exceeds ~40% of queries or quality matches tiny.
	if r.escalator != nil && !req.SkipRerank && len(scored) > 1 && rankingAmbiguous(scored) {
		tEsc := time.Now()
		esc, escErr := r.escalator.Rerank(ctx, req.Query, scored)
		if escErr != nil {
			r.log.Warn("escalation rerank failed", "err", escErr)
		} else {
			scored = esc
			stats.Escalated = true
			applyIntent()
		}
		stats.EscalateDuration = time.Since(tEsc)
	}

	if len(scored) > req.MaxResults {
		scored = scored[:req.MaxResults]
	}

	tPack := time.Now()
	selected, _ := Pack(scored, req.BudgetTokens, req.FullBodyResults)
	stats.PackDuration = time.Since(tPack)
	expansionSymbols := append([]ScoredResult(nil), scored[:len(selected)]...)
	for i := range expansionSymbols {
		expansionSymbols[i].Detail = "full"
		expansionSymbols[i].BodyStartLine = expansionSymbols[i].StartLine
		expansionSymbols[i].BodyEndLine = expansionSymbols[i].EndLine
	}

	// Trim long full-body results to their most query-relevant span before the
	// response is built (keeps the answer, drops the bulk of large functions).
	tEv := time.Now()
	if !req.PreserveFullBodies {
		applyEvidenceSpans(ctx, r, qvec, selected)
	} else {
		for i := range selected {
			selected[i].BodyStartLine = selected[i].StartLine
			selected[i].BodyEndLine = selected[i].EndLine
		}
	}
	stats.EvidenceDuration = time.Since(tEv)
	totalTokens := 0
	for _, sr := range selected {
		totalTokens += estimateTokens(sr.Body)
	}

	assignConfidence(selected)
	assignVisibility(selected)

	if req.OutputMode != OutputModeMinimal {
		tCtx := time.Now()
		if err := enrichGraphContext(ctx, r, req, selected, filePaths); err != nil {
			r.log.Warn("graph context enrichment failed", "err", err)
		}
		enrichCompanionFiles(r, selected)
		stats.GraphCtxDuration = time.Since(tCtx)
	}

	result := Result{
		Symbols:          selected,
		TotalTokens:      totalTokens,
		Stats:            stats,
		ExpansionSymbols: expansionSymbols,
	}

	if req.OutputMode != OutputModeMinimal {
		result.NextSteps = buildNextSteps(req, selected)
		result.RetrievalHealth = buildRetrievalHealth(selected, len(scored))
	}
	if req.OutputMode == OutputModeExplore {
		result.Structure = buildStructureView(selected)
	}

	stats.Total = time.Since(start)
	result.Stats = stats

	return result, nil
}

func assignConfidence(scored []ScoredResult) {
	if len(scored) == 0 {
		return
	}
	for i := range scored {
		sc := scored[i].Score
		var gap float32
		if i+1 < len(scored) && scored[i].Score > 0 {
			gap = (scored[i].Score - scored[i+1].Score) / scored[i].Score
		} else {
			gap = 1
		}
		switch {
		case sc > 0.85 && gap > 0.10:
			scored[i].Confidence = "high"
		case sc > 0.65:
			scored[i].Confidence = "medium"
		default:
			scored[i].Confidence = "low"
		}
	}
}

func enrichGraphContext(ctx context.Context, r *Retriever, _ Request, scored []ScoredResult, filePaths map[int64]string) error {
	if len(scored) == 0 {
		return nil
	}

	// All query-independent structure (lookup maps, adjacency, file paths,
	// test index) comes from the per-generation memo — building it per query
	// was ~350ms of CPU on a 90K-symbol index; bodies below stay per-query
	// because they cover only the call-site annotation set.
	memo, err := r.getGraphMemo(ctx)
	if err != nil {
		return err
	}
	allSymsByQN := memo.byQN
	allSymsByID := memo.byID
	allFilePaths := memo.filePaths
	callerMapByID := memo.callerByID
	calleeMapByID := memo.calleeByID
	callerEdgesByID := memo.callerEdges
	calleeEdgesByID := memo.calleeEdges
	testEntries := memo.testEntries

	symToRef := func(id int64) (SymbolRef, bool) {
		sym, ok := allSymsByID[id]
		if !ok {
			return SymbolRef{}, false
		}
		return SymbolRef{
			QualifiedName: sym.QualifiedName,
			File:          allFilePaths[sym.FileID],
			Lines:         fmt.Sprintf("%d-%d", sym.StartLine, sym.EndLine),
			Kind:          sym.Kind,
		}, true
	}

	const (
		maxCallerRefs = 3
		maxCalleeRefs = 5
	)
	const staticUnverified = "static_unverified"

	// DECISION(2026-08): call edges are path-insensitive. Mark every graph ref as
	// unverified; hydrate conditional callee windows for every returned result and
	// caller windows only for the top result. Five callees retain nearby switch
	// alternatives after helper calls without making the whole graph tail verbose.
	// ASSUMES: a five-line window identifies common branch/dispatch syntax.
	// REVISIT IF: branch mistakes persist or graph-context lookups become measurable.
	bodyCache := make(map[int64]store.SymbolBody)
	callSiteFor := func(symbolID int64, callLine int) string {
		if callLine <= 0 {
			return ""
		}
		body, ok := bodyCache[symbolID]
		if !ok {
			var bodyErr error
			body, bodyErr = r.store.GetSymbolBody(ctx, symbolID)
			if bodyErr != nil {
				return ""
			}
			bodyCache[symbolID] = body
		}
		sym, ok := allSymsByID[symbolID]
		if !ok {
			return ""
		}
		return callSiteEvidence(body.Body, sym.StartLine, callLine)
	}

	// Call-site lines are captured by the AST extractors and stored on edges;
	// only the top result hydrates bounded source windows around those lines.
	for i := range scored {
		qn := scored[i].QualifiedName
		ownerSym, ownerOK := allSymsByQN[qn]

		var callerEdges []store.Edge
		if ownerOK {
			callerEdges = callerEdgesByID[ownerSym.ID]
		}
		if len(callerEdges) > maxCallerRefs {
			callerEdges = callerEdges[:maxCallerRefs]
		}
		for _, edge := range callerEdges {
			if ref, ok := symToRef(edge.Src); ok {
				ref.CallLine = edge.CallLine
				ref.PathStatus = staticUnverified
				if i == 0 {
					ref.CallSite = callSiteFor(edge.Src, edge.CallLine)
				}
				scored[i].Callers = append(scored[i].Callers, ref)
			}
		}

		var calleeEdges []store.Edge
		if ownerOK {
			calleeEdges = calleeEdgesByID[ownerSym.ID]
		}
		if len(calleeEdges) > maxCalleeRefs {
			calleeEdges = calleeEdges[:maxCalleeRefs]
		}
		for _, edge := range calleeEdges {
			if ref, ok := symToRef(edge.Dst); ok {
				// A call edge to a non-callable is always a suffix-collision
				// artifact (seen live: `defer release()` linked to a same-named
				// const in an unrelated package). Never show it to the agent.
				if !callableKind(ref.Kind) {
					continue
				}
				ref.CallLine = edge.CallLine
				ref.PathStatus = staticUnverified
				ref.CallSite = callSiteFor(edge.Src, edge.CallLine)
				scored[i].Callees = append(scored[i].Callees, ref)
			}
		}

		// Test linkage (Feature 2) — using already-loaded test symbol index
		shortName := qn
		if idx := strings.LastIndex(shortName, "."); idx >= 0 {
			shortName = shortName[idx+1:]
		}
		lower := strings.ToLower(shortName)
		if lower != "" {
			var testMatches []SymbolRef
			for _, te := range testEntries {
				if strings.Contains(te.shortName, lower) || strings.Contains(lower, te.shortName) {
					testMatches = append(testMatches, te.ref)
					if len(testMatches) >= 3 {
						break
					}
				}
			}
			scored[i].Tests = testMatches
		}

		// Flow context 2-hop (Feature 4) — using already-loaded edge maps
		// DECISION: reuse callerMapByID/calleeMapByID built once above — no extra DB call.
		sym, hasSym := allSymsByQN[qn]
		if hasSym {
			id := sym.ID
			hop1 := make(map[int64]bool)
			hop1[id] = true
			for _, cid := range callerMapByID[id] {
				hop1[cid] = true
			}
			for _, cid := range calleeMapByID[id] {
				hop1[cid] = true
			}

			var flowRefs []FlowRef

			for _, mid := range callerMapByID[id] {
				for _, gid := range callerMapByID[mid] {
					if hop1[gid] {
						continue
					}
					s2, ok2 := allSymsByID[gid]
					if !ok2 {
						continue
					}
					midSym := allSymsByID[mid]
					flowRefs = append(flowRefs, FlowRef{
						QualifiedName: s2.QualifiedName,
						File:          allFilePaths[s2.FileID],
						Lines:         fmt.Sprintf("%d-%d", s2.StartLine, s2.EndLine),
						Kind:          s2.Kind,
						Distance:      2,
						Via:           "called by -> " + midSym.Name + " -> called by",
					})
					hop1[gid] = true
					if len(flowRefs) >= 5 {
						break
					}
				}
				if len(flowRefs) >= 5 {
					break
				}
			}

			if len(flowRefs) < 5 {
				for _, mid := range calleeMapByID[id] {
					for _, gid := range calleeMapByID[mid] {
						if hop1[gid] {
							continue
						}
						s2, ok2 := allSymsByID[gid]
						if !ok2 {
							continue
						}
						midSym := allSymsByID[mid]
						flowRefs = append(flowRefs, FlowRef{
							QualifiedName: s2.QualifiedName,
							File:          allFilePaths[s2.FileID],
							Lines:         fmt.Sprintf("%d-%d", s2.StartLine, s2.EndLine),
							Kind:          s2.Kind,
							Distance:      2,
							Via:           "calls -> " + midSym.Name + " -> calls",
						})
						hop1[gid] = true
						if len(flowRefs) >= 5 {
							break
						}
					}
					if len(flowRefs) >= 5 {
						break
					}
				}
			}

			scored[i].FlowContext = flowRefs
		}
	}

	// Feature 3 - Siblings (embedding similarity) for top-3 results only.
	// DECISION: run inside enrichGraphContext to reuse allSymsByID/allFilePaths.
	// Only top-3 to bound latency (each vector search ~0.3s on large indexes).
	siblingsLimit := 3
	if len(scored) < siblingsLimit {
		siblingsLimit = len(scored)
	}
	if siblingsLimit > 0 {
		topIDs := make([]int64, 0, siblingsLimit)
		for _, sr := range scored[:siblingsLimit] {
			if sym, ok := allSymsByQN[sr.QualifiedName]; ok {
				topIDs = append(topIDs, sym.ID)
			}
		}
		if len(topIDs) > 0 {
			embeddings, embErr := r.store.GetEmbeddingsByIDs(ctx, topIDs)
			if embErr == nil && len(embeddings) > 0 {
				selectedIDSet := make(map[int64]bool, len(scored))
				for _, sr := range scored {
					if sym, ok := allSymsByQN[sr.QualifiedName]; ok {
						selectedIDSet[sym.ID] = true
					}
				}
				for i := 0; i < siblingsLimit; i++ {
					sym, ok := allSymsByQN[scored[i].QualifiedName]
					if !ok {
						continue
					}
					vec, hasEmb := embeddings[sym.ID]
					if !hasEmb {
						continue
					}
					candidates, searchErr := r.store.SearchByVectorScored(ctx, vec, 10)
					if searchErr != nil {
						continue
					}
					var siblings []SymbolRef
					for _, c := range candidates {
						if c.Score < 0.75 {
							break
						}
						if c.ID == sym.ID || selectedIDSet[c.ID] {
							continue
						}
						s2, ok2 := allSymsByID[c.ID]
						if !ok2 {
							continue
						}
						siblings = append(siblings, SymbolRef{
							QualifiedName: s2.QualifiedName,
							File:          allFilePaths[s2.FileID],
							Lines:         fmt.Sprintf("%d-%d", s2.StartLine, s2.EndLine),
							Kind:          s2.Kind,
						})
						if len(siblings) >= 3 {
							break
						}
					}
					scored[i].Siblings = siblings
				}
			}
		}
	}

	return nil
}

func callSiteEvidence(body string, symbolStartLine, callLine int) string {
	const (
		linesBefore  = 3
		linesAfter   = 1
		maxLineRunes = 120
	)
	if body == "" || symbolStartLine <= 0 || callLine < symbolStartLine {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	callIndex := callLine - symbolStartLine
	if callIndex < 0 || callIndex >= len(lines) {
		return ""
	}
	start := callIndex - linesBefore
	if start < 0 {
		start = 0
	}
	end := callIndex + linesAfter
	if end >= len(lines) {
		end = len(lines) - 1
	}
	branchSensitive := false
	for i := start; i <= callIndex; i++ {
		if branchControlLine(strings.TrimSpace(lines[i])) {
			branchSensitive = true
			break
		}
	}
	if !branchSensitive {
		return ""
	}
	parts := make([]string, 0, end-start+1)
	for i := start; i <= end; i++ {
		line := strings.TrimSpace(strings.TrimSuffix(lines[i], "\r"))
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > maxLineRunes {
			line = string(runes[:maxLineRunes]) + "…"
		}
		parts = append(parts, fmt.Sprintf("%d %s", symbolStartLine+i, line))
	}
	return strings.Join(parts, " | ")
}

func branchControlLine(line string) bool {
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	for _, prefix := range []string{
		"if ", "if(", "if (", "else", "switch ", "switch(", "switch (",
		"case ", "case:", "default:", "select ", "match ", "when ",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// callableKind reports whether a symbol kind can meaningfully be the target
// of a call edge; edges into consts/vars/types are suffix-collision noise.
func callableKind(kind string) bool {
	switch kind {
	case store.KindConst, store.KindVar, store.KindType:
		return false
	}
	return true
}

func symbolPackage(qualifiedName string) string {
	idx := strings.LastIndex(qualifiedName, ".")
	if idx < 0 {
		return qualifiedName
	}
	return qualifiedName[:idx]
}

func buildNextSteps(req Request, scored []ScoredResult) *NextStepsHints {
	if len(scored) == 0 {
		return nil
	}
	hints := &NextStepsHints{}

	top := scored[0]
	if top.Confidence == "high" {
		callerNames := make([]string, 0, 3)
		for _, c := range top.Callers {
			callerNames = append(callerNames, c.QualifiedName)
			if len(callerNames) >= 3 {
				break
			}
		}
		if len(callerNames) > 0 {
			hints.IfTopCorrect = fmt.Sprintf("Read #1. Blast radius: %d callers (%s).", len(top.Callers), strings.Join(callerNames, ", "))
		} else {
			hints.IfTopCorrect = "Read #1. No callers found — likely a leaf or entry point."
		}
	} else if len(scored) >= 2 {
		hints.IfTopCorrect = fmt.Sprintf("Confidence is %s. Consider top-%d before acting.", top.Confidence, min3(len(scored), 3))
	}

	if req.OutputMode == OutputModeExplore && len(scored) >= 2 {
		sc0 := scored[0].Score
		sc1 := scored[1].Score
		if sc0 > 0 && (sc0-sc1)/sc0 < 0.15 {
			descs := make([]string, 0, 3)
			for i := 0; i < min3(len(scored), 3); i++ {
				short := scored[i].QualifiedName
				if idx := strings.LastIndex(short, "."); idx >= 0 {
					short = short[idx+1:]
				}
				descs = append(descs, fmt.Sprintf("#%d=%s", i+1, short))
			}
			hints.IfUnsure = fmt.Sprintf("Top-3 are similar candidates. %s. Pick by what your task needs.", strings.Join(descs, ", "))
		}

		pkgs := make(map[string]bool)
		for _, sr := range scored {
			pkgs[symbolPackage(sr.QualifiedName)] = true
		}
		queryWords := strings.Fields(req.Query)
		var suggestions []string
		for pkg := range pkgs {
			pkgShort := pkg
			if idx := strings.LastIndex(pkgShort, "/"); idx >= 0 {
				pkgShort = pkgShort[idx+1:]
			}
			if len(queryWords) > 0 {
				suggestions = append(suggestions, fmt.Sprintf("'%s %s'", queryWords[0], pkgShort))
			}
			if len(suggestions) >= 2 {
				break
			}
		}
		hints.ToExplore = suggestions
	}

	return hints
}

func buildStructureView(scored []ScoredResult) *StructureView {
	if len(scored) == 0 {
		return nil
	}
	pkgMap := make(map[string][]string)
	for _, sr := range scored {
		pkg := symbolPackage(sr.QualifiedName)
		pkgMap[pkg] = append(pkgMap[pkg], sr.QualifiedName)
	}

	var topPkg string
	var topCount int
	for pkg, qns := range pkgMap {
		if len(qns) > topCount {
			topPkg = pkg
			topCount = len(qns)
		}
	}

	summary := fmt.Sprintf("Top matches span %d package(s); most in %q (%d symbols).", len(pkgMap), topPkg, topCount)
	return &StructureView{
		Summary:     summary,
		PackageView: pkgMap,
	}
}

func min3(a, b int) int {
	if b < a {
		return b
	}
	return a
}

func normalizedScore(score, maxScore float32) float32 {
	if maxScore <= 0 {
		return 0
	}
	return score / maxScore
}

func effectiveAlpha(req Request, vectorSeeds []store.ScoredSymbol) float32 {
	if req.AlphaSet {
		return req.Alpha
	}
	const defaultAlpha = float32(0.7)
	if len(vectorSeeds) == 0 {
		return defaultAlpha
	}
	qTokens := tokenizeForOverlap(req.Query)
	top := vectorSeeds[0]
	overlap := max3(
		overlapRatio(qTokens, top.Name),
		overlapRatio(qTokens, top.QualifiedName),
		overlapRatio(qTokens, top.Signature),
	)
	var gap float32
	if len(vectorSeeds) == 1 || vectorSeeds[1].Score <= 0 {
		gap = 1
	} else {
		gap = (top.Score - vectorSeeds[1].Score) / top.Score
	}
	if overlap >= 0.60 {
		return 0.92
	}
	if overlap >= 0.35 && gap >= 0.20 {
		return 0.85
	}
	return defaultAlpha
}

func max3(a, b, c float32) float32 {
	if b > a {
		a = b
	}
	if c > a {
		a = c
	}
	return a
}

func protectTopVectorSeeds(topIDs []int64, vectorSeeds []store.ScoredSymbol, protectCount int) []int64 {
	if protectCount <= 0 || len(topIDs) == 0 || len(vectorSeeds) == 0 {
		return topIDs
	}
	if protectCount > len(vectorSeeds) {
		protectCount = len(vectorSeeds)
	}
	if protectCount > len(topIDs) {
		protectCount = len(topIDs)
	}
	seen := make(map[int64]bool, len(topIDs))
	protected := make([]int64, 0, protectCount)
	for i := 0; i < protectCount; i++ {
		id := vectorSeeds[i].ID
		if seen[id] {
			continue
		}
		protected = append(protected, id)
		seen[id] = true
	}
	out := make([]int64, 0, len(topIDs))
	out = append(out, protected...)
	for _, id := range topIDs {
		if seen[id] {
			continue
		}
		out = append(out, id)
		seen[id] = true
		if len(out) == len(topIDs) {
			break
		}
	}
	return out
}

// dotProduct computes the dot product of two float32 slices. For L2-normalized
// embeddings, this equals cosine similarity.
func dotProduct(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

// appendMissingTopPPR ensures top-N PPR-ranked candidates reach the rerank pool.
// FTS-introduced lexical matches in hybrid mode can crowd out PPR-only graph
// candidates that have low seed score; this protects them.
func appendMissingTopPPR(topIDs []int64, pprIDsByRank []int64, protectCount int) []int64 {
	if protectCount <= 0 || len(pprIDsByRank) == 0 {
		return topIDs
	}
	seen := make(map[int64]bool, len(topIDs)+protectCount)
	for _, id := range topIDs {
		seen[id] = true
	}
	out := append([]int64(nil), topIDs...)
	added := 0
	for _, id := range pprIDsByRank {
		if added >= protectCount {
			break
		}
		if seen[id] {
			continue
		}
		out = append(out, id)
		seen[id] = true
		added++
	}
	return out
}

// appendVectorSeedGraphNeighbors expands the candidate pool with 1-hop graph
// neighbors of the top-K vector seeds. Targets multi-hop scenarios where the
// expected sibling functions are call-graph-connected to a strong vector seed
// but didn't surface via PPR (which favors hub nodes).
func appendVectorSeedGraphNeighbors(topIDs []int64, vSeeds []store.ScoredSymbol, edges []store.Edge, topSeedK, protectCount int) []int64 {
	if protectCount <= 0 || len(vSeeds) == 0 || len(edges) == 0 {
		return topIDs
	}
	if topSeedK > len(vSeeds) {
		topSeedK = len(vSeeds)
	}
	seedSet := make(map[int64]bool, topSeedK)
	for i := 0; i < topSeedK; i++ {
		seedSet[vSeeds[i].ID] = true
	}
	// Collect 1-hop neighbors (both directions) of top vector seeds.
	neighborSet := make(map[int64]bool)
	for _, e := range edges {
		if seedSet[e.Src] {
			neighborSet[e.Dst] = true
		}
		if seedSet[e.Dst] {
			neighborSet[e.Src] = true
		}
	}
	seen := make(map[int64]bool, len(topIDs)+protectCount)
	for _, id := range topIDs {
		seen[id] = true
	}
	out := append([]int64(nil), topIDs...)
	added := 0
	for nid := range neighborSet {
		if added >= protectCount {
			break
		}
		if seen[nid] || seedSet[nid] {
			continue
		}
		out = append(out, nid)
		seen[nid] = true
		added++
	}
	return out
}

// unconditionalVectorSeedProtect is the number of top vector seeds that enter
// the rerank pool regardless of lexical overlap with the query.
// DECISION(2026-06): the overlap>=0.25 gate disabled protection exactly for
// paraphrastic queries (zero identifier overlap), where hybrid FTS noise
// crowds strong vector seeds out of the pool (gen-corpus ctx_g_alpha_balance:
// vector seed rank 11 -> hybrid fused rank 23, expected symbol lost).
// ASSUMES: cross-encoder can demote irrelevant protected seeds.
// REVISIT IF: rerank p95 or trap-rate regresses on the gen corpus.
const unconditionalVectorSeedProtect = 12

func appendMissingTopVectorSeeds(topIDs []int64, vectorSeeds []store.ScoredSymbol, protectCount int, qTokens []string) []int64 {
	if protectCount <= 0 || len(vectorSeeds) == 0 {
		return topIDs
	}
	if protectCount > len(vectorSeeds) {
		protectCount = len(vectorSeeds)
	}
	seen := make(map[int64]bool, len(topIDs)+protectCount)
	for _, id := range topIDs {
		seen[id] = true
	}
	out := append([]int64(nil), topIDs...)
	for i := 0; i < protectCount; i++ {
		seed := vectorSeeds[i]
		if i >= unconditionalVectorSeedProtect {
			overlap := max3(
				overlapRatio(qTokens, seed.Name),
				overlapRatio(qTokens, seed.QualifiedName),
				overlapRatio(qTokens, seed.Signature),
			)
			if overlap < 0.25 {
				continue
			}
		}
		id := seed.ID
		if seen[id] {
			continue
		}
		out = append(out, id)
		seen[id] = true
	}
	return out
}

func degreePenalty(g *Graph, id int64, maxDegree int) float32 {
	if g == nil || maxDegree <= 0 {
		return 0
	}
	i, ok := g.NodeIdx[id]
	if !ok {
		return 0
	}
	return float32(len(g.OutEdges[i])) / float32(maxDegree)
}

func overlapRatio(queryTokens []string, text string) float32 {
	if len(queryTokens) == 0 || text == "" {
		return 0
	}
	textTokens := tokenizeForOverlap(text)
	if len(textTokens) == 0 {
		return 0
	}
	textSet := make(map[string]struct{}, len(textTokens))
	for _, tok := range textTokens {
		textSet[tok] = struct{}{}
	}
	var hits int
	for _, tok := range queryTokens {
		if _, ok := textSet[tok]; ok {
			hits++
		}
	}
	return float32(hits) / float32(len(queryTokens))
}

func tokenizeForOverlap(text string) []string {
	seen := make(map[string]struct{})
	var tokens []string
	for _, tok := range strings.FieldsFunc(strings.ToLower(splitCamelForOverlap(text)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		tok = normalizeOverlapToken(tok)
		if len(tok) < 2 {
			continue
		}
		if _, ok := seen[tok]; ok {
			continue
		}
		seen[tok] = struct{}{}
		tokens = append(tokens, tok)
	}
	return tokens
}

func splitCamelForOverlap(text string) string {
	var b strings.Builder
	var prev rune
	for i, r := range text {
		if i > 0 && unicode.IsUpper(r) {
			nextLower := false
			if i+1 < len(text) {
				next, _ := utf8.DecodeRuneInString(text[i+1:])
				nextLower = unicode.IsLower(next)
			}
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || nextLower {
				b.WriteRune(' ')
			}
		}
		b.WriteRune(r)
		prev = r
	}
	return b.String()
}

func normalizeOverlapToken(tok string) string {
	switch tok {
	case "children":
		return "child"
	case "employees":
		return "employee"
	case "indices":
		return "index"
	case "initialized", "initialize", "initializes", "init":
		return "new"
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

const (
	// escalationGapThreshold: relative top1-top2 gap below which the fast
	// ranking is considered ambiguous.
	escalationGapThreshold = float32(0.10)
	// escalationTiedThreshold: number of top-5 candidates within 10% of top-1
	// at which the fast ranking is considered ambiguous.
	escalationTiedThreshold = 3
)

// rankingAmbiguous reports whether a ranking is ambiguous enough to justify
// a (stronger) reranker pass: small relative top1-top2 gap or several
// near-tied candidates. Mirrors the buildRetrievalHealth gap/tied logic.
// Used both for escalation and for the lazy-rerank gate.
func rankingAmbiguous(scored []ScoredResult) bool {
	if len(scored) < 2 {
		return false
	}
	top1 := scored[0].Score
	if top1 <= 0 {
		return true
	}
	gap := (top1 - scored[1].Score) / top1
	tied := 0
	limit := 5
	if len(scored) < limit {
		limit = len(scored)
	}
	for _, sr := range scored[:limit] {
		if (top1-sr.Score)/top1 <= 0.10 {
			tied++
		}
	}
	return gap < escalationGapThreshold || tied >= escalationTiedThreshold
}

func shouldSkipRerank(req Request, scored []ScoredResult) bool {
	if !req.AdaptiveRerank || len(scored) == 0 {
		return false
	}
	top := scored[0]
	query := strings.ToLower(req.Query)
	name := strings.ToLower(top.QualifiedName)
	shortName := name
	if idx := strings.LastIndex(shortName, "."); idx >= 0 {
		shortName = shortName[idx+1:]
	}
	queryCompact := strings.NewReplacer(" ", "", "_", "", "-", "").Replace(query)
	nameCompact := strings.NewReplacer(" ", "", "_", "", "-", "").Replace(shortName)

	exactName := nameCompact != "" && strings.Contains(queryCompact, nameCompact)
	constructorIntent := strings.Contains(query, "constructor") || strings.Contains(query, "new ")
	confidentConstructor := constructorIntent && top.Features.IsConstructor > 0 && exactName
	if confidentConstructor || exactName && top.Features.NameOverlap >= 0.5 {
		return true
	}

	// DECISION: high name overlap on top-1 means identifier match alone is very
	// likely sufficient; cross-encoder won't change the winner in practice.
	if top.Features.NameOverlap >= 0.8 {
		return true
	}

	// DECISION: a large relative score gap to top-2 means we have strong signal
	// from seed+PPR alone; reranking would only consume latency budget.
	if len(scored) >= 2 && scored[0].Score > 0 {
		gap := (scored[0].Score - scored[1].Score) / scored[0].Score
		if gap >= 0.5 {
			return true
		}
	}

	// DECISION: single/double-word queries are likely identifier lookups; the
	// vector embedding captures the symbol name well enough without cross-encoder.
	if len(strings.Fields(req.Query)) <= 2 && top.Why == "vector_seed" {
		return true
	}

	return false
}

// rrf fuses two ranked lists via Reciprocal Rank Fusion.
// k=60 is the standard RRF constant.
func rrf(lists [][]int64) map[int64]float32 {
	scores := make(map[int64]float32)
	for _, list := range lists {
		for rank, id := range list {
			scores[id] += 1.0 / float32(rrfK+rank+1)
		}
	}
	return scores
}

// --- Enrichment: Feature 1 - Visibility flag ---

// assignVisibility sets Visibility on each result using a simple heuristic:
// Go: uppercase first char of short name -> "public", else "internal".
// TS/JS: non-test file -> "public", test file -> "internal".
// DECISION: language detection by file extension only to avoid AST parsing cost.
func assignVisibility(scored []ScoredResult) {
	for i := range scored {
		scored[i].Visibility = visibilityFor(scored[i].QualifiedName, scored[i].File)
	}
}

func visibilityFor(qualifiedName, file string) string {
	ext := fileExt(file)
	isGo := ext == ".go"
	isTS := ext == ".ts" || ext == ".tsx" || ext == ".js" || ext == ".jsx"

	if isGo {
		shortName := qualifiedName
		if idx := strings.LastIndex(qualifiedName, "."); idx >= 0 {
			shortName = qualifiedName[idx+1:]
		}
		if shortName == "" {
			return "public"
		}
		r, _ := utf8.DecodeRuneInString(shortName)
		if unicode.IsUpper(r) {
			return "public"
		}
		return "internal"
	}
	if isTS {
		if strings.Contains(file, "_test") || strings.HasSuffix(file, ".test.ts") ||
			strings.HasSuffix(file, ".test.tsx") || strings.HasSuffix(file, ".spec.ts") {
			return "internal"
		}
		return "public"
	}
	return "public"
}

func fileExt(file string) string {
	for i := len(file) - 1; i >= 0; i-- {
		if file[i] == '.' {
			return file[i:]
		}
		if file[i] == '/' || file[i] == 92 {
			break
		}
	}
	return ""
}

func fileBasename(file string) string {
	base := file
	for i := len(file) - 1; i >= 0; i-- {
		if file[i] == '/' || file[i] == 92 {
			base = file[i+1:]
			break
		}
	}
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '.' {
			return base[:i]
		}
	}
	return base
}

func fileDir(file string) string {
	for i := len(file) - 1; i >= 0; i-- {
		if file[i] == '/' || file[i] == 92 {
			return file[:i]
		}
	}
	return ""
}

func isTestFile(path string) bool {
	return strings.HasSuffix(path, "_test.go") ||
		strings.HasSuffix(path, ".test.ts") ||
		strings.HasSuffix(path, ".test.tsx") ||
		strings.HasSuffix(path, ".spec.ts")
}

// --- Enrichment: Feature 5 - Retrieval health ---

// WeakMatchRelevance is the cross-encoder score below which the top result is
// reported as a weak match regardless of how far it leads the rest. On the
// jina-tiny sigmoid scale; calibrated by cmd/confcal and pinned by a parity
// test — a different reranker needs a new calibration run.
const WeakMatchRelevance = 0.70

// buildRetrievalHealth computes aggregate confidence signal about the retrieval.
// DECISION: TopScoreGap is relative (gap/top1) to be scale-independent.
// TiedCandidates counts top-5 within 10% of top1 to signal ambiguity.
func buildRetrievalHealth(selected []ScoredResult, candidatesSeen int) *RetrievalHealth {
	if len(selected) == 0 {
		return nil
	}

	h := &RetrievalHealth{
		CandidatesSeen: candidatesSeen,
	}

	top1 := selected[0].Score
	var top2 float32
	if len(selected) >= 2 {
		top2 = selected[1].Score
	}

	if top1 > 0 && top2 > 0 {
		h.TopScoreGap = (top1 - top2) / top1
	} else {
		h.TopScoreGap = 1
	}

	limit := 5
	if len(selected) < limit {
		limit = len(selected)
	}
	for _, sr := range selected[:limit] {
		if top1 > 0 && (top1-sr.Score)/top1 <= 0.10 {
			h.TiedCandidates++
		}
	}

	switch {
	case h.TopScoreGap >= 0.20 && h.TiedCandidates <= 1:
		h.Confidence = "high"
	case h.TopScoreGap >= 0.08 || h.TiedCandidates <= 2:
		h.Confidence = "medium"
	default:
		h.Confidence = "low"
		// Mirrors the measured hook guidance (reformulation lifted Hit@1
		// 0.70->0.83): make the retry actionable, not generic.
		h.Suggestion = "Tied scores. Rephrase in the vocabulary the code would use (mechanism nouns/verbs, likely identifier words), name the deciding function or action, one mechanism per query."
	}

	// DECISION(2026-07): absolute floor on the cross-encoder's own verdict.
	// The gap logic above only asks "does one candidate lead the others" — it
	// cannot ask "is the leader a match at all", so a query about a concept
	// ABSENT from the repo reads confident whenever one junk result leads
	// (measured: 42-67% of absent-concept queries came back unflagged).
	// Relevance == 0 means the reranker never ran (adaptive skip / failure):
	// no verdict is not the same as a weak verdict, so the floor stays out of
	// it rather than defaulting to "suspicious".
	// Calibrated with cmd/confcal; RECALIBRATED 2026-08 after the intent
	// weight changed. Lowering the priors moved the cross-encoder's own pick
	// to rank 1, so top-1 relevance on real answers shifted up (gen p05
	// 0.545 -> 0.689, cockroach 0.688 -> 0.893) and the old 0.50 floor stopped
	// firing: false confidence regressed on 8 of 20 repos. At 0.70 the floor
	// catches 60/60 (gen), 11/12 (rails), 8/12 (cockroach) absent-concept
	// queries for 2-5% false alarms, most of which are the no-rerank samples.
	// A ranking change REQUIRES rerunning this calibration.
	// REVISIT IF: the reranker model changes — the scale is that model's.
	if rel := selected[0].Relevance; rel > 0 && rel < WeakMatchRelevance {
		h.Confidence = "low"
		h.Suggestion = "Weak match: no indexed symbol scores as a real answer to this query. The concept may not exist in this repo (check a dependency or a different service), or the wording may be far from the code's — retry once naming the mechanism as the code would."
	}

	return h
}

// --- Enrichment: Feature 6 - Companion files ---

// enrichCompanionFiles lists adjacent files (test, mock, gen) for each result.
// DECISION: Checks against file paths in scored results as proxy for indexed set.
// A full index scan would require an extra store call not worth the latency.
func enrichCompanionFiles(r *Retriever, scored []ScoredResult) {
	fileSet := make(map[string]bool, len(scored)*2)
	for _, sr := range scored {
		fileSet[sr.File] = true
	}

	for i := range scored {
		file := scored[i].File
		if file == "" {
			continue
		}
		dir := fileDir(file)
		base := fileBasename(file)
		if dir == "" {
			dir = "."
		}

		patterns := []string{
			dir + "/" + base + "_test.go",
			dir + "/" + base + "_mock.go",
			dir + "/" + base + ".test.ts",
			dir + "/" + base + ".test.tsx",
			dir + "/" + base + ".spec.ts",
			dir + "/" + base + ".gen.go",
			dir + "/mocks/" + base + ".go",
		}

		var companions []string
		for _, p := range patterns {
			if fileSet[p] {
				companions = append(companions, p)
				if len(companions) >= 4 {
					break
				}
			}
		}
		scored[i].CompanionFiles = companions
	}
}
