// © 2026 Platform Engineering Labs Inc.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ariga.io/atlas-go-sdk/atlasexec"
	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"

	_ "github.com/lib/pq"
)

// =============================================================================
// Migration resource properties
// =============================================================================

// MigrationProperties mirrors the ATLAS::Schema::Migration PKL resource
// fields. Input fields echo back unchanged; output fields (AppliedVersion,
// Baseline) are populated by Create/Read by querying the target DB.
//
// `AllowDowngrade` is the only required-by-validator field that has a
// meaningful zero value (false). It drops `omitempty` so the marshalled
// output always carries it. `MigrationsUri` and `TargetVersion` are
// nullable in PKL (Read can omit them for both discovered and managed
// resources without forcing drift) and the plugin enforces non-empty
// at Create/Update time. See README §"Discovery & adoption" for the
// adoption flow.
type MigrationProperties struct {
	MigrationsUri   string  `json:"migrationsUri,omitempty"`
	TargetVersion   string  `json:"targetVersion,omitempty"`
	RevisionsSchema *string `json:"revisionsSchema,omitempty"`
	AllowDowngrade  bool    `json:"allowDowngrade"`
	Tool            string  `json:"tool,omitempty"`

	// Computed outputs.
	AppliedVersion string `json:"appliedVersion,omitempty"`
	Baseline       string `json:"baseline,omitempty"`
}

func parseMigrationProperties(raw json.RawMessage) (*MigrationProperties, error) {
	var props MigrationProperties
	if err := json.Unmarshal(raw, &props); err != nil {
		return nil, fmt.Errorf("invalid resource properties: %w", err)
	}
	return &props, nil
}

// =============================================================================
// Create — apply migrations to targetVersion
// =============================================================================

// createMigration runs atlas migrate apply to bring the target DB up to
// (or, with allowDowngrade, down to) the requested version. Mirrors the
// Atlas TF provider's Create semantics: the same migrate() logic Update
// uses, naturally idempotent — already-applied targets are a no-op.
func (p *Plugin) createMigration(ctx context.Context, req *resource.CreateRequest) *resource.ProgressResult {
	cfg, creds, props, errResult := p.parseCreateInputs(req)
	if errResult != nil {
		return errResult
	}

	connURL, err := BuildConnectionURL(cfg, creds)
	if err != nil {
		return progressFailure(resource.OperationCreate, resource.OperationErrorCodeInvalidRequest,
			fmt.Sprintf("BuildConnectionURL: %v", err))
	}

	// Destructive-Delete mode tolerates re-Create against a DB whose
	// user schema persists. See runAtlasMigrate's allowDirty docs.
	allowDirty := !p.retainRevisionsOnDelete()

	if err := runAtlasMigrate(ctx, connURL, props, allowDirty); err != nil {
		return progressFailure(resource.OperationCreate, classifyMigrateError(err), err.Error())
	}

	applied, err := readAppliedVersion(ctx, connURL, props.RevisionsSchema)
	if err != nil {
		return progressFailure(resource.OperationCreate, resource.OperationErrorCodeInternalFailure,
			fmt.Sprintf("readAppliedVersion: %v", err))
	}

	out := *props
	out.AppliedVersion = applied
	outRaw, _ := json.Marshal(out)

	return &resource.ProgressResult{
		Operation:          resource.OperationCreate,
		OperationStatus:    resource.OperationStatusSuccess,
		NativeID:           nativeIDFromConfig(cfg),
		ResourceProperties: outRaw,
	}
}

