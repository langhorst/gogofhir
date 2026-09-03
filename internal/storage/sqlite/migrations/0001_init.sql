-- The current version of every resource. pid is the surrogate key everything
-- else joins on; fhir_id is the logical id a client chooses and sees.
--
-- Separating the two is deliberate. Clients supply arbitrary strings as ids on
-- PUT, and those ids are what appear in references, but joins want a compact
-- integer. Conflating them makes every index table carry a variable-length key.
CREATE TABLE resource (
  pid           INTEGER PRIMARY KEY AUTOINCREMENT,
  resource_type TEXT    NOT NULL,
  fhir_id       TEXT    NOT NULL,
  version_id    INTEGER NOT NULL,
  last_updated  INTEGER NOT NULL,          -- microseconds since the epoch
  deleted       INTEGER NOT NULL DEFAULT 0,
  content       BLOB,                      -- NULL for a tombstone
  UNIQUE (resource_type, fhir_id)
);
CREATE INDEX idx_resource_type ON resource (resource_type, last_updated DESC, pid DESC);

-- Every version ever written, including tombstones. A delete is a version, not
-- an erasure: history has to show that the resource existed and stopped.
CREATE TABLE resource_history (
  resource_type TEXT    NOT NULL,
  fhir_id       TEXT    NOT NULL,
  version_id    INTEGER NOT NULL,
  last_updated  INTEGER NOT NULL,
  deleted       INTEGER NOT NULL DEFAULT 0,
  content       BLOB,
  PRIMARY KEY (resource_type, fhir_id, version_id)
);
CREATE INDEX idx_history_updated      ON resource_history (last_updated DESC);
CREATE INDEX idx_history_type_updated ON resource_history (resource_type, last_updated DESC);

-- Search indexes, one table per parameter type.
--
-- Ordinary relational tables with B-tree indexes, not engine-specific JSON
-- indexing: this is what lets the same schema serve SQLite and PostgreSQL, and
-- it is the lesson worth taking from HAPI's design. Every row is written in the
-- same transaction as the resource it indexes, so an index can never describe a
-- version that is not stored.

CREATE TABLE idx_string (
  pid   INTEGER NOT NULL REFERENCES resource (pid) ON DELETE CASCADE,
  code  TEXT    NOT NULL,
  norm  TEXT    NOT NULL,   -- case- and accent-folded, for matching
  exact TEXT    NOT NULL    -- as written, for the :exact modifier
);
CREATE INDEX idx_string_lookup ON idx_string (code, norm);
CREATE INDEX idx_string_pid    ON idx_string (pid);

CREATE TABLE idx_token (
  pid    INTEGER NOT NULL REFERENCES resource (pid) ON DELETE CASCADE,
  code   TEXT    NOT NULL,
  system TEXT    NOT NULL,
  value  TEXT    NOT NULL
);
CREATE INDEX idx_token_lookup ON idx_token (code, value, system);
CREATE INDEX idx_token_pid    ON idx_token (pid);

CREATE TABLE idx_reference (
  pid         INTEGER NOT NULL REFERENCES resource (pid) ON DELETE CASCADE,
  code        TEXT    NOT NULL,
  target_type TEXT    NOT NULL,
  target_id   TEXT    NOT NULL,
  url         TEXT    NOT NULL
);
CREATE INDEX idx_reference_lookup ON idx_reference (code, target_type, target_id);
CREATE INDEX idx_reference_pid    ON idx_reference (pid);

-- Dates are stored as the interval they denote, because "2024" is a year rather
-- than an instant. Prefix comparisons are interval algebra over these bounds.
CREATE TABLE idx_date (
  pid  INTEGER NOT NULL REFERENCES resource (pid) ON DELETE CASCADE,
  code TEXT    NOT NULL,
  low  INTEGER NOT NULL,
  high INTEGER NOT NULL
);
CREATE INDEX idx_date_lookup ON idx_date (code, low, high);
CREATE INDEX idx_date_pid    ON idx_date (pid);

CREATE TABLE idx_quantity (
  pid    INTEGER NOT NULL REFERENCES resource (pid) ON DELETE CASCADE,
  code   TEXT    NOT NULL,
  low    REAL    NOT NULL,
  high   REAL    NOT NULL,
  system TEXT    NOT NULL,
  unit   TEXT    NOT NULL
);
CREATE INDEX idx_quantity_lookup ON idx_quantity (code, low, high);
CREATE INDEX idx_quantity_pid    ON idx_quantity (pid);

CREATE TABLE idx_uri (
  pid   INTEGER NOT NULL REFERENCES resource (pid) ON DELETE CASCADE,
  code  TEXT    NOT NULL,
  value TEXT    NOT NULL
);
CREATE INDEX idx_uri_lookup ON idx_uri (code, value);
CREATE INDEX idx_uri_pid    ON idx_uri (pid);

CREATE TABLE idx_number (
  pid  INTEGER NOT NULL REFERENCES resource (pid) ON DELETE CASCADE,
  code TEXT    NOT NULL,
  low  REAL    NOT NULL,
  high REAL    NOT NULL
);
CREATE INDEX idx_number_lookup ON idx_number (code, low, high);
CREATE INDEX idx_number_pid    ON idx_number (pid);
