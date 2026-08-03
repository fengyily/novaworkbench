package model

import "time"

type PlatformToken struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Platform  string    `json:"platform"`
	BaseURL   string    `json:"base_url"`
	Token     string    `json:"token,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
