package retrieve

import "sort"

// Prefer the member over the class that holds it.
//
// Measured on all 848 SWE-Explore instances: of the gold regions we miss having
// already found the file, 6.8% are cases where the right symbol WAS returned and
// the shown window fell elsewhere in it. Taking those apart, the symbol we
// returned has a median length of 296 lines while the symbol actually holding
// the answer has a median of 46 — and in 64% of them that finer symbol was in
// the index all along. We rank a container above its own member and then show
// 8% of the container.
//
// Nothing in the ranking pushes the other way. A container's indexed text spans
// everything inside it, so it matches more queries than any one member; its head
// is a class docstring, which reads like the prose of an issue report; and no
// stage carries a prior on how long a span is. The micro-symbol filter drops
// candidates for being too small and there is no counterpart for being too
// large.
//
// DECISION(2026-09): written, measured, NOT WIRED IN. The rule is sound and the
// diagnosis behind it holds; what it cannot do is reach the cases it was built
// for, because they are not in the answer to be reordered.
//
// Of the 36 trimmed misses where a finer symbol existed, the finer symbol was
// itself returned in 4 — so at the served depth this rule has about 0.5 points
// of gold regions to work with, which is noise. Deepening the answer to 100
// results raises that share from 11% to 41%, which says the member IS in the
// candidate pool and simply ranks far below its container. A rule that PROMOTED
// it from the pool, rather than reordering what came back, would have roughly
// 1.4 points to work with — still small, and paid for in precision, since
// promoting deep candidates is exactly what cost 0.09 precision in the
// max_results sweep.
//
// So the container problem is real and its remedy is not here: the member loses
// at ranking time because a container's indexed text spans everything inside it
// and therefore matches a vague issue report better than any one of its parts.
// That is a seeding question.
//
// Kept rather than deleted because the next person to have this idea should find
// it already measured. Reorder, never drop, if it is ever switched on: a class
// is legitimate context, and removing it would trade one kind of miss for
// another.
// REVISIT IF: served max_results grows past ~40, or seeding stops preferring
// containers, either of which changes the arithmetic above.

// preferNestedSymbols moves a contained candidate ahead of the candidate that
// contains it, leaving every other pair in its ranked order.
//
// Containment is checked on the file and the line span, which is what the index
// stores; a symbol is not treated as containing itself, and equal spans are left
// alone because neither is finer than the other.
func preferNestedSymbols(scored []ScoredResult) []ScoredResult {
	if len(scored) < 2 {
		return scored
	}
	// rank[i] is the position result i should sort to. Starting from the
	// existing order means a pair with no containment relation never moves.
	rank := make([]int, len(scored))
	for i := range rank {
		rank[i] = i
	}
	moved := false
	for i := range scored {
		for j := range scored {
			if i == j || scored[i].File != scored[j].File {
				continue
			}
			// j is strictly inside i.
			if scored[i].StartLine <= scored[j].StartLine &&
				scored[j].EndLine <= scored[i].EndLine &&
				(scored[i].EndLine-scored[i].StartLine) > (scored[j].EndLine-scored[j].StartLine) {
				if rank[j] > rank[i] {
					rank[j], rank[i] = rank[i], rank[j]
					moved = true
				}
			}
		}
	}
	if !moved {
		return scored
	}
	order := make([]int, len(scored))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return rank[order[a]] < rank[order[b]] })
	out := make([]ScoredResult, len(scored))
	for pos, idx := range order {
		out[pos] = scored[idx]
	}
	return out
}
