package model

import "time"

type Memory struct {
	ID        string     `json:"id"`
	ProjectID string     `json:"project_id"`
	Type      string     `json:"type"`
	Title     string     `json:"title"`
	Content   string     `json:"content"`
	Source    string     `json:"source"`
	FilePath  string     `json:"file_path"`
	Tags      string     `json:"tags"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`
}

type CreateMemoryReq struct {
	ProjectID string `json:"project_id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Tags      string `json:"tags"`
	ValidUntil string `json:"valid_until,omitempty"`
}
