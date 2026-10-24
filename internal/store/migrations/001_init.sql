-- Investigations and their event streams.
CREATE TABLE investigations (
    id          TEXT PRIMARY KEY,
    incident    TEXT NOT NULL,              -- core.Incident JSON
    status      TEXT NOT NULL,
    report      TEXT,                       -- core.Report JSON
    error       TEXT,
    fingerprint TEXT,                       -- alert fingerprint for dedupe
    created_at  INTEGER NOT NULL,           -- unix nanoseconds
    updated_at  INTEGER NOT NULL
);

CREATE INDEX investigations_created_at ON investigations (created_at);
CREATE INDEX investigations_fingerprint ON investigations (fingerprint, created_at);

CREATE TABLE events (
    investigation_id TEXT    NOT NULL REFERENCES investigations (id) ON DELETE CASCADE,
    seq              INTEGER NOT NULL,
    type             TEXT    NOT NULL,
    at               INTEGER NOT NULL,      -- unix nanoseconds
    data             TEXT    NOT NULL,      -- JSON
    PRIMARY KEY (investigation_id, seq)
);
