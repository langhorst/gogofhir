-- Full-text index backing the _text and _content search parameters.
--
-- This is the one genuinely engine-specific part of the schema: SQLite uses
-- FTS5, PostgreSQL will use tsvector with a GIN index. Everything else is
-- ordinary tables and B-tree lookups that both engines index identically, so
-- this file is the whole of the divergence rather than the start of it.
--
-- rowid is the resource's surrogate key, which makes reindexing a delete and an
-- insert by rowid rather than a scan.
CREATE VIRTUAL TABLE idx_fulltext USING fts5(
  narrative,   -- the rendered narrative, markup stripped: what _text searches
  content      -- every text value in the resource: what _content searches
);