func (p *Plugin) parseCreateInputs(req *resource.CreateRequest) (
	*Config, Credentials, *MigrationProperties, *resource.ProgressResult,
) {
	cfg, err := ParseConfig(req.TargetConfig)
	if err != nil {
		return nil, nil, nil, progressFailure(resource.OperationCreate,
			resource.OperationErrorCodeInvalidRequest, err.Error())
	}

	var creds Credentials
	if len(cfg.Credentials) > 0 {
		creds, err = ParseCredentials(cfg.Credentials)
		if err != nil {
			return nil, nil, nil, progressFailure(resource.OperationCreate,
				resource.OperationErrorCodeInvalidRequest, err.Error())
		}
	}

	props, err := parseMigrationProperties(req.Properties)
	if err != nil {
		return nil, nil, nil, progressFailure(resource.OperationCreate,
			resource.OperationErrorCodeInvalidRequest, err.Error())
	}
	if props.MigrationsUri == "" {
		return nil, nil, nil, progressFailure(resource.OperationCreate,
			resource.OperationErrorCodeInvalidRequest, "migrationsUri is required")
	}
	if props.TargetVersion == "" {
		return nil, nil, nil, progressFailure(resource.OperationCreate,
			resource.OperationErrorCodeInvalidRequest, "targetVersion is required")
	}

	return cfg, creds, props, nil
}

// runAtlasMigrate shells out to the atlas binary via the atlasexec SDK.
// For targetVersion=latest, applies all pending migrations. For pinned
// versions, computes the amount needed to reach the target via
// MigrateStatus.Amount(version).
//
// allowDirty=true is passed in destructive-Delete-followed-by-Create
// scenarios where atlas's clean-DB safety check would otherwise reject
// the call (user schema persists from the prior Create; we dropped
// only atlas's bookkeeping). Combined with idempotent migration SQL
// (CREATE TABLE IF NOT EXISTS, etc.) this gives a working
// re-Create-after-destructive-Delete path. The flag has no effect on
// truly fresh DBs.
func runAtlasMigrate(ctx context.Context, connURL string, props *MigrationProperties, allowDirty bool) error {
	client, err := atlasexec.NewClient("", "atlas")
	if err != nil {
		return fmt.Errorf("atlasexec.NewClient: %w", err)
	}

	apply := &atlasexec.MigrateApplyParams{
		URL:        connURL,
		DirURL:     props.MigrationsUri,
		AllowDirty: allowDirty,
	}
	if props.RevisionsSchema != nil {
		apply.RevisionsSchema = *props.RevisionsSchema
	}

	if props.TargetVersion != "" && props.TargetVersion != "latest" {
		status, err := client.MigrateStatus(ctx, &atlasexec.MigrateStatusParams{
			URL:             connURL,
			DirURL:          props.MigrationsUri,
			RevisionsSchema: apply.RevisionsSchema,
		})
		if err != nil {
			return fmt.Errorf("atlas migrate status: %w", err)
		}
		amount, ok := status.Amount(props.TargetVersion)
		if !ok {
			// Either the version is already applied (no-op) or it's not
			// in the available set. Apply 0 is the right behavior for the
			// already-applied case; the not-in-set case will error from
			// atlas itself on the next status check.
			if isAlreadyAtTarget(status, props.TargetVersion) {
				return nil
			}
			return fmt.Errorf("targetVersion %q not in available migrations", props.TargetVersion)
		}
		apply.Amount = amount
	}

	result, err := client.MigrateApply(ctx, apply)
	if err != nil {
		if isNothingToApply(err) {
			return nil
		}
		return fmt.Errorf("atlas migrate apply: %w", err)
	}
	if result == nil {
		return fmt.Errorf("atlas migrate apply: nil result")
	}
	if result.Error != "" {
		return fmt.Errorf("atlas migrate apply error: %s", result.Error)
	}
	return nil
}

// isAlreadyAtTarget reports whether the DB is already at or past the
// requested version (so MigrateStatus.Amount returned ok=false because
// there's nothing pending to apply, not because the version is unknown).
func isAlreadyAtTarget(status *atlasexec.MigrateStatus, version string) bool {
	if status == nil {
		return false
	}
	for _, applied := range status.Applied {
		if applied.Version == version {
			return true
		}
	}
	return false
}

// isNothingToApply detects atlas's "no migration files to execute" path,
// which surfaces as an error from MigrateApply on some versions of the
// CLI even though the operational outcome is "no-op success."
func isNothingToApply(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "no migration files to execute") ||
		strings.Contains(s, "no pending migrations")
}

