-- Idempotent for conformance harness: re-apply runs against a DB that
-- may still hold the user schema from a prior Create (Delete drops only
-- atlas's bookkeeping in destructive mode; user tables persist).
CREATE TABLE IF NOT EXISTS users (id INT PRIMARY KEY, email TEXT NOT NULL);
