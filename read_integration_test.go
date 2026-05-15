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

// TestRead_ReturnsAppliedVersion verifies that Read against a migrated DB
// returns the latest applied version in the result Properties.
func TestRead_ReturnsAppliedVersion(t *testing.T) {
	pg := startPostgres(t)
	migrationsUri := writeMigrationsDir(t, []migrationFile{
		{Version: "20260101000001", Name: "init", SQL: "CREATE TABLE t (id INT);"},
	})

	plugin := &Plugin{}
	// Seed: apply migrations via Create.
	createReq := makeCreateRequest(t, pg, "test-mig", migrationsUri, "latest")
	if _, err := plugin.Create(context.Background(), createReq); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	// Now exercise Read.
	readReq := &resource.ReadRequest{
		NativeID:     "test-mig",
		ResourceType: "ATLAS::Schema::Migration",
		TargetConfig: createReq.TargetConfig,
	}
	result, err := plugin.Read(context.Background(), readReq)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if result.ErrorCode != "" {
		t.Fatalf("Read ErrorCode: got %q, want empty (success)", result.ErrorCode)
	}
	if result.ResourceType != "ATLAS::Schema::Migration" {
		t.Errorf("ResourceType: got %q", result.ResourceType)
	}

	var props MigrationProperties
	if err := json.Unmarshal([]byte(result.Properties), &props); err != nil {
		t.Fatalf("Properties not valid JSON: %v (raw=%s)", err, result.Properties)
	}
	if props.AppliedVersion != "20260101000001" {
		t.Errorf("AppliedVersion: got %q, want %q", props.AppliedVersion, "20260101000001")
	}
}

// TestRead_NotFoundOnMissingRevisionsTable mirrors the Atlas TF provider's
// drift handling: if the revisions table is gone (someone wiped it
// out-of-band), Read returns NotFound. The agent uses this to treat the
// resource as deleted and trigger re-Create on the next reconcile.
func TestRead_NotFoundOnMissingRevisionsTable(t *testing.T) {
	pg := startPostgres(t)

	// Fresh DB; no Create has been invoked, so atlas_schema_revisions
	// doesn't exist. Read should treat this as NotFound.
	plugin := &Plugin{}
	cfgRaw := configRawFor(pg)
	readReq := &resource.ReadRequest{
		NativeID:     "never-existed",
		ResourceType: "ATLAS::Schema::Migration",
		TargetConfig: cfgRaw,
	}
	result, err := plugin.Read(context.Background(), readReq)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if result.ErrorCode != resource.OperationErrorCodeNotFound {
		t.Errorf("ErrorCode: got %q, want NotFound. Properties=%q",
			result.ErrorCode, result.Properties)
	}
}

// TestRead_InvalidTargetConfig surfaces InvalidRequest when the target
// config can't be parsed (malformed JSON, missing credentials, etc.).
func TestRead_InvalidTargetConfig(t *testing.T) {
	plugin := &Plugin{}
	readReq := &resource.ReadRequest{
		NativeID:     "x",
		ResourceType: "ATLAS::Schema::Migration",
		TargetConfig: []byte(`{not json`),
	}
	result, err := plugin.Read(context.Background(), readReq)
	if err != nil {
		t.Fatalf("Read should not return Go error: %v", err)
	}
	if result.ErrorCode != resource.OperationErrorCodeInvalidRequest {
		t.Errorf("ErrorCode: got %q, want InvalidRequest", result.ErrorCode)
	}
	if !strings.Contains(result.Properties, "") {
		t.Errorf("unexpected Properties: %q", result.Properties)
	}
}

// configRawFor builds the TargetConfig JSON used by Read tests that
// don't go through Create first.
func configRawFor(pg *pgContainer) json.RawMessage {
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
	raw, _ := json.Marshal(cfg)
	return raw
}
