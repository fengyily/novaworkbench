package model

import "time"

type Requirement struct {
	ID                 string     `json:"id"`
	ProjectID          string     `json:"project_id"`
	Title              string     `json:"title"`
	Description        string     `json:"description"`
	Status             string     `json:"status"`
	Priority           string     `json:"priority"`
	AcceptanceCriteria string     `json:"acceptance_criteria"` // JSON array
	DesignDocs         string     `json:"design_docs"`         // JSON array
	ConversationIDs    string     `json:"conversation_ids"`    // JSON array
	AssignedTo         string     `json:"assigned_to"`
	Sprint             string     `json:"sprint"`
	CreatedBy          string     `json:"created_by"`
	AnalysisSessionID  string     `json:"analysis_session_id"`
	DesignSessionID    string     `json:"design_session_id"`
	DesignJobID        string     `json:"design_job_id"` // active architect-design JobStore job id; empty when no design job is running
	CodingSessionID    string     `json:"coding_session_id"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
}

type CreateRequirementReq struct {
	ProjectID   string `json:"project_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    string `json:"priority"`
	Sprint      string `json:"sprint"`
}

type UpdateStatusReq struct {
	Status string `json:"status"`
}

type AnalysisResult struct {
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	TechnicalRisks     []string `json:"technical_risks"`
	RelatedModules     []string `json:"related_modules"`
	Summary            string   `json:"summary"`
}

type TechnDesign struct {
	Overview    string   `json:"overview"`
	Files       []string `json:"files"`
	Steps       []string `json:"steps"`
	ModelChanges string  `json:"model_changes"`
	Risks       []string `json:"risks"`
}
