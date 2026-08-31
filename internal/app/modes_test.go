package app

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWithGlobalIndex(t *testing.T) {
	// `contextmaxxer --index X query "..."` used to answer from the DEFAULT
	// index without saying so: query declares its own -index, the global one was
	// never passed down, and both spell the same. The command exited 0 with
	// results from another repository.
	cases := []struct {
		name string
		set  bool
		path string
		args []string
		want []string
	}{
		{
			name: "global flag reaches the subcommand",
			set:  true, path: "/tmp/a.db",
			args: []string{"some query"},
			want: []string{"-index", "/tmp/a.db", "some query"},
		},
		{
			name: "an explicit subcommand flag wins",
			set:  true, path: "/tmp/a.db",
			args: []string{"-index", "/tmp/b.db", "some query"},
			want: []string{"-index", "/tmp/b.db", "some query"},
		},
		{
			name: "the = form counts as explicit too",
			set:  true, path: "/tmp/a.db",
			args: []string{"--index=/tmp/b.db", "some query"},
			want: []string{"--index=/tmp/b.db", "some query"},
		},
		{
			name: "an unset global flag forwards nothing",
			set:  false, path: ".contextmaxxer/index.db",
			args: []string{"some query"},
			want: []string{"some query"},
		},
		{
			name: "a query that merely contains the word is not a flag",
			set:  true, path: "/tmp/a.db",
			args: []string{"where is the index built"},
			want: []string{"-index", "/tmp/a.db", "where is the index built"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{cfg: Config{IndexPath: tc.path, IndexPathSet: tc.set}}
			require.Equal(t, tc.want, withGlobalIndex(a, tc.args))
		})
	}
}
