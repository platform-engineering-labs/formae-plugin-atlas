// © 2026 Platform Engineering Labs Inc.
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// integration_helpers_test.go — shared scaffolding for plugin integration
// tests. Spins up an ephemeral Postgres container per test, writes
// versioned migration files into a temp dir, generates atlas.sum so
// atlas accepts the directory.
//
// All helpers are test-only (this file uses the `integration` build tag).
// Tests skip cleanly if docker or the atlas binary are unavailable so
// developers without the full toolchain still get a green test run.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"

	_ "github.com/lib/pq"
)

// =============================================================================
// Postgres container helper
// =============================================================================

// pgContainer is a handle to a running Postgres container. Cleanup is
// automatic via t.Cleanup() registered by startPostgres.
type pgContainer struct {
	ID       string
	Host     string
	Port     int
	User     string
	Password string
	Database string
}

// startPostgres launches a fresh `postgres:15-alpine` container on a
// random local port. Test is t.Skip()'d if docker isn't available so
// the suite remains runnable on workstations without docker.
func startPostgres(t *testing.T) *pgContainer {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not found on PATH: %v", err)
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}

	cmd := exec.Command("docker", "run", "-d",
		"--rm",
		"-p", fmt.Sprintf("%d:5432", port),
		"-e", "POSTGRES_PASSWORD=test",
		"-e", "POSTGRES_DB=testdb",
		"postgres:15-alpine",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("docker run failed (is docker daemon running?): %v\n%s", err, string(out))
	}
	id := strings.TrimSpace(string(out))

	t.Cleanup(func() {
		_ = exec.Command("docker", "kill", id).Run()
	})

	pg := &pgContainer{
		ID:       id,
		Host:     "127.0.0.1",
		Port:     port,
		User:     "postgres",
		Password: "test",
		Database: "testdb",
	}

	waitForPostgresReady(t, pg)
	return pg
}

// freePort picks an ephemeral TCP port the OS is willing to bind. The
// brief window between Close and docker -p binding is racy in theory
// but reliable in practice for test ports.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForPostgresReady polls the Postgres TCP port and tries a SELECT 1
// until it succeeds or the deadline fires.
func waitForPostgresReady(t *testing.T, pg *pgContainer) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	connStr := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		pg.User, pg.Password, pg.Host, pg.Port, pg.Database)
	for time.Now().Before(deadline) {
		db, err := sql.Open("postgres", connStr)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			row := db.QueryRowContext(ctx, "SELECT 1")
			var n int
			err = row.Scan(&n)
			cancel()
			_ = db.Close()
			if err == nil && n == 1 {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("postgres container %s did not become ready within deadline", pg.ID)
}

// queryAppliedVersion reads the latest version from atlas_schema_revisions.
// Returns ("", nil) when the table doesn't exist.
func queryAppliedVersion(t *testing.T, pg *pgContainer) string {
	t.Helper()
	connStr := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		pg.User, pg.Password, pg.Host, pg.Port, pg.Database)
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	var v string
	err = db.QueryRow(`SELECT version FROM atlas_schema_revisions.atlas_schema_revisions
		ORDER BY executed_at DESC LIMIT 1`).Scan(&v)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") || err == sql.ErrNoRows {
			return ""
		}
		t.Fatalf("queryAppliedVersion: %v", err)
	}
	return v
}

// =============================================================================
// Migration directory helper
// =============================================================================

// migrationFile represents one SQL migration in a versioned directory.
type migrationFile struct {
	Version string // e.g. "20240101000001"
	Name    string // e.g. "create_users"
	SQL     string // CREATE TABLE ...
}

// writeMigrationsDir lays out a temp dir with the given migrations and
// runs `atlas migrate hash` so atlas accepts the directory. Returns a
// file:// URI for use as migrationsUri.
func writeMigrationsDir(t *testing.T, files []migrationFile) string {
	t.Helper()
	if _, err := exec.LookPath("atlas"); err != nil {
		t.Skipf("atlas binary not on PATH: %v (run `make atlas-binary`)", err)
	}
	dir := t.TempDir()
	for _, f := range files {
		path := filepath.Join(dir, fmt.Sprintf("%s_%s.sql", f.Version, f.Name))
		if err := writeFile(path, f.SQL); err != nil {
			t.Fatalf("writeFile: %v", err)
		}
	}
	// atlas refuses to apply a directory without atlas.sum; generate it.
	cmd := exec.Command("atlas", "migrate", "hash", "--dir", "file://"+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("atlas migrate hash failed: %v\n%s", err, string(out))
	}
	return "file://" + dir
}

func writeFile(path, content string) error {
	cmd := exec.Command("sh", "-c", "cat > "+path)
	cmd.Stdin = strings.NewReader(content)
	return cmd.Run()
}

// =============================================================================
// CreateRequest builder
// =============================================================================

// makeCreateRequest assembles a CreateRequest from container + migration
// details. Uses UsernamePasswordCredentials so tests don't have to fake
// a secret payload.
func makeCreateRequest(t *testing.T, pg *pgContainer, label, migrationsUri, targetVersion string) *resource.CreateRequest {
	t.Helper()
	props := map[string]any{
		"migrationsUri": migrationsUri,
		"targetVersion": targetVersion,
		"tool":          "atlas",
		"allowDowngrade": false,
	}
	propsRaw, _ := json.Marshal(props)
	cfg := map[string]any{
		"dialect":  "postgres",
		"host":     pg.Host,
		"port":     pg.Port,
		"database": pg.Database,
		"sslMode":  "disable",
		"credentials": map[string]any{
			"type":     "UsernamePassword",
			"username": pg.User,
			"password": pg.Password,
		},
	}
	cfgRaw, _ := json.Marshal(cfg)
	return &resource.CreateRequest{
		ResourceType: "ATLAS::Schema::Migration",
		Label:        label,
		Properties:   propsRaw,
		TargetConfig: cfgRaw,
	}
}
