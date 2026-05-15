// © 2026 Platform Engineering Labs Inc.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/url"
)

// BuildConnectionURL composes a libpq-compatible connection string from
// a parsed Config + Credentials. If Config.ConnectionString is set, it
// takes precedence and is returned as-is.
//
// The structured path: postgres://<user>:<pass>@<host>:<port>/<db>?sslmode=<mode>
//
// V1 supports postgres only. Other dialects error explicitly so the
// failure surfaces at config-validation time rather than at connect time.
func BuildConnectionURL(cfg *Config, creds Credentials) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("BuildConnectionURL: nil config")
	}
	if cfg.ConnectionString != "" {
		return cfg.ConnectionString, nil
	}

	dialect := cfg.Dialect
	if dialect == "" {
		dialect = "postgres"
	}
	if dialect != "postgres" {
		return "", fmt.Errorf("BuildConnectionURL: unsupported dialect %q (v1 supports postgres only)", dialect)
	}
	if cfg.Host == "" {
		return "", fmt.Errorf("BuildConnectionURL: 'host' is required when 'connectionString' is unset")
	}
	if cfg.Database == "" {
		return "", fmt.Errorf("BuildConnectionURL: 'database' is required when 'connectionString' is unset")
	}
	if creds == nil {
		return "", fmt.Errorf("BuildConnectionURL: credentials are required when 'connectionString' is unset")
	}

	user, err := creds.Username()
	if err != nil {
		return "", fmt.Errorf("BuildConnectionURL: %w", err)
	}
	pass, err := creds.Password()
	if err != nil {
		return "", fmt.Errorf("BuildConnectionURL: %w", err)
	}

	port := cfg.Port
	if port == 0 {
		port = 5432
	}

	sslMode := cfg.SSLMode
	if sslMode == "" {
		sslMode = "verify-ca"
	}

	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, pass),
		Host:   fmt.Sprintf("%s:%d", cfg.Host, port),
		Path:   "/" + cfg.Database,
	}
	q := u.Query()
	q.Set("sslmode", sslMode)
	u.RawQuery = q.Encode()

	return u.String(), nil
}
