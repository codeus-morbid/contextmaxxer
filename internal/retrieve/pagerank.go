package retrieve

import "github.com/codeus-morbid/contextmaxxer/internal/store"

type Graph struct {
	NumNodes int
	NodeIdx  map[int64]int
	NodeIDs  []int64
	OutEdges [][]int
}

// DECISION: using undirected edges (both A→B and B→A per edge) for retrieval.
// Callers are equally relevant context as callees — if you query a helper function,
// you want to see its callers too. Bidirectionality makes PPR seeds propagate
// through the full local neighbourhood rather than only downstream.
func BuildGraph(symbolIDs []int64, edges []store.Edge) *Graph {
	idx := make(map[int64]int, len(symbolIDs))
	for i, id := range symbolIDs {
		idx[id] = i
	}
	n := len(symbolIDs)
	out := make([][]int, n)
	for _, e := range edges {
		si, okS := idx[e.Src]
		di, okD := idx[e.Dst]
		if !okS || !okD {
			continue
		}
		out[si] = append(out[si], di)
		out[di] = append(out[di], si)
	}
	return &Graph{
		NumNodes: n,
		NodeIdx:  idx,
		NodeIDs:  symbolIDs,
		OutEdges: out,
	}
}

// PersonalizedPageRank runs PPR with the given seed distribution.
// Seeds map symbolID → initial weight (need not sum to 1).
// damping=0.85, maxIter=30 are standard choices.
// Returns map symbolID → rank score (scores sum ≈ 1.0).
func PersonalizedPageRank(g *Graph, seeds map[int64]float32, damping float32, maxIter int) (map[int64]float32, int) {
	n := g.NumNodes
	if n == 0 {
		return nil, 0
	}

	p := make([]float32, n)
	var psum float32
	for id, w := range seeds {
		if i, ok := g.NodeIdx[id]; ok {
			p[i] = w
			psum += w
		}
	}
	if psum > 0 {
		for i := range p {
			p[i] /= psum
		}
	} else {
		uni := float32(1.0) / float32(n)
		for i := range p {
			p[i] = uni
		}
	}

	r := make([]float32, n)
	copy(r, p)
	rNew := make([]float32, n)

	var iters int
	for iter := 0; iter < maxIter; iter++ {
		for i := range rNew {
			rNew[i] = 0
		}

		var danglingMass float32
		for i, outN := range g.OutEdges {
			if len(outN) == 0 {
				danglingMass += r[i]
			}
		}
		dangleContrib := danglingMass / float32(n)

		for i, outN := range g.OutEdges {
			if len(outN) == 0 {
				continue
			}
			share := r[i] / float32(len(outN))
			for _, j := range outN {
				rNew[j] += share
			}
		}

		var l1 float32
		for i := range rNew {
			rNew[i] = (1-damping)*p[i] + damping*(rNew[i]+dangleContrib*p[i])
			diff := rNew[i] - r[i]
			if diff < 0 {
				diff = -diff
			}
			l1 += diff
		}

		r, rNew = rNew, r
		iters = iter + 1
		if l1 < 1e-6 {
			break
		}
	}

	result := make(map[int64]float32, n)
	for i, id := range g.NodeIDs {
		result[id] = r[i]
	}
	return result, iters
}
