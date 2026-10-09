// Package models lists the models an API connection offers (stormo connections models), for the
// app's model picker: OpenRouter's public catalog, an OpenAI-compatible /models with the key, or
// Anthropic's /v1/models. The key is only sent to the connection's own base URL.
package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/instance"
)

// Model is one model a connection serves.
type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextLength int    `json:"contextLength,omitempty"`
}

// Client is the HTTP client List uses (tests swap it).
var Client = &http.Client{Timeout: 20 * time.Second}

// List asks the connection for its models; key is its API key's value ("" when unset).
func List(ctx context.Context, c instance.Connection, key string) ([]Model, error) {
	if !c.API() {
		return nil, fmt.Errorf("%s is a ChatGPT sign-in: its models come from the core", c.Name)
	}
	url := strings.TrimSuffix(c.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	switch {
	case c.Kind == instance.KindAnthropic:
		if key == "" {
			return nil, fmt.Errorf("%s has no value yet: set it to list %s's models", c.Key, c.Name)
		}
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		q := req.URL.Query()
		q.Set("limit", "1000")
		req.URL.RawQuery = q.Encode()
	case key != "":
		req.Header.Set("Authorization", "Bearer "+key)
	case c.Kind != instance.KindOpenRouter: // OpenRouter's catalog is public
		return nil, fmt.Errorf("%s has no value yet: set it to list %s's models", c.Key, c.Name)
	}
	res, err := Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.Name, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: GET /models answered HTTP %d", c.Name, res.StatusCode)
	}
	var page struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			DisplayName   string `json:"display_name"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("%s: /models is not a model list: %w", c.Name, err)
	}
	out := []Model{}
	for _, d := range page.Data {
		if d.ID == "" {
			continue
		}
		out = append(out, Model{ID: d.ID, Name: firstOf(d.Name, d.DisplayName), ContextLength: d.ContextLength})
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
