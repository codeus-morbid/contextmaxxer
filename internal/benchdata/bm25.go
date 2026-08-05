package benchdata

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// BM25Index is a small in-memory BM25 (k1=1.2, b=0.75) with the same
// tokenization idea as the store's FTS layer: runs of letters/digits/
// underscore, lowercased. Used for seed fusion in corebench and for mining
// hard negatives in ftdata.
type BM25Index struct {
	docTF  []map[string]int
	docLen []int
	df     map[string]int
	avgLen float64
	n      int
}

func BM25Tokenize(s string) []string {
	var toks []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			toks = append(toks, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for _, r := range s {
		if r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return toks
}

func NewBM25Index(docs []CorpusDoc) *BM25Index {
	b := &BM25Index{
		docTF:  make([]map[string]int, len(docs)),
		docLen: make([]int, len(docs)),
		df:     map[string]int{},
		n:      len(docs),
	}
	total := 0
	for i, d := range docs {
		tf := map[string]int{}
		toks := BM25Tokenize(d.Text)
		for _, t := range toks {
			tf[t]++
		}
		b.docTF[i] = tf
		b.docLen[i] = len(toks)
		total += len(toks)
		for t := range tf {
			b.df[t]++
		}
	}
	if len(docs) > 0 {
		b.avgLen = float64(total) / float64(len(docs))
	}
	return b
}

// Rank scores the candidate rows for the query and returns them best-first.
func (b *BM25Index) Rank(query string, cand []int) []int {
	const k1, bp = 1.2, 0.75
	qtf := map[string]int{}
	for _, t := range BM25Tokenize(query) {
		qtf[t]++
	}
	type scored struct {
		row int
		s   float64
	}
	out := make([]scored, 0, len(cand))
	for _, row := range cand {
		var score float64
		for t := range qtf {
			tf, ok := b.docTF[row][t]
			if !ok {
				continue
			}
			df := b.df[t]
			idf := math.Log(1 + (float64(b.n)-float64(df)+0.5)/(float64(df)+0.5))
			norm := float64(tf) * (k1 + 1) / (float64(tf) + k1*(1-bp+bp*float64(b.docLen[row])/b.avgLen))
			score += idf * norm
		}
		out = append(out, scored{row, score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].s > out[j].s })
	rows := make([]int, len(out))
	for i, sc := range out {
		rows[i] = sc.row
	}
	return rows
}