// classifyMigrateError maps atlasexec errors to a formae OperationErrorCode.
// Conservative: anything we can't classify becomes InternalFailure.
func classifyMigrateError(err error) resource.OperationErrorCode {
	if err == nil {
		return ""
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"),
		strings.Contains(s, "no such host"),
		strings.Contains(s, "i/o timeout"):
		return resource.OperationErrorCodeNetworkFailure
	case strings.Contains(s, "authentication failed"),
		strings.Contains(s, "password authentication"):
		return resource.OperationErrorCodeInvalidCredentials
	case strings.Contains(s, "permission denied"):
		return resource.OperationErrorCodeAccessDenied
	default:
		return resource.OperationErrorCodeInternalFailure
	}
}

// =============================================================================
// Read — query atlas_schema_revisions
// =============================================================================

// migrationState captures everything Read needs to report. tableExists
// distinguishes "fresh DB / never migrated" (Read maps to NotFound) from
// "table exists but empty" (no rows, treat as no version applied yet).
type migrationState struct {
	AppliedVersion string
	TableExists    bool
}

// readMigrationState locates the atlas_schema_revisions table (in any
// schema — atlas defaults to a dedicated `atlas_schema_revisions`
// schema but the operator can override via revisionsSchema), then
// returns the latest applied version.
//
// hintSchema is consulted first when non-nil — useful when the caller
// knows the configured revisionsSchema (Create/Update have it via
// Properties). When the hint produces nothing, the function falls back
// to discovering the schema from information_schema.
func readMigrationState(ctx context.Context, connURL string, hintSchema *string) (migrationState, error) {
	db, err := sql.Open("postgres", connURL)
	if err != nil {
		return migrationState{}, fmt.Errorf("sql.Open: %w", err)
	}
	defer db.Close()

	schema, err := locateRevisionsSchema(ctx, db, hintSchema)
	if err != nil {
		return migrationState{}, err
	}
	if schema == "" {
		return migrationState{TableExists: false}, nil
	}

	q := fmt.Sprintf(`SELECT version FROM %q.atlas_schema_revisions
		ORDER BY executed_at DESC LIMIT 1`, schema)
	var v string
	err = db.QueryRowContext(ctx, q).Scan(&v)
	if err == nil {
		return migrationState{AppliedVersion: v, TableExists: true}, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return migrationState{TableExists: true}, nil
	}
	return migrationState{}, fmt.Errorf("query revisions: %w", err)
}

// locateRevisionsSchema returns the schema name that contains atlas's
// revisions table, or "" if the table doesn't exist in any schema.
// Atlas creates the table on first migrate apply; absence means the
// resource hasn't been Created yet (or was wiped out-of-band).
func locateRevisionsSchema(ctx context.Context, db *sql.DB, hint *string) (string, error) {
	if hint != nil && *hint != "" {
		ok, err := tableExists(ctx, db, *hint)
		if err != nil {
			return "", err
		}
		if ok {
			return *hint, nil
		}
	}
	row := db.QueryRowContext(ctx, `SELECT table_schema FROM information_schema.tables
		WHERE table_name = 'atlas_schema_revisions' LIMIT 1`)
	var schema string
	err := row.Scan(&schema)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("scan revisions schema: %w", err)
	}
	return schema, nil
}

