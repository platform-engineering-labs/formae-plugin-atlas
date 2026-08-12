// © 2026 Platform Engineering Labs Inc.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
)

// Config is the Go-side mirror of the atlas.Config PKL class.
// It's the shape of the JSON blob passed to the plugin in TargetConfig
// on every CRUD request. Unmarshal from json.RawMessage via ParseConfig.
type Config struct {
	Dialect          string          `json:"Dialect,omitempty"`
	Host             string          `json:"Host,omitempty"`
	Port             int             `json:"Port,omitempty"`
	Database         string          `json:"Database,omitempty"`
	Credentials      json.RawMessage `json:"Credentials,omitempty"`
	SSLMode          string          `json:"SslMode,omitempty"`
	ConnectionString string          `json:"ConnectionString,omitempty"`
}

// ParseConfig unmarshals the TargetConfig JSON blob into a Config struct.
// Credentials is left as a raw message; call ParseCredentials separately
// to dispatch on the discriminator.
func ParseConfig(raw json.RawMessage) (*Config, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("invalid target config: %w", err)
	}
	return &cfg, nil
}

// =============================================================================
// Credentials
// =============================================================================

// Credentials is the discriminator-keyed interface for credential shapes.
// Concrete types unmarshal lazily from the `secret` JSON string field
// (or from inline username/password fields) when Username/Password are
// first called.
type Credentials interface {
	Username() (string, error)
	Password() (string, error)
}

// AwsRdsMasterCredentials parses an RDS-managed master secret with the
// standardized AWS shape: {"username":"...", "password":"...", "host":"...",
// "port":5432, "engine":"...", ...}. The `secret` field carries the entire
// `secretString` (a JSON-encoded string).
type AwsRdsMasterCredentials struct {
	TypeField string `json:"type"`   // "AwsRdsMaster"
	Secret    string `json:"secret"` // JSON-encoded payload
}

func (c *AwsRdsMasterCredentials) decode() (user, pass string, err error) {
	var payload struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal([]byte(c.Secret), &payload); err != nil {
		return "", "", fmt.Errorf("AwsRdsMasterCredentials.secret: invalid JSON: %w", err)
	}
	if payload.Username == "" {
		return "", "", fmt.Errorf("AwsRdsMasterCredentials.secret: missing 'username' field")
	}
	if payload.Password == "" {
		return "", "", fmt.Errorf("AwsRdsMasterCredentials.secret: missing 'password' field")
	}
	return payload.Username, payload.Password, nil
}

func (c *AwsRdsMasterCredentials) Username() (string, error) { u, _, err := c.decode(); return u, err }
func (c *AwsRdsMasterCredentials) Password() (string, error) { _, p, err := c.decode(); return p, err }

// UsernamePasswordJsonCredentials parses a hand-rolled JSON secret with
// shape {"username":"...", "password":"..."} — the minimal payload used
// when the operator manages secrets outside RDS's master-secret rotation.
type UsernamePasswordJsonCredentials struct {
	TypeField string `json:"type"`
	Secret    string `json:"secret"`
}

func (c *UsernamePasswordJsonCredentials) decode() (user, pass string, err error) {
	var payload struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal([]byte(c.Secret), &payload); err != nil {
		return "", "", fmt.Errorf("UsernamePasswordJsonCredentials.secret: invalid JSON: %w", err)
	}
	if payload.Username == "" {
		return "", "", fmt.Errorf("UsernamePasswordJsonCredentials.secret: missing 'username' field")
	}
	if payload.Password == "" {
		return "", "", fmt.Errorf("UsernamePasswordJsonCredentials.secret: missing 'password' field")
	}
	return payload.Username, payload.Password, nil
}

func (c *UsernamePasswordJsonCredentials) Username() (string, error) {
	u, _, err := c.decode()
	return u, err
}
func (c *UsernamePasswordJsonCredentials) Password() (string, error) {
	_, p, err := c.decode()
	return p, err
}

// UsernamePasswordCredentials carries username + password as direct fields.
// Use when neither value comes from a JSON-shaped secret (env vars, config
// files, ad-hoc strings).
type UsernamePasswordCredentials struct {
	TypeField string `json:"type"`
	User      string `json:"username"`
	Pass      string `json:"password"`
}

func (c *UsernamePasswordCredentials) Username() (string, error) {
	if c.User == "" {
		return "", fmt.Errorf("UsernamePasswordCredentials: 'username' is empty")
	}
	return c.User, nil
}
func (c *UsernamePasswordCredentials) Password() (string, error) {
	if c.Pass == "" {
		return "", fmt.Errorf("UsernamePasswordCredentials: 'password' is empty")
	}
	return c.Pass, nil
}

// ParseCredentials reads the `type` discriminator and unmarshals the
// payload into the matching concrete struct. Returns an error on unknown
// or missing type, but does NOT validate the inner payload — that's
// deferred to Username/Password so callers see field-level errors
// closer to where they consume the credentials.
func ParseCredentials(raw json.RawMessage) (Credentials, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("credentials: invalid JSON: %w", err)
	}
	switch probe.Type {
	case "AwsRdsMaster":
		var c AwsRdsMasterCredentials
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("AwsRdsMasterCredentials: %w", err)
		}
		return &c, nil
	case "UsernamePasswordJson":
		var c UsernamePasswordJsonCredentials
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("UsernamePasswordJsonCredentials: %w", err)
		}
		return &c, nil
	case "UsernamePassword":
		var c UsernamePasswordCredentials
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("UsernamePasswordCredentials: %w", err)
		}
		return &c, nil
	case "":
		return nil, fmt.Errorf("credentials: missing required 'type' field")
	default:
		return nil, fmt.Errorf("credentials: unknown type %q (supported: AwsRdsMaster, UsernamePasswordJson, UsernamePassword)", probe.Type)
	}
}
