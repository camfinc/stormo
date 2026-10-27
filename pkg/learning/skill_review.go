package learning

// SkillDraft is a proposed exact replacement, never an executable tool call.
// The host binds the skill name and revision; the model supplies replacement text.
type SkillDraft struct {
	Name             string `json:"name"`
	ExpectedRevision string `json:"expectedRevision"`
	OldText          string `json:"oldText"`
	NewText          string `json:"newText"`
}

type BackgroundJobView struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	Attempts     int    `json:"attempts"`
	OwnerRetries int    `json:"ownerRetries"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
	Next         string `json:"next"`
	ResultCode   string `json:"resultCode"`
	CandidateID  string `json:"candidateId,omitempty"`
}

type SkillReviewData struct {
	Patches            []SkillPatchView       `json:"patches"`
	Maintenance        []SkillMaintenanceView `json:"maintenance"`
	Archives           []SkillArchiveView     `json:"archives"`
	LastScanAt         string                 `json:"lastScanAt,omitempty"`
	MaintenanceEnabled bool                   `json:"maintenanceEnabled"`
}

type SkillPatchView struct {
	EvidenceRefs     []string `json:"evidenceRefs,omitempty"`
	Checks           string   `json:"checks,omitempty"`
	Conditions       string   `json:"conditions,omitempty"`
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Status           string   `json:"status"`
	Before           string   `json:"before"`
	After            string   `json:"after"`
	ExpectedRevision string   `json:"expectedRevision"`
	CurrentRevision  string   `json:"currentRevision"`
	Audience         string   `json:"audience"`
	Reason           string   `json:"reason,omitempty"`
	CreatedAt        string   `json:"createdAt"`
	CanApprove       bool     `json:"canApprove"`
	CanRollback      bool     `json:"canRollback"`
}

type SkillMaintenanceView struct {
	Name         string `json:"name"`
	Action       string `json:"action"`
	Reason       string `json:"reason"`
	Revision     string `json:"revision"`
	LastActivity string `json:"lastActivity,omitempty"`
}

type SkillArchiveView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Revision  string `json:"revision"`
	CreatedAt string `json:"createdAt"`
}
