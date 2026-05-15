// © 2026 Platform Engineering Labs Inc.
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package main

import (
	"context"
	"testing"

	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

// TestDelete_IsNoOpAndPreservesSchema mirrors Atlas TF's behavior
// exactly: Delete reports success without touching the DB. The schema
// and revisions table survive so a subsequent Create on the same DB
// resumes from the existing cursor.
func TestDelete_IsNoOpAndPreservesSchema(t *testing.T) {
	pg := startPostgres(t)
	migsUri := writeMigrationsDir(t, []migrationFile{
		{Version: "20260101000001", Name: "init", SQL: "CREATE TABLE keep_me (id INT);"},
	})
	plugin := &Plugin{}
	createReq := makeCreateRequest(t, pg, "mig", migsUri, "latest")
	if _, err := plugin.Create(context.Background(), createReq); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	delReq := &resource.DeleteRequest{
		NativeID:     "mig",
		ResourceType: "ATLAS::Schema::Migration",
		TargetConfig: createReq.TargetConfig,
	}
	result, err := plugin.Delete(context.Background(), delReq)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if result.ProgressResult.OperationStatus != resource.OperationStatusSuccess {
		t.Errorf("status: got %q, msg=%q",
			result.ProgressResult.OperationStatus, result.ProgressResult.StatusMessage)
	}

	// Schema should still be there — verify revisions table is intact.
	if got := queryAppliedVersion(t, pg); got != "20260101000001" {
		t.Errorf("DB state changed by Delete: revisions row got=%q, want=%q",
			got, "20260101000001")
	}
}

// TestDelete_OnFreshDBSucceeds — calling Delete with no resource state
// (no prior Create against this DB) is still a success: Delete is a
// no-op, there's nothing to remove either way.
func TestDelete_OnFreshDBSucceeds(t *testing.T) {
	pg := startPostgres(t)
	plugin := &Plugin{}
	delReq := &resource.DeleteRequest{
		NativeID:     "never-existed",
		ResourceType: "ATLAS::Schema::Migration",
		TargetConfig: configRawFor(pg),
	}
	result, err := plugin.Delete(context.Background(), delReq)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if result.ProgressResult.OperationStatus != resource.OperationStatusSuccess {
		t.Errorf("status: got %q, msg=%q",
			result.ProgressResult.OperationStatus, result.ProgressResult.StatusMessage)
	}
}

// TestDelete_DestructiveMode_ReCreateAfterDestroyWorks reproduces the
// conformance harness's OOB-Delete flow at the plugin layer: Create →
// destructive Delete → re-Create on the same DB. Re-Create should
// succeed (or surface a clear error) because the user's schema persists
// — atlas only dropped its own bookkeeping. With idempotent migrations
// (CREATE TABLE IF NOT EXISTS), atlas migrate apply runs cleanly.
func TestDelete_DestructiveMode_ReCreateAfterDestroyWorks(t *testing.T) {
	pg := startPostgres(t)
	migsUri := writeMigrationsDir(t, []migrationFile{
		{
			Version: "20260101000001",
			Name:    "init",
			SQL:     "CREATE TABLE IF NOT EXISTS users (id INT PRIMARY KEY);",
		},
	})
	mode := DeleteModeDropRevisions
	plugin := &Plugin{config: PluginRuntimeConfig{DeleteMode: &mode}}

	createReq := makeCreateRequest(t, pg, "mig", migsUri, "latest")
	if _, err := plugin.Create(context.Background(), createReq); err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	delReq := &resource.DeleteRequest{
		NativeID:     "mig",
		ResourceType: "ATLAS::Schema::Migration",
		TargetConfig: createReq.TargetConfig,
	}
	if _, err := plugin.Delete(context.Background(), delReq); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Re-Create on the same DB. With destructive Delete, atlas's revisions
	// table is gone but the `users` table persists. Idempotent migration
	// (IF NOT EXISTS) lets atlas migrate apply re-record without colliding.
	recreateResult, err := plugin.Create(context.Background(), createReq)
	if err != nil {
		t.Fatalf("re-Create: %v", err)
	}
	if recreateResult.ProgressResult.OperationStatus != resource.OperationStatusSuccess {
		t.Fatalf("re-Create status: got %q, msg=%q",
			recreateResult.ProgressResult.OperationStatus,
			recreateResult.ProgressResult.StatusMessage)
	}
	if got := queryAppliedVersion(t, pg); got != "20260101000001" {
		t.Errorf("post re-Create applied version: got %q, want %q", got, "20260101000001")
	}
}

// TestDelete_DestructiveMode_DropsRevisionsSchema verifies the opt-in
// destructive variant: with `retainRevisionTableOnDelete=false`, Delete
// drops atlas's `atlas_schema_revisions` schema. The user's migrated
// tables are NOT touched — only atlas's bookkeeping.
//
// After Delete, a Read should report NotFound (revisions table gone),
// which is what the conformance harness's OOB Delete phase tests for.
func TestDelete_DestructiveMode_DropsRevisionsSchema(t *testing.T) {
	pg := startPostgres(t)
	migsUri := writeMigrationsDir(t, []migrationFile{
		{Version: "20260101000001", Name: "init", SQL: "CREATE TABLE keep_me (id INT);"},
	})
	mode := DeleteModeDropRevisions
	plugin := &Plugin{config: PluginRuntimeConfig{DeleteMode: &mode}}

	createReq := makeCreateRequest(t, pg, "mig", migsUri, "latest")
	if _, err := plugin.Create(context.Background(), createReq); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	// Sanity-check: revisions table has the row.
	if got := queryAppliedVersion(t, pg); got != "20260101000001" {
		t.Fatalf("pre-delete: applied version got %q, want %q", got, "20260101000001")
	}

	delReq := &resource.DeleteRequest{
		NativeID:     "mig",
		ResourceType: "ATLAS::Schema::Migration",
		TargetConfig: createReq.TargetConfig,
	}
	result, err := plugin.Delete(context.Background(), delReq)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if result.ProgressResult.OperationStatus != resource.OperationStatusSuccess {
		t.Fatalf("status: got %q, msg=%q",
			result.ProgressResult.OperationStatus, result.ProgressResult.StatusMessage)
	}

	// Subsequent Read should now report NotFound — the revisions table
	// (and its enclosing schema) are gone.
	readReq := &resource.ReadRequest{
		NativeID:     "mig",
		ResourceType: "ATLAS::Schema::Migration",
		TargetConfig: createReq.TargetConfig,
	}
	readResult, err := plugin.Read(context.Background(), readReq)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if readResult.ErrorCode != resource.OperationErrorCodeNotFound {
		t.Errorf("post-destructive-delete Read ErrorCode: got %q, want NotFound",
			readResult.ErrorCode)
	}
}
