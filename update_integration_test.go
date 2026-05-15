// © 2026 Platform Engineering Labs Inc.
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

// TestUpdate_AppliesNewMigrations is the canonical forward path: a DB
// at v1 receives an updated migration artifact containing v1+v2, and
// Update should apply v2 (leaving v1 untouched).
func TestUpdate_AppliesNewMigrations(t *testing.T) {
	pg := startPostgres(t)

	// Seed v1.
	v1Uri := writeMigrationsDir(t, []migrationFile{
		{Version: "20260101000001", Name: "v1", SQL: "CREATE TABLE t1 (id INT);"},
	})
	plugin := &Plugin{}
	createReq := makeCreateRequest(t, pg, "mig", v1Uri, "latest")
	if _, err := plugin.Create(context.Background(), createReq); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	// Artifact v2 contains both migrations.
	v2Uri := writeMigrationsDir(t, []migrationFile{
		{Version: "20260101000001", Name: "v1", SQL: "CREATE TABLE t1 (id INT);"},
		{Version: "20260102000001", Name: "v2", SQL: "CREATE TABLE t2 (id INT);"},
	})
	priorProps, _ := json.Marshal(MigrationProperties{
		MigrationsUri: v1Uri,
		TargetVersion: "latest",
	})
	desiredProps, _ := json.Marshal(MigrationProperties{
		MigrationsUri: v2Uri,
		TargetVersion: "latest",
	})
	updReq := &resource.UpdateRequest{
		NativeID:          "mig",
		ResourceType:      "ATLAS::Schema::Migration",
		Label:             "mig",
		PriorProperties:   priorProps,
		DesiredProperties: desiredProps,
		TargetConfig:      createReq.TargetConfig,
	}

	result, err := plugin.Update(context.Background(), updReq)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if result.ProgressResult.OperationStatus != resource.OperationStatusSuccess {
		t.Fatalf("status: got %q, msg=%q",
			result.ProgressResult.OperationStatus, result.ProgressResult.StatusMessage)
	}
	if got := queryAppliedVersion(t, pg); got != "20260102000001" {
		t.Errorf("applied version: got %q, want %q", got, "20260102000001")
	}
}

// TestUpdate_DowngradeBlockedByDefault verifies that asking to downgrade
// without allowDowngrade=true fails fast with a clear error — the
// canonical safety property from D2 in the design doc.
func TestUpdate_DowngradeBlockedByDefault(t *testing.T) {
	pg := startPostgres(t)

	migsUri := writeMigrationsDir(t, []migrationFile{
		{Version: "20260101000001", Name: "v1", SQL: "CREATE TABLE t1 (id INT);"},
		{Version: "20260102000001", Name: "v2", SQL: "CREATE TABLE t2 (id INT);"},
	})
	plugin := &Plugin{}
	createReq := makeCreateRequest(t, pg, "mig", migsUri, "latest")
	if _, err := plugin.Create(context.Background(), createReq); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	// Now request targetVersion=20260101000001 with allowDowngrade=false.
	priorProps, _ := json.Marshal(MigrationProperties{
		MigrationsUri: migsUri,
		TargetVersion: "latest",
	})
	desiredProps, _ := json.Marshal(MigrationProperties{
		MigrationsUri:  migsUri,
		TargetVersion:  "20260101000001",
		AllowDowngrade: false,
	})
	updReq := &resource.UpdateRequest{
		NativeID:          "mig",
		ResourceType:      "ATLAS::Schema::Migration",
		Label:             "mig",
		PriorProperties:   priorProps,
		DesiredProperties: desiredProps,
		TargetConfig:      createReq.TargetConfig,
	}

	result, err := plugin.Update(context.Background(), updReq)
	if err != nil {
		t.Fatalf("Update returned go error: %v", err)
	}
	if result.ProgressResult.OperationStatus != resource.OperationStatusFailure {
		t.Errorf("status: got %q, want Failure", result.ProgressResult.OperationStatus)
	}
	if !strings.Contains(result.ProgressResult.StatusMessage, "downgrade") {
		t.Errorf("message should explain downgrade was blocked; got: %q",
			result.ProgressResult.StatusMessage)
	}
}

// TestUpdate_NoOpForSameTarget verifies that re-running Update with the
// same target version is a no-op success (matches Atlas TF's idempotent
// semantics — migrate apply finds nothing pending and exits cleanly).
func TestUpdate_NoOpForSameTarget(t *testing.T) {
	pg := startPostgres(t)
	migsUri := writeMigrationsDir(t, []migrationFile{
		{Version: "20260101000001", Name: "v1", SQL: "CREATE TABLE t1 (id INT);"},
	})
	plugin := &Plugin{}
	createReq := makeCreateRequest(t, pg, "mig", migsUri, "latest")
	if _, err := plugin.Create(context.Background(), createReq); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	priorProps, _ := json.Marshal(MigrationProperties{
		MigrationsUri: migsUri,
		TargetVersion: "latest",
	})
	desiredProps, _ := json.Marshal(MigrationProperties{
		MigrationsUri: migsUri,
		TargetVersion: "latest",
	})
	updReq := &resource.UpdateRequest{
		NativeID:          "mig",
		ResourceType:      "ATLAS::Schema::Migration",
		Label:             "mig",
		PriorProperties:   priorProps,
		DesiredProperties: desiredProps,
		TargetConfig:      createReq.TargetConfig,
	}

	result, err := plugin.Update(context.Background(), updReq)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if result.ProgressResult.OperationStatus != resource.OperationStatusSuccess {
		t.Errorf("status: got %q, msg=%q", result.ProgressResult.OperationStatus,
			result.ProgressResult.StatusMessage)
	}
}
