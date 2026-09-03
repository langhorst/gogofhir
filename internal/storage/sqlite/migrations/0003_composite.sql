-- Composite search parameters ask a question about one occurrence: "an
-- Observation whose code is X *and whose value* is Y", where both must come
-- from the same component rather than from different ones.
--
-- seq records which occurrence of the composite's base expression a row came
-- from, so a composite query can require its components to agree on it. Rows
-- from ordinary parameters leave it at zero and never consult it.
--
-- Components are indexed under a synthetic code, "<composite>$<n>", which lets
-- them reuse the typed index tables rather than needing a table of their own:
-- a token component still needs a system and a code, a quantity component still
-- needs a range.
ALTER TABLE idx_string   ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE idx_token    ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE idx_reference ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE idx_date     ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE idx_quantity ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE idx_uri      ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE idx_number   ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
