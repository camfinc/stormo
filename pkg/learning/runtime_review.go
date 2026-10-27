package learning

// ReflectionCandidate is private owner review material, never active policy.
type ReflectionCandidate struct {
	SkillDraft      *SkillDraft `json:"skillDraft,omitempty"`
	ID              string      `json:"id"`
	Text            string      `json:"text"`
	PreviousMistake string      `json:"previousMistake,omitempty"`
	Checks          string      `json:"checks"`
	Conditions      string      `json:"conditions"`
	Exceptions      string      `json:"exceptions"`
	Audience        string      `json:"audience"`
	Sensitivity     string      `json:"sensitivity"`
	ReviewDate      string      `json:"reviewDate"`
	Action          string      `json:"action"`
	RelatedIDs      []string    `json:"relatedIds"`
	EventRefs       []string    `json:"eventRefs"`
	Root            string      `json:"root"`
}

type RuntimeReviewStatus struct {
	ReflectionEnabled bool     `json:"reflectionEnabled"`
	CallsUsed         int      `json:"callsUsed"`
	CallsLimit        int      `json:"callsLimit"`
	EventsUsed        int      `json:"eventsUsed"`
	EventsLimit       int      `json:"eventsLimit"`
	CandidatesUsed    int      `json:"candidatesUsed"`
	CandidatesLimit   int      `json:"candidatesLimit"`
	ExportsUsed       int      `json:"exportsUsed"`
	ExportsLimit      int      `json:"exportsLimit"`
	ExportedIDs       []string `json:"exportedIds"`
	FeedbackAllowed   bool     `json:"feedbackAllowed"`
	ExportAllowed     bool     `json:"exportAllowed"`
}

type RuntimeReviewOperation struct {
	OK            bool   `json:"ok"`
	Code          string `json:"code"`
	Message       string `json:"message"`
	FeedbackSaved bool   `json:"feedbackSaved"`
}

type RuntimeReviewData struct {
	Skills     *SkillReviewData        `json:"skills,omitempty"`
	Background *BackgroundReviewData   `json:"background,omitempty"`
	Events     []OutcomeEvent          `json:"events"`
	Candidates []ReflectionCandidate   `json:"candidates"`
	Status     RuntimeReviewStatus     `json:"status"`
	Operation  *RuntimeReviewOperation `json:"operation,omitempty"`
}

// BackgroundReviewData is owner-only, scoped proposal material. Candidates here
// cannot be exported or activated through the legacy reflection operations.
type BackgroundReviewData struct {
	Jobs           []BackgroundJobView   `json:"jobs,omitempty"`
	CallsLimit     int                   `json:"callsLimit"`
	TokensLimit    int                   `json:"tokensLimit"`
	Enabled        bool                  `json:"enabled"`
	Pending        int                   `json:"pending"`
	Completed      int                   `json:"completed"`
	Failed         int                   `json:"failed"`
	Interrupted    int                   `json:"interrupted"`
	CallsUsed      int                   `json:"callsUsed"`
	TokensReserved int                   `json:"tokensReserved"`
	Candidates     []ReflectionCandidate `json:"candidates"`
}

type RuntimeReviewRequest struct {
	Action           string `json:"action,omitempty"`
	ID               string `json:"id,omitempty"`
	ExpectedRevision string `json:"expectedRevision,omitempty"`
	EventID          string `json:"eventId"`
	Outcome          string `json:"outcome"`
	Origin           string `json:"origin"`
	Summary          string `json:"summary"`
	ExportID         string `json:"exportId"`
}

// Valid rejects ambiguous operations before any write. No credential belongs in this payload.
func (r RuntimeReviewRequest) Valid() bool {
	if r.Action != "" {
		if r.EventID != "" || r.Outcome != "" || r.Origin != "" || r.Summary != "" || r.ExportID != "" || len(r.ID) > 256 || len(r.ExpectedRevision) > 128 {
			return false
		}
		switch r.Action {
		case "prune_jobs", "scan_maintenance":
			return r.ID == "" && r.ExpectedRevision == ""
		case "cancel_job", "retry_job", "reject_patch", "restore_skill":
			return r.ID != "" && r.ExpectedRevision == ""
		case "approve_patch", "rollback_patch", "archive_skill":
			return r.ID != "" && r.ExpectedRevision != ""
		default:
			return false
		}
	}
	if r.ID != "" || r.ExpectedRevision != "" {
		return false
	}
	if r.ExportID != "" {
		return len(r.ExportID) <= 128 && r.EventID == "" && r.Outcome == "" && r.Origin == "" && r.Summary == ""
	}
	if r.EventID == "" || len(r.EventID) > 128 || r.Origin == "" || len(r.Origin) > 128 || len(r.Summary) > 2000 {
		return false
	}
	if r.Outcome != "succeeded" && r.Outcome != "failed" && r.Outcome != "corrected" {
		return false
	}
	for _, c := range r.Origin {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
