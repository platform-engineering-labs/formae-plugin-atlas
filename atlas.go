// © 2025 Platform Engineering Labs Inc.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/platform-engineering-labs/formae/pkg/model"
	"github.com/platform-engineering-labs/formae/pkg/plugin"
	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

// ErrNotImplemented is returned by stub methods that still need work.
// As CRUD operations land they replace the stubs and stop returning this.
var ErrNotImplemented = errors.New("not implemented")

// DeleteMode controls Delete's behavior and the related dirty-DB tolerance
// on Create/Update. See `atlas.PluginConfig.deleteMode` in
// `schema/Config.pkl` for the full doc.
type DeleteMode string

const (
	// DeleteModeRetain: Delete is a no-op (default). Matches the Atlas
	// Terraform provider. Re-Create against the same DB resumes idempotently
	// from the existing revisions cursor.
	DeleteModeRetain DeleteMode = "retain"

	// DeleteModeDropRevisions: Delete drops atlas's `atlas_schema_revisions`
	// schema. User data is preserved. Create/Update pass AllowDirty=true to
	// atlas so a subsequent Create against a DB with persistent user schema
	// is tolerated (combined with idempotent migrations).
	DeleteModeDropRevisions DeleteMode = "dropRevisions"
)

// PluginRuntimeConfig mirrors `atlas.PluginConfig` from `schema/Config.pkl`.
// Operators set these via the agent's `formae.conf.pkl`; the SDK marshals
// the PKL block to JSON and invokes Plugin.Configure at startup.
type PluginRuntimeConfig struct {
	// DeleteMode is a pointer so we can distinguish "unset → use built-in
	// default" from an explicit value. Defaults to DeleteModeRetain when nil
	// OR when Configure is never called (no per-plugin config in
	// formae.conf.pkl).
	DeleteMode *DeleteMode `json:"deleteMode,omitempty"`
}

// Plugin implements the Formae ResourcePlugin interface.
// The SDK automatically provides identity methods (Name, Version, Namespace)
// by reading formae-plugin.pkl at startup, and calls Configure with the
// operator-supplied PluginConfig (if any) before serving requests.
type Plugin struct {
	config PluginRuntimeConfig
}

// Compile-time checks: Plugin must satisfy both interfaces.
var (
	_ plugin.ResourcePlugin = &Plugin{}
	_ plugin.Configurable   = &Plugin{}
)

// Configure receives the operator's PluginConfig JSON (from formae.conf.pkl).
// Called once at plugin startup by the SDK; safe to no-op when no config is
// supplied — the built-in defaults already prefer prod-safe behavior.
func (p *Plugin) Configure(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var cfg PluginRuntimeConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("atlas plugin config: %w", err)
	}
	p.config = cfg
	return nil
}

// deleteMode is the resolved delete-mode policy. Precedence:
//  1. ATLAS_DELETE_MODE env var (override hatch for the conformance harness,
//     which has no per-plugin-config injection mechanism yet; remove once
//     that lands — see ~/dev/personal/engineering-notes/formae-mcp/...G-12).
//  2. PluginConfig.deleteMode (from formae.conf.pkl).
//  3. Built-in default: DeleteModeRetain.
func (p *Plugin) deleteMode() DeleteMode {
	if v := os.Getenv("ATLAS_DELETE_MODE"); v != "" {
		return DeleteMode(v)
	}
	if p.config.DeleteMode != nil {
		return *p.config.DeleteMode
	}
	return DeleteModeRetain
}

// retainRevisionsOnDelete is a convenience for code paths that need a
// boolean view of the policy: true when in retain mode, false otherwise.
func (p *Plugin) retainRevisionsOnDelete() bool {
	return p.deleteMode() == DeleteModeRetain
}

// =============================================================================
// Configuration Methods
// =============================================================================

// RateLimit caps concurrent operations against the target DB. Migrations
// hold a schema-level advisory lock and can be I/O-heavy on large tables;
// a conservative ceiling avoids saturating connection pools on shared
// Aurora clusters. Tune upward only after observing real-world load.
func (p *Plugin) RateLimit() model.RateLimitConfig {
	return model.RateLimitConfig{
		Scope:                            model.RateLimitScopeNamespace,
		MaxRequestsPerSecondForNamespace: 5,
	}
}

// DiscoveryFilters returns nil — discovery is disabled for Atlas Targets
// in v1. Migrations are declared, not discovered: there's no meaningful
// "enumerate Migrations on this DB" semantics, and Targets default to
// discoverable=false. See README §"Discovery" for the rationale.
func (p *Plugin) DiscoveryFilters() []model.MatchFilter {
	return nil
}

