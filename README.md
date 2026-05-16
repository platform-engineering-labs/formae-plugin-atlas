# formae-plugin-atlas

Database schema migrations as a first-class formae resource. Wraps
[Atlas](https://atlasgo.io) to manage migrations declaratively with
the same DAG ordering, drift detection, and reconciliation as cloud
resources.

## Why

Today's typical hub deployment splits app rollout and schema migration
across two pipelines: pre-deploy hook runs `atlas migrate apply`, then
the IaC pipeline updates the workload. The split has two costs:

1. **Ordering is operator-managed.** Forget the hook and your service
   ships before the schema; remember it but get a transient failure
   and the workload runs against a partially-migrated DB.
2. **State drifts silently.** The IaC tool has no idea what version
   the schema is on. Manual atlas runs aren't visible to your plan.

This plugin collapses both. Declaring an `ATLAS::Schema::Migration`
resource puts the migration into the formae DAG: upstream cloud
resources (RDS, secrets) feed in through the Target, downstream
workloads reference `migration.res.appliedVersion`, and a single
`formae apply` reconciles the entire stack in dependency order.

## Supported resources

| Type | Description |
|------|-------------|
| `ATLAS::Schema::Migration` | Ensures the target DB is migrated up to (or, with `allowDowngrade=true`, back down to) `targetVersion`. Read returns the currently applied version. Delete is a no-op (schema is preserved). |

V1 is postgres-only. The `dialect` field is reserved for future MySQL
/ SQL Server expansion without schema breakage.

## Prerequisites

The plugin shells out to the `atlas` CLI binary at runtime. It must
be available on the agent's `$PATH`. Two install paths:

- **For local development / conformance tests** in this repo:
  `make atlas-binary` downloads a pinned binary into `./tools/atlas`.
  Test targets prepend that directory to `$PATH` automatically.
- **For agent deployment**: until formae core bundles atlas alongside
  its installer (tracked as a known gap), operators install atlas in
  the agent's container image. The hub's existing Dockerfile already
  does this via `curl -sSf https://atlasgo.sh | sh -s -- --community --yes`.

## Target configuration

A Target carries the DB connection details. Pick the credential
strategy that matches how your secret is stored:

```pkl
import "@formae/formae.pkl"
import "@atlas/atlas.pkl"

hubDb = new formae.Target {
  label = "hub-db"
  namespace = "ATLAS"
  config = new atlas.Config {
    dialect  = "postgres"
    host     = hubDbCluster.res.endpointAddress  // Resolvable from AWS::RDS::DBCluster
    port     = hubDbCluster.res.endpointPort
    database = "hub"
    sslMode  = "verify-ca"

    // Pick one:
    credentials = new atlas.AwsRdsMasterCredentials {
      secret = hubDbMasterSecret.res.secretString  // RDS-managed master secret
    }
  }
}
```

### Credential variants

| Variant | When to use | Shape |
|---------|-------------|-------|
| `AwsRdsMasterCredentials` | RDS master secret rotation | `{ secret = <full secretString> }` — plugin extracts `username` + `password` from the standard `{"username":"...","password":"...","host":"...","port":...}` shape |
| `UsernamePasswordJsonCredentials` | Hand-rolled JSON secret | `{ secret = <secretString> }` — plugin extracts from `{"username":"...","password":"..."}` |
| `UsernamePasswordCredentials` | Direct values (env vars, ad-hoc) | `{ username = "u"; password = "p" }` — separate fields, each can be a Resolvable |

The polymorphic shape future-proofs for additional credential types
(Azure managed identity, GCP IAM auth) without breaking existing PKL.

## Migration resource

```pkl
import "@atlas/schema/migration.pkl"

hubMigration = new migration.Migration {
  stack          = hubStack.res
  label          = "hub-schema"
  target         = hubDb.res
  migrationsUri  = "file:///path/to/migrations"  // or oci://, s3://, git+https://
  targetVersion  = "latest"                       // or "20240101120000" (pinned)
  allowDowngrade = false                          // gate destructive ops
}
```

| Field | Mutability | Notes |
|-------|-----------|-------|
| `migrationsUri` | createOnly | Migration artifact location. `file://` for local-dev; `oci://`, `s3://`, `git+https://` per atlas's native support. Accepts a Resolvable so the artifact location can reference another formae-managed resource (e.g. an `AWS::S3::Bucket` whose contents your release pipeline uploads). Nullable in schema for discovery's sake — the plugin enforces non-empty at Create/Update time. |
| `targetVersion` | mutable | `"latest"` or a pinned atlas version (e.g. `"20240101120000"`). Nullable in schema so Read doesn't overwrite the operator's intent during sync — the plugin enforces non-empty at Create/Update time. |
| `revisionsSchema` | mutable | DB schema for atlas's bookkeeping table. Defaults to `atlas_schema_revisions`. |
| `allowDowngrade` | mutable | When false (default), reconcile errors on requests to lower the applied version. |
| `tool` | createOnly | Reserved for future flyway/sqitch/liquibase support; v1 = atlas only. |
| `appliedVersion` | output | Currently applied version, populated by Read. Reference via `migration.res.appliedVersion`. |
| `baseline` | output | Checksum of the revisions-table state, populated by Read. |

## Hub example — the DAG that drops out

The motivating use case: a stack with an RDS cluster, a secret, a
migration, and an ECS service that depends on the migration being
complete before its task definition rolls out:

```pkl
hubDb = new formae.Target {
  config = new atlas.Config {
    host        = hubDbCluster.res.endpointAddress
    port        = hubDbCluster.res.endpointPort
    database    = "hub"
    credentials = new atlas.AwsRdsMasterCredentials {
      secret = hubDbMasterSecret.res.secretString
    }
  }
}

hubMigration = new migration.Migration {
  target        = hubDb.res
  migrationsUri = "oci://ghcr.io/example/hub-migrations:\(imageTag)"
  targetVersion = "latest"
}

hubService = new aws.ecs.Service {
  taskDefinition = new aws.ecs.TaskDefinition {
    environment = new Listing {
      new { name = "HUB_SCHEMA_VERSION"; value = hubMigration.res.appliedVersion }
      // ... other env vars ...
    }
  }
}
```

Edges in the resulting DAG: `hubDbCluster + hubDbMasterSecret →
hubDb → hubMigration → hubService.taskDefinition → hubService`. A
single `formae apply --mode reconcile` produces a coherent rollout.

### Silent footgun — operator vigilance required

Like every Resolvable-based dependency in formae, **forgetting to
wire `migration.res.appliedVersion` into a downstream resource means
that resource is NOT downstream of the migration**, and may deploy in
parallel with it. For services sharing the DB, the canonical pattern
is the env-var injection shown above. A label / tag / container
metadata key works just as well — the DAG cares about the dependency
edge, not the semantic meaning of the value.

## Discovery & adoption

Atlas Targets are discoverable by default. On each discovery cycle the
plugin probes the Target's database for an `atlas_schema_revisions`
schema; if found, it reports a single unmanaged `Migration` resource
with NativeID `<database>-migration`. If absent (empty DB, or a DB
managed by Flyway/Liquibase/Sqitch — a different plugin's concern), the
Target produces no discoveries.

### What the discovered resource knows

| Property | Source |
|---|---|
| `appliedVersion`, `baseline` | atlas's revisions table |
| `revisionsSchema` | `information_schema` lookup |
| `targetVersion` | = `appliedVersion` at discovery time (current state is the desired state until you bump it) |
| `tool` | `"atlas"` (everything we surface is by definition atlas-tracked) |
| `allowDowngrade` | plugin default (`false`) |
| `migrationsUri` | **not recoverable from the DB** — atlas's revisions table doesn't store the source URI; left absent in the discovered resource |

### Adopting a discovered resource

Write a `Migration` resource whose `label` matches the discovered
NativeID and supply the artifact location you manage:

```pkl
new migration.Migration {
    stack          = appStack.res
    label          = "hub-migration"          // matches discovered NativeID
    target         = hubDbTarget.res
    migrationsUri  = "git+https://github.com/myorg/hub.git//migrations"
    targetVersion  = "latest"
}
```

The agent matches by label, switches the resource from unmanaged →
managed, and subsequent reconciles use the supplied `migrationsUri`.

### Why `migrationsUri` is operator-supplied

The DB only knows that migration version X was applied — not where the
SQL files came from. Different operators store their migrations in
different places (git, OCI, S3, …) and atlas itself doesn't persist the
source URI anywhere. The discovered resource is a "9/10 stub" by design;
the operator filling in `migrationsUri` is also where they confirm
"yes, these are MY migrations on this DB" rather than someone else's
atlas project pointing at the same database.

For artifact locations managed by formae (e.g. an `AWS::S3::Bucket`
that your release pipeline uploads to), `migrationsUri` accepts a
Resolvable so the dependency edge gets wired into the DAG normally.

## Lifecycle semantics

| Op | What it does |
|----|--------------|
| **Create** | Resolves credentials → builds connection URL → runs `atlas migrate apply` to the requested `targetVersion`. Naturally idempotent: re-running against an already-migrated DB is a no-op success. |
| **Read** | Queries the `atlas_schema_revisions` table for the latest applied version. Returns `NotFound` when the revisions table is absent — the agent treats this as "resource deleted out-of-band" and triggers re-Create on the next reconcile. |
| **Update** | Same `migrate apply` flow as Create. When the requested version is lower than what's applied, gates on `allowDowngrade`: defaults to a clear error; with `allowDowngrade=true` runs `atlas migrate down`. |
| **Delete** | **No-op by default.** Mirrors the Atlas Terraform provider's `atlas_migration` resource. Schema and revisions table are preserved so subsequent re-Create resumes from the existing cursor without colliding with already-applied schema. Can be opted into a destructive variant via plugin config — see "Plugin configuration" below. |

## Plugin configuration

The plugin reads operator-supplied configuration from the agent's
`formae.conf.pkl`. Defaults are prod-safe; you only need to override
for ephemeral test/dev environments.

```pkl
import "plugins:/Atlas.pkl" as Atlas

agent {
    resourcePlugins {
        new Atlas.PluginConfig {
            // How Delete handles the DB. Default: "retain" (no-op).
            //   - "retain":        Delete preserves all state. Use for prod.
            //   - "dropRevisions": Delete drops atlas's bookkeeping schema.
            //                      User schema/data is NOT touched. Use for
            //                      ephemeral test/dev DBs.
            deleteMode = "retain"
        }
    }
}
```

**Test environments**: with `deleteMode = "dropRevisions"`, Create and
Update also pass `AllowDirty=true` to atlas, so re-creates against DBs
whose user schema persists from a prior Create succeed (provided
migration SQL is idempotent, e.g. `CREATE TABLE IF NOT EXISTS`). The
conformance harness exercises this path via the `ATLAS_DELETE_MODE`
env var — see the Makefile's `conformance-test-crud` target.

**Future modes** (e.g. `rollback` to run `atlas migrate down` to 0, or
`dropDatabase` for a full clean slate) will land as additional enum
variants without breaking existing configs.

## Local development

```bash
# Build the plugin
make build

# Install into ~/.pel/formae/plugins/atlas/v<version>/
make install

# Download a pinned atlas CLI into ./tools/atlas (one-time, idempotent)
make atlas-binary

# Run unit tests (no external dependencies)
make test-unit

# Run integration tests (spins up postgres via docker, uses ./tools/atlas)
make test-integration

# Run the conformance harness end-to-end (slower, runs a real agent)
make conformance-test-crud
```

### Examples

A working sample lives in `examples/basic/main.pkl`. Adjust the host,
port, credentials, and `migrationsUri` for your environment, then
apply:

```bash
formae apply --mode reconcile examples/basic/main.pkl
```

## Known limitations

- **Postgres only.** `dialect` accepts `"postgres"` exclusively in v1.
- **One Migration per DB.** Atlas supports multiple
  `atlas_schema_revisions` trackers per database (via custom
  `revisionsSchema`), but v1 assumes a single tracker — `List` returns
  one NativeID per Target and `Read` picks the first revisions schema
  it finds. Multi-tracker DBs collapse to whichever tracker
  `information_schema` returns first. Use a separate `formae.Target`
  per tracker until v2 supports per-schema discovery.
- **No full destructive rollback.** The opt-in destructive Delete mode
  drops atlas's bookkeeping only, not user data. `atlas migrate down`
  to 0 as part of Delete is on the roadmap for ephemeral test/dev DBs
  that want a true clean slate.

## License

Apache-2.0
