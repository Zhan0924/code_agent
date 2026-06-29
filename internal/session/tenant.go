package session

// DefaultProjectID is the canonical project_id when callers omit scope.
// Aligns with session.Manager.Create and memory HTTP handlers so Redis
// key prefixes like memory:anonymous:default:* stay consistent.
const DefaultProjectID = "default"

// NormalizeTenantIDs applies the shared (user_id, project_id) fallback used
// across session creation, memory HTTP handlers, and orchestrator extraction.
// Empty userID → AnonymousUserID; empty projectID → DefaultProjectID.
func NormalizeTenantIDs(userID, projectID string) (string, string) {
	if userID == "" {
		userID = AnonymousUserID
	}
	if projectID == "" {
		projectID = DefaultProjectID
	}
	return userID, projectID
}
