package elevenlabs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Voice is a voice an agent can speak with: the account's own (InAccount) or one of the Voice
// Library's (Owner is its sharer's public id, needed to add it to the account).
type Voice struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Category    string `json:"category,omitempty"` // premade, cloned, generated, professional…
	Gender      string `json:"gender,omitempty"`
	Age         string `json:"age,omitempty"`
	Accent      string `json:"accent,omitempty"`
	Descriptive string `json:"descriptive,omitempty"`
	UseCase     string `json:"useCase,omitempty"`
	Language    string `json:"language,omitempty"`
	Description string `json:"description,omitempty"`
	// PreviewURL is ElevenLabs' own sample of the voice (free to play).
	PreviewURL string `json:"previewUrl,omitempty"`
	InAccount  bool   `json:"inAccount"`
	Owner      string `json:"owner,omitempty"`
}

// Query narrows a listing. The library filters by every field; the account's voices by Search on
// the provider's side and the rest here, against their labels.
type Query struct {
	Search, Language, Gender, Age, Accent string
	// Limit is the most voices (1 to MaxListed; 0: 30).
	Limit int
}

func (q Query) limit() int {
	switch {
	case q.Limit <= 0:
		return 30
	case q.Limit > MaxListed:
		return MaxListed
	}
	return q.Limit
}

// Voices are the account's voices that match q (GET /v2/voices).
func (c *Client) Voices(ctx context.Context, q Query) ([]Voice, error) {
	v := url.Values{"page_size": {strconv.Itoa(MaxListed)}}
	if q.Search != "" {
		v.Set("search", q.Search)
	}
	raw, err := c.do(ctx, http.MethodGet, "/v2/voices", v, "", nil)
	if err != nil {
		return nil, fmt.Errorf("voices: %w", err)
	}
	var page struct {
		Voices []struct {
			ID          string            `json:"voice_id"`
			Name        string            `json:"name"`
			Category    string            `json:"category"`
			Labels      map[string]string `json:"labels"`
			Description string            `json:"description"`
			PreviewURL  string            `json:"preview_url"`
		} `json:"voices"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, errors.New("voices: the answer is not a voice list")
	}
	out := []Voice{}
	for _, x := range page.Voices {
		l := x.Labels
		d := Voice{ID: x.ID, Name: x.Name, Category: x.Category, Gender: l["gender"], Age: l["age"], Accent: l["accent"],
			Descriptive: firstOf(l["descriptive"], l["description"]), UseCase: firstOf(l["use_case"], l["use case"]),
			Language: l["language"], Description: x.Description, PreviewURL: x.PreviewURL, InAccount: true}
		if d.ID == "" || !matches(d, q) {
			continue
		}
		out = append(out, d)
		if len(out) == q.limit() {
			break
		}
	}
	return out, nil
}

// matches checks the account voice's labels against q's filters (case and spaces ignored).
func matches(v Voice, q Query) bool {
	same := func(want, got string) bool {
		norm := func(s string) string { return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), " ", "_") }
		return want == "" || norm(want) == norm(got)
	}
	return same(q.Gender, v.Gender) && same(q.Age, v.Age) && same(q.Accent, v.Accent) && same(q.Language, v.Language)
}

// Library are the Voice Library's voices that match q (GET /v1/shared-voices).
func (c *Client) Library(ctx context.Context, q Query) ([]Voice, error) {
	v := url.Values{"page_size": {strconv.Itoa(q.limit())}}
	for k, s := range map[string]string{"search": q.Search, "language": q.Language, "gender": q.Gender, "age": q.Age, "accent": q.Accent} {
		if s != "" {
			v.Set(k, s)
		}
	}
	raw, err := c.do(ctx, http.MethodGet, "/v1/shared-voices", v, "", nil)
	if err != nil {
		return nil, fmt.Errorf("voice library: %w", err)
	}
	var page struct {
		Voices []struct {
			Owner       string `json:"public_owner_id"`
			ID          string `json:"voice_id"`
			Name        string `json:"name"`
			Category    string `json:"category"`
			Gender      string `json:"gender"`
			Age         string `json:"age"`
			Accent      string `json:"accent"`
			Descriptive string `json:"descriptive"`
			UseCase     string `json:"use_case"`
			Language    string `json:"language"`
			Description string `json:"description"`
			PreviewURL  string `json:"preview_url"`
		} `json:"voices"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, errors.New("voice library: the answer is not a voice list")
	}
	out := []Voice{}
	for _, x := range page.Voices {
		if x.ID == "" {
			continue
		}
		out = append(out, Voice{ID: x.ID, Name: x.Name, Category: x.Category, Gender: x.Gender, Age: x.Age, Accent: x.Accent,
			Descriptive: x.Descriptive, UseCase: x.UseCase, Language: x.Language, Description: x.Description, PreviewURL: x.PreviewURL, Owner: x.Owner})
	}
	return out, nil
}

// Model is a speech model the account can use.
type Model struct {
	ID        string   `json:"id"`
	Name      string   `json:"name,omitempty"`
	Languages []string `json:"languages,omitempty"` // ISO codes
	// MaxChars is the most text one request may carry.
	MaxChars int `json:"maxChars,omitempty"`
}

// Models are the text-to-speech models (GET /v1/models), by id.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	raw, err := c.do(ctx, http.MethodGet, "/v1/models", nil, "", nil)
	if err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	var list []struct {
		ID        string `json:"model_id"`
		Name      string `json:"name"`
		TTS       bool   `json:"can_do_text_to_speech"`
		MaxChars  int    `json:"maximum_text_length_per_request"`
		Languages []struct {
			ID string `json:"language_id"`
		} `json:"languages"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, errors.New("models: the answer is not a model list")
	}
	out := []Model{}
	for _, m := range list {
		if !m.TTS || m.ID == "" {
			continue
		}
		langs := []string{}
		for _, l := range m.Languages {
			langs = append(langs, l.ID)
		}
		out = append(out, Model{ID: m.ID, Name: m.Name, Languages: langs, MaxChars: m.MaxChars})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