func tableExists(ctx context.Context, db *sql.DB, schema string) (bool, error) {
	row := db.QueryRowContext(ctx, `SELECT 1 FROM information_schema.tables
		WHERE table_schema = $1 AND table_name = 'atlas_schema_revisions'`, schema)
	var one int
	err := row.Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// readAppliedVersion is a thin convenience wrapper for createMigration's
// post-apply state pull. Use readMigrationState directly when callers
// also need the TableExists bit.
func readAppliedVersion(ctx context.Context, connURL string, revisionsSchema *string) (string, error) {
	st, err := readMigrationState(ctx, connURL, revisionsSchema)
	if err != nil {
		return "", err
	}
	return st.AppliedVersion, nil
}

// =============================================================================
// Update — forward / downgrade-gated
// =============================================================================

// updateMigration runs the same migrate flow as Create, with a
// downgrade gate: if the desired targetVersion is lower than what's
// currently applied AND allowDowngrade is false, the request is
// rejected before any DB work happens. Mirrors Atlas TF's `migrate()`
// idempotent forward path plus our v1 design's explicit downgrade
// safety property (D2).
func (p *Plugin) updateMigration(ctx context.Context, req *resource.UpdateRequest) *resource.ProgressResult {
	cfg, err := ParseConfig(req.TargetConfig)
	if err != nil {
		return progressFailure(resource.OperationUpdate,
			resource.OperationErrorCodeInvalidRequest, err.Error())
	}
	var creds Credentials
	if len(cfg.Credentials) > 0 {
		creds, err = ParseCredentials(cfg.Credentials)
		if err != nil {
			return progressFailure(resource.OperationUpdate,
				resource.OperationErrorCodeInvalidRequest, err.Error())
		}
	}
	desired, err := parseMigrationProperties(req.DesiredProperties)
	if err != nil {
		return progressFailure(resource.OperationUpdate,
			resource.OperationErrorCodeInvalidRequest, err.Error())
	}
	if desired.MigrationsUri == "" || desired.TargetVersion == "" {
		return progressFailure(resource.OperationUpdate,
			resource.OperationErrorCodeInvalidRequest,
			"migrationsUri and targetVersion are required")
	}

	connURL, err := BuildConnectionURL(cfg, creds)
	if err != nil {
		return progressFailure(resource.OperationUpdate,
			resource.OperationErrorCodeInvalidRequest, err.Error())
	}

	// Pre-check: if requested version is lower than current applied,
	// gate on allowDowngrade. Compares lexicographically — fine for
	// atlas's YYYYMMDDHHMMSS timestamp scheme.
	state, err := readMigrationState(ctx, connURL, desired.RevisionsSchema)
	if err != nil {
		return progressFailure(resource.OperationUpdate,
			resource.OperationErrorCodeInternalFailure,
			fmt.Sprintf("readMigrationState: %v", err))
	}
	allowDirty := !p.retainRevisionsOnDelete()

	if state.TableExists && isDowngrade(state.AppliedVersion, desired.TargetVersion) {
		if !desired.AllowDowngrade {
			return progressFailure(resource.OperationUpdate,
				resource.OperationErrorCodeInvalidRequest,
				fmt.Sprintf("downgrade blocked: current=%s, requested=%s; set allowDowngrade=true to opt in",
					state.AppliedVersion, desired.TargetVersion))
		}
		if err := runAtlasMigrateDown(ctx, connURL, desired); err != nil {
			return progressFailure(resource.OperationUpdate,
				classifyMigrateError(err), err.Error())
		}
	} else {
		if err := runAtlasMigrate(ctx, connURL, desired, allowDirty); err != nil {
			return progressFailure(resource.OperationUpdate,
				classifyMigrateError(err), err.Error())
		}
	}

	applied, err := readAppliedVersion(ctx, connURL, desired.RevisionsSchema)
	if err != nil {
		return progressFailure(resource.OperationUpdate,
			resource.OperationErrorCodeInternalFailure,
			fmt.Sprintf("post-update readAppliedVersion: %v", err))
	}
	out := *desired
	out.AppliedVersion = applied
	outRaw, _ := json.Marshal(out)
	return &resource.ProgressResult{
		Operation:          resource.OperationUpdate,
		OperationStatus:    resource.OperationStatusSuccess,
		NativeID:           nativeIDFromConfig(cfg),
		ResourceProperties: outRaw,
	}
}

// isDowngrade reports whether `desired` is strictly less than `current`.
// "latest" is never a downgrade. Empty current (fresh DB) is never a
// downgrade either — Update on a never-Created resource is treated as
// forward apply.
func isDowngrade(current, desired string) bool {
	if current == "" || desired == "" || desired == "latest" {
		return false
	}
	return desired < current
}

// runAtlasMigrateDown rolls the DB back to the requested ToVersion via
// `atlas migrate down`. Community Atlas has limited support; if the
// underlying call fails because the necessary down statements aren't
// present, the error surfaces to the operator unchanged.
func runAtlasMigrateDown(ctx context.Context, connURL string, props *MigrationProperties) error {
	client, err := atlasexec.NewClient("", "atlas")
	if err != nil {
		return fmt.Errorf("atlasexec.NewClient: %w", err)
	}
	params := &atlasexec.MigrateDownParams{
		URL:       connURL,
		DirURL:    props.MigrationsUri,
		ToVersion: props.TargetVersion,
	}
	if props.RevisionsSchema != nil {
		params.RevisionsSchema = *props.RevisionsSchema
	}
	_, err = client.MigrateDown(ctx, params)
	return err
}

// =============================================================================
// List — surface atlas-managed migrations for discovery
// =============================================================================

// listMigrations probes the target DB for an `atlas_schema_revisions`
// schema. If present, the Target has an atlas-tracked migration and we
// return a single synthetic NativeID derived from the DB connection
// coordinates. If absent, the Target either has no migrations or is
// managed by a different tool (flyway/sqitch) — neither concerns this
// plugin.
func (p *Plugin) listMigrations(ctx context.Context, req *resource.ListRequest) (*resource.ListResult, error) {
	cfg, err := ParseConfig(req.TargetConfig)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	var creds Credentials
	if len(cfg.Credentials) > 0 {
		creds, err = ParseCredentials(cfg.Credentials)
		if err != nil {
			return nil, fmt.Errorf("list credentials: %w", err)
		}
	}
	connURL, err := BuildConnectionURL(cfg, creds)
	if err != nil {
		return nil, fmt.Errorf("list connection url: %w", err)
	}

	state, err := readMigrationState(ctx, connURL, nil)
	if err != nil {
		return nil, fmt.Errorf("list revisions probe: %w", err)
	}
	if !state.TableExists {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}

	return &resource.ListResult{NativeIDs: []string{nativeIDFromConfig(cfg)}}, nil
}

// nativeIDFromConfig returns a stable, observable-from-TargetConfig
// identifier for a Migration. The "identity" of a migration resource is
// the DB it operates on — host:port/database fully specifies that.
// Both Create and List derive from the same data so discovery's
// NativeID round-trip works without any plugin-side label persistence.
//
// Form: postgres://<host>:<port>/<database> (DSN-shaped, sans credentials).
// Reads as the actual connection URL the operator pointed at, so
// `formae inventory | grep <host>` works.
func nativeIDFromConfig(cfg *Config) string {
	host := cfg.Host
	if host == "" {
		host = "unknown"
	}
	port := cfg.Port
	if port == 0 {
		port = 5432
	}
	db := cfg.Database
	if db == "" {
		db = "unknown"
	}
	dialect := cfg.Dialect
	if dialect == "" {
		dialect = "postgres"
	}
	return fmt.Sprintf("%s://%s:%d/%s", dialect, host, port, db)
}

// =============================================================================
// Delete — destructive variant (drops atlas revisions schema)
// =============================================================================

// deleteMigration runs the destructive Delete path used when
// PluginConfig.retainRevisionTableOnDelete=false. Drops the
// `atlas_schema_revisions` schema (CASCADE removes its tables). The
// user's migrated schema/data is untouched — atlas migrate apply
// created those in other schemas/tables, and we don't track which.
//
// On a subsequent Create against the same DB:
//   - If the DB is otherwise empty: atlas migrate apply runs cleanly.
//   - If the user's tables persist: atlas migrate apply errors on the
//     first CREATE TABLE that collides. Operator runs
//     `atlas migrate baseline <version>` to align the cursor with
//     the existing state, then re-applies.
func (p *Plugin) deleteMigration(ctx context.Context, req *resource.DeleteRequest) *resource.ProgressResult {
	cfg, err := ParseConfig(req.TargetConfig)
	if err != nil {
		return progressFailure(resource.OperationDelete,
			resource.OperationErrorCodeInvalidRequest, err.Error())
	}
	var creds Credentials
	if len(cfg.Credentials) > 0 {
		creds, err = ParseCredentials(cfg.Credentials)
		if err != nil {
			return progressFailure(resource.OperationDelete,
				resource.OperationErrorCodeInvalidRequest, err.Error())
		}
	}
	connURL, err := BuildConnectionURL(cfg, creds)
	if err != nil {
		return progressFailure(resource.OperationDelete,
			resource.OperationErrorCodeInvalidRequest, err.Error())
	}

	// req.Properties is unavailable for Delete (the contract only exposes
	// TargetConfig + NativeID), so we can't honor a custom revisionsSchema
	// set on the Migration resource. Discovery falls back to information_schema.
	if err := dropRevisionsSchema(ctx, connURL); err != nil {
		return progressFailure(resource.OperationDelete,
			classifyMigrateError(err),
			fmt.Sprintf("dropRevisionsSchema: %v", err))
	}
	return &resource.ProgressResult{
		Operation:       resource.OperationDelete,
		OperationStatus: resource.OperationStatusSuccess,
		NativeID:        req.NativeID,
		StatusMessage:   "Revisions table dropped (destructive mode); user schema preserved",
	}
}

// dropRevisionsSchema removes atlas's bookkeeping schema. Tolerates the
// schema already being absent (treats as idempotent success).
func dropRevisionsSchema(ctx context.Context, connURL string) error {
	db, err := sql.Open("postgres", connURL)
	if err != nil {
		return fmt.Errorf("sql.Open: %w", err)
	}
	defer db.Close()

	schema, err := locateRevisionsSchema(ctx, db, nil)
	if err != nil {
		return err
	}
	if schema == "" {
		// Nothing to drop; treat as already-clean.
		return nil
	}
	q := fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema)
	if _, err := db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("drop revisions schema %q: %w", schema, err)
	}
	return nil
}

