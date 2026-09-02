package proc

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want Version
		ok   bool
	}{
		{"git version 2.43.0", V(2, 43, 0), true},
		{"bd version 1.2.2 (build abc123)", V(1, 2, 2), true},
		{"1.2.2", V(1, 2, 2), true},
		{"v1.2", V(1, 2, 0), true},
		{"gh version 2.55.0 (2024-08-20)\nhttps://github.com/cli/cli/releases/tag/v2.55.0", V(2, 55, 0), true},
		{"claude 1.0.83 (Claude Code)", V(1, 0, 83), true},
		{"tool 7", V(7, 0, 0), true},
		{"no digits here", Version{}, false},
		{"", Version{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ParseVersion(tc.in)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestVersion_CompareAndAtLeast(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0, V(1, 2, 3).Compare(V(1, 2, 3)))
	assert.Equal(t, -1, V(1, 2, 3).Compare(V(1, 3, 0)))
	assert.Equal(t, 1, V(2, 0, 0).Compare(V(1, 99, 99)))
	assert.Equal(t, -1, V(1, 2, 2).Compare(V(1, 2, 3)))
	assert.True(t, V(2, 43, 0).AtLeast(V(2, 30, 0)))
	assert.True(t, V(1, 2, 2).AtLeast(V(1, 2, 2)))
	assert.False(t, V(1, 2, 1).AtLeast(V(1, 2, 2)))
	assert.Equal(t, "1.2.3", V(1, 2, 3).String())
}
