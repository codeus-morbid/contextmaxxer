package retrieve

import (
	"context"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// relatedStore answers body-text probes from a fixed table, so the test states
// what "rare" means rather than depending on a corpus.
type relatedStore struct {
	Store
	byName map[string][]store.ScoredSymbol
	paths  map[int64]string
	asked  []string
}

func (r *relatedStore) SearchByBodyText(_ context.Context, q string, _ int) ([]store.ScoredSymbol, error) {
	r.asked = append(r.asked, q)
	return r.byName[q], nil
}

func (r *relatedStore) GetFilesByIDs(_ context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	for _, id := range ids {
		out[id] = r.paths[id]
	}
	return out, nil
}

func sym(id, fileID int64, name string) store.ScoredSymbol {
	return store.ScoredSymbol{Symbol: store.Symbol{ID: id, FileID: fileID, QualifiedName: name}}
}

func manySymbols(n int, fileID int64) []store.ScoredSymbol {
	out := make([]store.ScoredSymbol, n)
	for i := range out {
		out[i] = sym(int64(1000+i), fileID, "noise")
	}
	return out
}

// The case this exists for: django__django-14376 renames the connection keys
// db -> database, and the file the agent failed to edit is the only other one
// carrying that literal. The common name must not decide the ranking.
func TestFindRelatedEditsRanksByRarityNotByCount(t *testing.T) {
	st := &relatedStore{
		byName: map[string][]store.ScoredSymbol{
			"passwd":   {sym(1, 10, "mysql.get_connection_params"), sym(2, 20, "mysql.DatabaseClient")},
			"settings": manySymbols(relatedProbeK, 30), // at the cap: everywhere, so evidence of nothing
		},
		paths: map[int64]string{10: "db/mysql/base.py", 20: "db/mysql/client.py", 30: "conf/global.py"},
	}

	got, err := FindRelatedEdits(context.Background(), st,
		[]string{"kwargs['passwd'] = settings['PASSWORD']"},
		[]string{"db/mysql/base.py"}, 5)
	require.NoError(t, err)
	require.Len(t, got, 1, "the edited file is excluded and the saturated name contributes nothing")
	assert.Equal(t, "db/mysql/client.py", got[0].File)
	assert.Contains(t, got[0].Shared, "passwd")
	assert.Contains(t, got[0].Symbols, "mysql.DatabaseClient")
}

// A diff is the natural thing to paste, and only its changed lines describe the
// change: context lines are the code that stayed the same and would drag in
// every neighbour that also left it alone.
func TestExtractRelatedNamesUsesOnlyChangedDiffLines(t *testing.T) {
	diff := "diff --git a/x.py b/x.py\n@@ -1,3 +1,3 @@\n unchangedName\n-oldKeyName\n+newKeyName\n"
	names := extractRelatedNames([]string{diff})
	assert.Contains(t, names, "oldKeyName")
	assert.Contains(t, names, "newKeyName")
	assert.NotContains(t, names, "unchangedName")
}

// Plain text has no +/- markers; treating it as a diff would discard all of it.
//
// Note what is NOT filtered here. Dropping short names would be the obvious
// cleanup and it would destroy the signal: in django__django-14376 the name
// that finds the missed file is the two-letter key "db". A noise word like
// "to" is the same length and cannot be told apart lexically, so both are
// extracted and rarity decides — "to" saturates the probe and contributes
// nothing, "db" matches two symbols and decides the answer.
func TestExtractRelatedNamesKeepsShortNamesForRarityToJudge(t *testing.T) {
	names := extractRelatedNames([]string{"renamed db to database and passwd to password"})
	assert.Contains(t, names, "database")
	assert.Contains(t, names, "passwd")
	assert.Contains(t, names, "db", "the two-letter key is the whole signal in the case this was built for")
	assert.Contains(t, names, "to", "kept deliberately: only the store can tell a rare name from a common one")
}

func TestFindRelatedEditsReturnsNothingWithoutNames(t *testing.T) {
	st := &relatedStore{byName: map[string][]store.ScoredSymbol{}, paths: map[int64]string{}}
	got, err := FindRelatedEdits(context.Background(), st, []string{"self return if else"}, nil, 5)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Empty(t, st.asked, "stop words are dropped before the store is touched")
}

// A test file matches on the same generic names as the code it exercises. On
// django__django-14376 three of them scored into the top five on "password"
// alone and one outranked client.py — the file the agent had actually missed.
// They are ordered after implementation, not dropped: a change often does
// belong in a test too.
func TestFindRelatedEditsPutsImplementationBeforeTests(t *testing.T) {
	st := &relatedStore{
		byName: map[string][]store.ScoredSymbol{
			"password": {sym(1, 10, "AdminTests"), sym(2, 20, "mysql.DatabaseClient")},
			"passwd":   {sym(3, 20, "mysql.DatabaseClient")},
		},
		paths: map[int64]string{10: "tests/admin_views/tests.py", 20: "db/mysql/client.py"},
	}
	got, err := FindRelatedEdits(context.Background(), st, []string{"password passwd"}, nil, 5)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "db/mysql/client.py", got[0].File, "implementation first")
	assert.Equal(t, "tests/admin_views/tests.py", got[1].File, "the test is kept, just not ahead of the answer")
}
