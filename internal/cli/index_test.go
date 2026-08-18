package cli

import "testing"

func TestShouldForceReindexForContentUpgrade(t *testing.T) {
	tests := []struct {
		name             string
		requested        bool
		contentVersion   int
		wantForceReindex bool
	}{
		{name: "explicit force", requested: true, contentVersion: 3, wantForceReindex: true},
		{name: "pre-import-aware graph", contentVersion: 2, wantForceReindex: true},
		{name: "legacy content", contentVersion: 1, wantForceReindex: true},
		{name: "empty content", contentVersion: 0, wantForceReindex: true},
		{name: "current content", contentVersion: 3, wantForceReindex: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldForceReindex(tc.requested, tc.contentVersion); got != tc.wantForceReindex {
				t.Fatalf("shouldForceReindex(%v, %d) = %v, want %v", tc.requested, tc.contentVersion, got, tc.wantForceReindex)
			}
		})
	}
}
