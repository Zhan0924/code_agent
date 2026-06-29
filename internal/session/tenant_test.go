package session

import "testing"

func TestNormalizeTenantIDs(t *testing.T) {
	tests := []struct {
		inUser, inProj, wantUser, wantProj string
	}{
		{"", "", AnonymousUserID, DefaultProjectID},
		{"alice", "", "alice", DefaultProjectID},
		{"", "p1", AnonymousUserID, "p1"},
		{"alice", "p1", "alice", "p1"},
	}
	for _, tc := range tests {
		gotUser, gotProj := NormalizeTenantIDs(tc.inUser, tc.inProj)
		if gotUser != tc.wantUser || gotProj != tc.wantProj {
			t.Fatalf("NormalizeTenantIDs(%q,%q) = (%q,%q), want (%q,%q)",
				tc.inUser, tc.inProj, gotUser, gotProj, tc.wantUser, tc.wantProj)
		}
	}
}
