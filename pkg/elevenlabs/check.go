package elevenlabs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
)

// Permission is one thing a key may be allowed to do, what Stormo uses it for, and whether this
// key has it.
type Permission struct {
	Name    string `json:"name"`
	For     string `json:"for"`
	Allowed bool   `json:"allowed"`
}

// Report is what a key may do, and the account's characters this period when the key may read the
// account (nil otherwise).
type Report struct {
	Permissions []Permission `json:"permissions"`
	Characters  *Usage       `json:"characters,omitempty"`
}

// Usage is the account's characters used and allowed this billing period.
type Usage struct {
	Used  int `json:"used"`
	Limit int `json:"limit"`
}

// probeID is a voice id no account has: a request naming it is refused as invalid, never acted on.
const probeID = "stormo0permission0probe"

// Check asks, free of charge, what the key may do. ElevenLabs checks a key's permission before
// the request itself, so a request it can refuse as invalid (no file, an unknown voice) says whether
// the permission is there without transcribing, speaking or adding anything: missing_permissions
// means no, any other answer yes. An invalid key, or ElevenLabs out of reach, is an error.
func (c *Client) Check(ctx context.Context) (*Report, error) {
	var stt bytes.Buffer
	w := multipart.NewWriter(&stt)
	_ = w.WriteField("model_id", DefaultSTTModel)
	_ = w.Close()
	probes := []struct {
		name, use, method, path, contentType string
		body                                 []byte
	}{
		{"speech_to_text", "transcribing voice notes", http.MethodPost, "/v1/speech-to-text", w.FormDataContentType(), stt.Bytes()},
		{"text_to_speech", "speaking", http.MethodPost, "/v1/text-to-speech/" + probeID, "application/json", []byte(`{"text":""}`)},
		{"voices_read", "listing voices (the picker, an agent picking its own)", http.MethodGet, "/v2/voices?page_size=1", "", nil},
		{"voices_write", "adding a Voice Library voice to the account", http.MethodPost, "/v1/voices/add/" + probeID + "/" + probeID, "application/json", []byte(`{"new_name":""}`)},
		{"models_read", "listing speech models", http.MethodGet, "/v1/models", "", nil},
		{"user_read", "showing the characters left", http.MethodGet, "/v1/user/subscription", "", nil},
	}
	r := &Report{Permissions: []Permission{}}
	for _, p := range probes {
		raw, err := c.do(ctx, p.method, p.path, nil, p.contentType, bytes.NewReader(p.body))
		allowed := true
		var ae *APIError
		switch {
		case errors.As(err, &ae) && ae.Status == "invalid_api_key":
			return nil, ae
		case errors.As(err, &ae) && ae.Status == "missing_permissions":
			allowed = false
		case err != nil && ae == nil:
			return nil, err
		}
		r.Permissions = append(r.Permissions, Permission{Name: p.name, For: p.use, Allowed: allowed})
		if p.name == "user_read" && err == nil {
			var s struct {
				Used  int `json:"character_count"`
				Limit int `json:"character_limit"`
			}
			if json.Unmarshal(raw, &s) == nil {
				r.Characters = &Usage{Used: s.Used, Limit: s.Limit}
			}
		}
	}
	return r, nil
}
