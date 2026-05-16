// © 2026 Platform Engineering Labs Inc.
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

// TestCreate_AppliesMigrations exercises the canonical happy path: a
// fresh Postgres, a single migration file, targetVersion=latest. After
// Create returns Success the revisions table should record the version.
func TestCreate_AppliesMigrations(t *testing.T) {
	pg := startPostgres(t)
	migrationsUri := writeMigrationsDir(t, []migrationFile{
		{
			Version: "20260101000001",
			Name:    "create_users",
			SQL:     "CREATE TABLE users (id INT PRIMARY KEY, email TEXT NOT NULL);",
		},
	})

	plugin := &Plugin{}
	req := makeCreateRequest(t, pg, "test-mig", migrationsUri, "latest")

	result, err := plugin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result == nil || result.ProgressResult == nil {
		t.Fatalf("Create returned nil result")
	}
	if result.ProgressResult.OperationStatus != resource.OperationStatusSuccess {
		t.Fatalf("status: got %q, want Success; message: %q",
			result.ProgressResult.OperationStatus, result.ProgressResult.StatusMessage)
	}
	// NativeID is synthesized from the Target's DB coordinates so it
	// matches what discovery would surface, not the user-supplied Label.
	wantNative := nativeIDFromConfig(&Config{Dialect: "postgres", Host: pg.Host, Port: pg.Port, Database: pg.Database})
	if result.ProgressResult.NativeID != wantNative {
		t.Errorf("NativeID: got %q, want %q (DSN-shaped synthetic)",
			result.ProgressResult.NativeID, wantNative)
	}

	// Assert the DB actually has the schema + revisions row.
	v := queryAppliedVersion(t, pg)
	if v != "20260101000001" {
		t.Errorf("DB applied version: got %q, want %q", v, "20260101000001")
	}

	// ResourceProperties should carry appliedVersion for downstream resolvables.
	if !strings.Contains(string(result.ProgressResult.ResourceProperties), `"appliedVersion":"20260101000001"`) {
		t.Errorf("ResourceProperties should include appliedVersion=20260101000001, got: %s",
			string(result.ProgressResult.ResourceProperties))
	}
}

// TestCreate_Idempotent verifies that running Create twice against the
// same DB succeeds both times — Atlas's migrate apply is naturally
// idempotent, and our plugin must preserve that property (matches the
// Atlas TF provider's behavior: no "already migrated, use import" gate).
func TestCreate_Idempotent(t *testing.T) {
	pg := startPostgres(t)
	migrationsUri := writeMigrationsDir(t, []migrationFile{
		{Version: "20260101000001", Name: "init", SQL: "CREATE TABLE t (id INT);"},
	})

	plugin := &Plugin{}
	req := makeCreateRequest(t, pg, "test-mig", migrationsUri, "latest")

	if _, err := plugin.Create(context.Background(), req); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// Second invocation should also succeed; nothing to apply.
	result, err := plugin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if result.ProgressResult.OperationStatus != resource.OperationStatusSuccess {
		t.Errorf("second Create status: got %q, want Success", result.ProgressResult.OperationStatus)
	}
}
