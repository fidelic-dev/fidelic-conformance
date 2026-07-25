package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AuthConfig describes how the runner obtains a bearer token for a target.
// For v0 the only realistic type is "client_credentials"; the emulator uses
// "none" (it accepts any creds and the token endpoint is exercised by the
// auth-* conformance tests directly).
type AuthConfig struct {
	Type         string `yaml:"type"`
	TokenURL     string `yaml:"token_url"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
}

// Target is a single run destination: a base URL plus auth config.
type Target struct {
	Name    string     `yaml:"-"`
	BaseURL string     `yaml:"base_url"`
	Auth    AuthConfig `yaml:"auth"`

	// bearer is the token obtained via Authorize(); injected into data
	// requests (everything except the OAuth token endpoint itself).
	bearer string
}

type targetsFile struct {
	Targets map[string]*Target `yaml:"targets"`
}

// LoadTarget reads a targets YAML file and returns the named target.
func LoadTarget(path, name string) (*Target, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading targets file %q: %w", path, err)
	}
	var tf targetsFile
	if err := yaml.Unmarshal(raw, &tf); err != nil {
		return nil, fmt.Errorf("parsing targets file %q: %w", path, err)
	}
	t, ok := tf.Targets[name]
	if !ok {
		return nil, fmt.Errorf("target %q not found in %q", name, path)
	}
	t.Name = name
	if t.BaseURL == "" {
		return nil, fmt.Errorf("target %q has no base_url", name)
	}
	t.BaseURL = strings.TrimRight(t.BaseURL, "/")
	return t, nil
}

// Authorize obtains a bearer token for targets that need one
// (client_credentials). It is a no-op for auth type "none"/"" so the
// emulator, which accepts any creds, runs without a pre-step.
func (t *Target) Authorize(ctx context.Context) error {
	if t.Auth.Type == "" || t.Auth.Type == "none" {
		return nil
	}
	if t.Auth.Type != "client_credentials" {
		return fmt.Errorf("unsupported auth type %q", t.Auth.Type)
	}

	tokenURL := t.Auth.TokenURL
	if tokenURL == "" {
		tokenURL = t.BaseURL + "/services/oauth2/token"
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", t.Auth.ClientID)
	form.Set("client_secret", t.Auth.ClientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("token request to %q: %w", tokenURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token request to %q returned %d: %s", tokenURL, resp.StatusCode, string(body))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return fmt.Errorf("decoding token response: %w", err)
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("token response from %q had no access_token", tokenURL)
	}
	t.bearer = tok.AccessToken
	return nil
}

// Bearer returns the token obtained by Authorize (empty if none).
func (t *Target) Bearer() string { return t.bearer }
