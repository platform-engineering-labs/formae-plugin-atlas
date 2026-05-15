// © 2026 Platform Engineering Labs Inc.
//
// SPDX-License-Identifier: Apache-2.0

//go:build unit

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// =============================================================================
// ParseConfig
// =============================================================================

func TestParseConfig_AllFields(t *testing.T) {
	raw := json.RawMessage(`{
		"dialect": "postgres",
		"host": "db.example.com",
		"port": 5432,
		"database": "hub",
		"sslMode": "verify-ca",
		"credentials": {"type": "UsernamePassword", "username": "u", "password": "p"}
	}`)

	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Dialect != "postgres" {
		t.Errorf("Dialect: got %q, want %q", cfg.Dialect, "postgres")
	}
	if cfg.Host != "db.example.com" {
		t.Errorf("Host: got %q, want %q", cfg.Host, "db.example.com")
	}
	if cfg.Port != 5432 {
		t.Errorf("Port: got %d, want %d", cfg.Port, 5432)
	}
	if cfg.Database != "hub" {
		t.Errorf("Database: got %q, want %q", cfg.Database, "hub")
	}
	if cfg.SSLMode != "verify-ca" {
		t.Errorf("SSLMode: got %q, want %q", cfg.SSLMode, "verify-ca")
	}
	if len(cfg.Credentials) == 0 {
		t.Errorf("Credentials raw: empty")
	}
}