// readMigration is the per-resource handler invoked by Plugin.Read.
// Returns NotFound when atlas's revisions table is missing — the
// canonical signal that the resource has been deleted out-of-band
// (mirrors Atlas TF provider's `resp.State.RemoveResource(ctx)`).
func (p *Plugin) readMigration(ctx context.Context, req *resource.ReadRequest) *resource.ReadResult {
	cfg, err := ParseConfig(req.TargetConfig)
	if err != nil {
		return &resource.ReadResult{
			ResourceType: req.ResourceType,
			ErrorCode:    resource.OperationErrorCodeInvalidRequest,
		}
	}

	var creds Credentials
	if len(cfg.Credentials) > 0 {
		creds, err = ParseCredentials(cfg.Credentials)
		if err != nil {
			return &resource.ReadResult{
				ResourceType: req.ResourceType,
				ErrorCode:    resource.OperationErrorCodeInvalidRequest,
			}
		}
	}

	connURL, err := BuildConnectionURL(cfg, creds)
	if err != nil {
		return &resource.ReadResult{
			ResourceType: req.ResourceType,
			ErrorCode:    resource.OperationErrorCodeInvalidRequest,
		}
	}

	// Read has no access to req.Properties (the contract only exposes
	// TargetConfig + NativeID), so we can't honor a custom
	// revisionsSchema set in the Migration resource. Discovery falls
	// back to information_schema lookup in locateRevisionsSchema.
	state, err := readMigrationState(ctx, connURL, nil)
	if err != nil {
		return &resource.ReadResult{
			ResourceType: req.ResourceType,
			ErrorCode:    resource.OperationErrorCodeInternalFailure,
		}
	}
	if !state.TableExists {
		return &resource.ReadResult{
			ResourceType: req.ResourceType,
			ErrorCode:    resource.OperationErrorCodeNotFound,
		}
	}

	// Return only observable state so Sync's diff doesn't trigger a
	// phantom change for managed resources (e.g. user declared
	// targetVersion="latest", Read returning the literal applied
	// version would look like drift). For discovered resources, the
	// caller treats the absent inputs as operator-fillable on adopt.
	out := MigrationProperties{
		AllowDowngrade: false,
		Tool:           "atlas",
		AppliedVersion: state.AppliedVersion,
	}
	raw, _ := json.Marshal(out)
	return &resource.ReadResult{
		ResourceType: req.ResourceType,
		Properties:   string(raw),
	}
}

// =============================================================================
// Helpers
// =============================================================================

// progressFailure builds a ProgressResult for an early failure path.
// Use when an operation can't even start (parse error, validation
// failure, etc.); the operation's NativeID is unset.
func progressFailure(op resource.Operation, code resource.OperationErrorCode, message string) *resource.ProgressResult {
	return &resource.ProgressResult{
		Operation:       op,
		OperationStatus: resource.OperationStatusFailure,
		ErrorCode:       code,
		StatusMessage:   message,
	}
}