// LabelConfig extracts a human-readable identifier from a resource's JSON.
// ATLAS::Schema::Migration is a logical resource with no server-side
// native ID, so the @ResourceHint declares identifier = "Label" and the
// label is the canonical user-facing handle. Query mirrors that choice.
func (p *Plugin) LabelConfig() model.LabelConfig {
	return model.LabelConfig{
		DefaultQuery:      "$.Label",
		ResourceOverrides: map[string]string{},
	}
}

// =============================================================================
// CRUD Operations
// =============================================================================

// Create runs atlas migrate apply to bring the target DB up to the
// requested version. Naturally idempotent — already-applied targets
// are a no-op. See migration.go:createMigration for the implementation.
func (p *Plugin) Create(ctx context.Context, req *resource.CreateRequest) (*resource.CreateResult, error) {
	return &resource.CreateResult{ProgressResult: p.createMigration(ctx, req)}, nil
}

// Read queries the target DB's atlas_schema_revisions table to report
// the currently applied migration version. Returns NotFound when the
// revisions table is absent — the agent treats that as "resource gone"
// and removes the migration from inventory. See migration.go:readMigration.
func (p *Plugin) Read(ctx context.Context, req *resource.ReadRequest) (*resource.ReadResult, error) {
	return p.readMigration(ctx, req), nil
}

// Update applies desired migrations using the same atlas migrate apply
// logic as Create (naturally idempotent). When the requested version
// is lower than what's currently applied, gates on the resource's
// allowDowngrade field — defaults to false (reject) per design D2.
// See migration.go:updateMigration.
func (p *Plugin) Update(ctx context.Context, req *resource.UpdateRequest) (*resource.UpdateResult, error) {
	return &resource.UpdateResult{ProgressResult: p.updateMigration(ctx, req)}, nil
}

// Delete tears down the resource's formae state. Behavior is controlled
// by PluginConfig.deleteMode (default: `retain`):
//
//   - retain: Delete is a no-op on the DB. Matches the Atlas Terraform
//     provider — schema + revisions table are preserved so re-Create
//     resumes idempotently via the existing cursor.
//   - dropRevisions: Delete drops the `atlas_schema_revisions` schema.
//     User schema/data is NOT touched, but a subsequent Create may need
//     idempotent migrations or `atlas migrate baseline` to realign.
//
// See migration.go:deleteMigration for the destructive-mode implementation.
func (p *Plugin) Delete(ctx context.Context, req *resource.DeleteRequest) (*resource.DeleteResult, error) {
	switch p.deleteMode() {
	case DeleteModeRetain:
		return &resource.DeleteResult{
			ProgressResult: &resource.ProgressResult{
				Operation:       resource.OperationDelete,
				OperationStatus: resource.OperationStatusSuccess,
				NativeID:        req.NativeID,
				StatusMessage:   "Delete is a no-op; database schema preserved (retain mode)",
			},
		}, nil
	case DeleteModeDropRevisions:
		return &resource.DeleteResult{ProgressResult: p.deleteMigration(ctx, req)}, nil
	default:
		return &resource.DeleteResult{
			ProgressResult: progressFailure(resource.OperationDelete,
				resource.OperationErrorCodeInvalidRequest,
				fmt.Sprintf("unknown deleteMode %q (supported: retain, dropRevisions)", p.deleteMode())),
		}, nil
	}
}

// Status reports progress of in-flight async operations. atlasexec's
// MigrateApply blocks the calling goroutine until the migration
// completes, so Create/Update never return OperationStatusInProgress
// and Status should never be invoked. We implement it defensively
// (immediate Success on the supplied NativeID) for protocol
// conformance.
func (p *Plugin) Status(ctx context.Context, req *resource.StatusRequest) (*resource.StatusResult, error) {
	return &resource.StatusResult{
		ProgressResult: &resource.ProgressResult{
			Operation:       resource.OperationCheckStatus,
			OperationStatus: resource.OperationStatusSuccess,
			NativeID:        req.NativeID,
		},
	}, nil
}

// List enumerates resources of a given type for discovery. Atlas
// migrations are declared, not discovered (Targets default to
// discoverable=false in v1 — see DiscoveryFilters). The plugin
// returns an empty list so the discovery harness gets a well-formed
// no-op response.
func (p *Plugin) List(ctx context.Context, req *resource.ListRequest) (*resource.ListResult, error) {
	return &resource.ListResult{
		NativeIDs:     []string{},
		NextPageToken: nil,
	}, nil
}
