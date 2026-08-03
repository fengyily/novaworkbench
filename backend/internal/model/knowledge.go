package model

import "time"

type Knowledge struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Title       string    `json:"title"`
	Content     string    `json:"content"`
	Category    string    `json:"category"`
	SourceType  string    `json:"source_type"`
	SourceRef   string    `json:"source_ref"`
	IsReviewed  bool      `json:"is_reviewed"`
	IsApproved  bool      `json:"is_approved"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type CreateKnowledgeReq struct {
	ProjectID  string `json:"project_id"`
	Title      string `json:"title"`
	Content    string `json:"content"`
	Category   string `json:"category"`
	SourceType string `json:"source_type"`
	SourceRef  string `json:"source_ref"`
}

type ReviewActionReq struct {
	IDs        []string `json:"ids"`
	Action     string   `json:"action"` // approve, reject, skip
	EditedTitle   string `json:"edited_title,omitempty"`
	EditedContent string `json:"edited_content,omitempty"`
}
