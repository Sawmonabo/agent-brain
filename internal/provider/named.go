package provider

// NamedIdentity is the identity for a per-project root that has no git
// remote: the human-chosen folder name under the reserved named/ namespace.
// It is THE contract shared by every enrollment surface (cli's enrollOne,
// the dashboard add flow), so the collision argument lives exactly once.
//
// named/<folderName> can never collide with a canonical remote-derived id:
// joinRemoteID (remote.go) requires a non-empty host AND a path containing
// "/", so every remote-derived id has at least 3 slash-separated segments
// (host/owner/repo, ...). named/<folderName> has exactly 2 — provided
// folderName itself is a single segment, which is what
// repo.ValidateFolderName's charset (no "/") guarantees at every prompt and
// what the daemon re-checks fail-closed on Track.
func NamedIdentity(folderName string) Identity {
	return Identity{ProjectID: "named/" + folderName, PreferredFolder: folderName}
}