func TestParseConfig_InvalidJSON(t *testing.T) {
	_, err := ParseConfig(json.RawMessage(`{not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

// =============================================================================
// ParseCredentials
// =============================================================================

func TestParseCredentials_AwsRdsMaster(t *testing.T) {
	// The "secret" field is a JSON-encoded string (matches AWS Secrets Manager
	// behavior: secretString IS a string, the user serialized JSON into it).
	secretJSON := `{"username":"admin","password":"sekret","host":"x","port":5432,"engine":"postgres"}`
	raw := json.RawMessage(`{"type":"AwsRdsMaster","secret":` + jsonQuote(secretJSON) + `}`)

	creds, err := ParseCredentials(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	user, err := creds.Username()
	if err != nil || user != "admin" {
		t.Errorf("Username: got %q (err %v), want %q", user, err, "admin")
	}
	pass, err := creds.Password()
	if err != nil || pass != "sekret" {
		t.Errorf("Password: got %q (err %v), want %q", pass, err, "sekret")
	}
}

func TestParseCredentials_AwsRdsMaster_InvalidSecretJSON(t *testing.T) {
	raw := json.RawMessage(`{"type":"AwsRdsMaster","secret":"not json"}`)
	creds, err := ParseCredentials(raw)
	if err != nil {
		t.Fatalf("ParseCredentials should not fail (lazy decode); got error: %v", err)
	}
	if _, err := creds.Username(); err == nil {
		t.Errorf("Username() should fail on bad secret JSON, got nil error")
	}
}

func TestParseCredentials_AwsRdsMaster_MissingUsername(t *testing.T) {
	secretJSON := `{"password":"only"}`
	raw := json.RawMessage(`{"type":"AwsRdsMaster","secret":` + jsonQuote(secretJSON) + `}`)
	creds, err := ParseCredentials(raw)
	if err != nil {
		t.Fatalf("ParseCredentials: %v", err)
	}
	if _, err := creds.Username(); err == nil {
		t.Error("Username() should fail when secret lacks username, got nil")
	}
}

func TestParseCredentials_UsernamePasswordJson(t *testing.T) {
	secretJSON := `{"username":"u","password":"p"}`
	raw := json.RawMessage(`{"type":"UsernamePasswordJson","secret":` + jsonQuote(secretJSON) + `}`)
	creds, err := ParseCredentials(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	user, _ := creds.Username()
	pass, _ := creds.Password()
	if user != "u" || pass != "p" {
		t.Errorf("got %q/%q, want u/p", user, pass)
	}
}

func TestParseCredentials_UsernamePassword(t *testing.T) {
	raw := json.RawMessage(`{"type":"UsernamePassword","username":"u","password":"p"}`)
	creds, err := ParseCredentials(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	user, _ := creds.Username()
	pass, _ := creds.Password()
	if user != "u" || pass != "p" {
		t.Errorf("got %q/%q, want u/p", user, pass)
	}
}

func TestParseCredentials_UnknownType(t *testing.T) {
	raw := json.RawMessage(`{"type":"Mystery","secret":"x"}`)
	_, err := ParseCredentials(raw)
	if err == nil {
		t.Fatal("expected error for unknown type, got nil")
	}
	if !strings.Contains(err.Error(), "Mystery") {
		t.Errorf("error should name the unknown type, got: %v", err)
	}
}

func TestParseCredentials_MissingType(t *testing.T) {
	raw := json.RawMessage(`{"username":"u","password":"p"}`)
	_, err := ParseCredentials(raw)
	if err == nil {
		t.Fatal("expected error for missing type field, got nil")
	}
}

// =============================================================================
// BuildConnectionURL
// =============================================================================

func TestBuildConnectionURL_ConnectionStringTakesPrecedence(t *testing.T) {
	cfg := &Config{
		Host:             "ignored",
		Database:         "ignored",
		ConnectionString: "postgres://from-literal/db",
	}
	url, err := BuildConnectionURL(cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if url != "postgres://from-literal/db" {
		t.Errorf("got %q, want literal connection string", url)
	}
}

func TestBuildConnectionURL_StructuredFields_VerifyCa(t *testing.T) {
	cfg := &Config{
		Dialect:  "postgres",
		Host:     "db.example.com",
		Port:     5432,
		Database: "hub",
		SSLMode:  "verify-ca",
	}
	creds := &UsernamePasswordCredentials{TypeField: "UsernamePassword", User: "admin", Pass: "p@ss/word"}
	url, err := BuildConnectionURL(cfg, creds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "postgres://admin:p%40ss%2Fword@db.example.com:5432/hub?sslmode=verify-ca"
	if url != want {
		t.Errorf("got %q,\nwant %q", url, want)
	}
}

func TestBuildConnectionURL_SslDisable(t *testing.T) {
	cfg := &Config{
		Dialect:  "postgres",
		Host:     "localhost",
		Port:     5432,
		Database: "test",
		SSLMode:  "disable",
	}
	creds := &UsernamePasswordCredentials{TypeField: "UsernamePassword", User: "u", Pass: "p"}
	url, err := BuildConnectionURL(cfg, creds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(url, "sslmode=disable") {
		t.Errorf("url should contain sslmode=disable; got %q", url)
	}
}

func TestBuildConnectionURL_MissingHost(t *testing.T) {
	cfg := &Config{Dialect: "postgres", Database: "x", SSLMode: "disable"}
	_, err := BuildConnectionURL(cfg, &UsernamePasswordCredentials{User: "u", Pass: "p"})
	if err == nil {
		t.Fatal("expected error for missing host, got nil")
	}
}

func TestBuildConnectionURL_MissingDatabase(t *testing.T) {
	cfg := &Config{Dialect: "postgres", Host: "h", Port: 5432, SSLMode: "disable"}
	_, err := BuildConnectionURL(cfg, &UsernamePasswordCredentials{User: "u", Pass: "p"})
	if err == nil {
		t.Fatal("expected error for missing database, got nil")
	}
}

func TestBuildConnectionURL_UnsupportedDialect(t *testing.T) {
	cfg := &Config{Dialect: "mysql", Host: "h", Port: 3306, Database: "x", SSLMode: "disable"}
	_, err := BuildConnectionURL(cfg, &UsernamePasswordCredentials{User: "u", Pass: "p"})
	if err == nil {
		t.Fatal("expected error for unsupported dialect, got nil")
	}
}

// =============================================================================
// helpers
// =============================================================================

// jsonQuote wraps a raw string as a JSON string literal (escapes inner quotes).
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
