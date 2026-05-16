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

// TestList_EmptyDB verifies that discovery on a fresh Postgres (no
// atlas_schema_revisions schema yet) returns no native IDs. The plugin
// only surfaces Migrations that atlas has actually tracked; we don't
// invent placeholders for unmigrated DBs.
func TestList_EmptyDB(t *testing.T) {
	pg := startPostgres(t)
	plugin := &Plugin{}
	result, err := plugin.List(context.Background(), &resource.ListRequest{
		ResourceType: "ATLAS::Schema::Migration",
		TargetConfig: configRawFor(pg),
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(result.NativeIDs) != 0 {
		t.Errorf("NativeIDs: got %v, want []", result.NativeIDs)
	}
}

// TestList_AfterCreate verifies that once a Migration has been applied
// to a DB, List reports exactly one native ID. The native ID encodes
// the database name so cross-Target discoveries don't collide.
func TestList_AfterCreate(t *testing.T) {
	pg := startPostgres(t)
	migsUri := writeMigrationsDir(t, []migrationFile{
		{Version: "20260101000001", Name: "init", SQL: "CREATE TABLE t (id INT);"},
	})
	plugin := &Plugin{}
	createReq := makeCreateRequest(t, pg, "mig", migsUri, "latest")
	if _, err := plugin.Create(context.Background(), createReq); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	result, err := plugin.List(context.Background(), &resource.ListRequest{
		ResourceType: "ATLAS::Schema::Migration",
		TargetConfig: createReq.TargetConfig,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(result.NativeIDs) != 1 {
		t.Fatalf("NativeIDs: got %d entries (%v), want 1", len(result.NativeIDs), result.NativeIDs)
	}
	// Native ID is derived from the Target's connection coordinates so
	// it matches what Create returns (the harness's discovery flow
	// captures Create's NativeID and waits for List to surface the same).
	wantNative := nativeIDFromConfig(&Config{Dialect: "postgres", Host: pg.Host, Port: pg.Port, Database: pg.Database})
	if got := result.NativeIDs[0]; got != wantNative {
		t.Errorf("NativeIDs[0]: got %q, want %q", got, wantNative)
	}

	// Read on the discovered native ID returns the applied version.
	readResult, err := plugin.Read(context.Background(), &resource.ReadRequest{
		NativeID:     result.NativeIDs[0],
		ResourceType: "ATLAS::Schema::Migration",
		TargetConfig: createReq.TargetConfig,
	})
	if err != nil {
		t.Fatalf("Read on discovered ID: %v", err)
	}
	if readResult.ErrorCode != "" {
		t.Errorf("Read ErrorCode: got %q, want empty", readResult.ErrorCode)
	}
}

// TestList_InvalidTargetConfig surfaces InvalidRequest cleanly when the
// target config can't be parsed, instead of returning a Go error.
func TestList_InvalidTargetConfig(t *testing.T) {
	plugin := &Plugin{}
	_, err := plugin.List(context.Background(), &resource.ListRequest{
		ResourceType: "ATLAS::Schema::Migration",
		TargetConfig: []byte(`{not json`),
	})
	if err == nil {
		t.Fatal("expected Go error for invalid target config, got nil")
	}
}
