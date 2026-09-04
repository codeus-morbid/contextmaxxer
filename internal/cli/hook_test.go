package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFirstLocator(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			// Grep in content mode: the shape the suggestion is built for.
			name: "matching lines with line numbers",
			body: `{"mode":"content","content":"src/topics/posts.js:142:  const pid = await getLatestUndeletedPid();\nsrc/topics/recent.js:31:  return pid;"}`,
			want: "src/topics/posts.js:142",
		},
		{
			// files_with_matches has no line to offer; a bare path still gives
			// the agent something the index can resolve.
			name: "file names only",
			body: `{"mode":"files_with_matches","filenames":["internal/retrieve/fusion.go","internal/retrieve/packer.go"],"numFiles":2}`,
			want: "internal/retrieve/fusion.go",
		},
		{
			name: "a line number is preferred over a bare path that comes first",
			body: `{"filenames":["docs/notes.md"],"content":"internal/store/sqlite/sqlite.go:921:func (s *Store) SearchByText("}`,
			want: "internal/store/sqlite/sqlite.go:921",
		},
		{
			// A Windows path is JSON-escaped on the wire; separators are
			// normalized so the match does not stop at the first backslash.
			name: "windows separators",
			body: `{"content":"internal\\cli\\hook.go:57:  case \"post-find\":"}`,
			want: "internal/cli/hook.go:57",
		},

		// Nothing concrete to suggest: stay silent rather than invent a query.
		{name: "count mode", body: `{"mode":"count","numLines":12}`, want: ""},
		{name: "no matches", body: `{"mode":"content","content":"No matches found"}`, want: ""},
		{name: "empty", body: ``, want: ""},
		{
			// A bare filename with no directory could be prose as easily as a
			// path, and the locator branch would refuse it anyway.
			name: "bare filename is not a path",
			body: `{"content":"see config.py for details"}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, firstLocator([]byte(tc.body)))
		})
	}
}
