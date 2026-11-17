-- 000017: D33 wave — cross-household synced records (ADR-0006).
--
-- A synced record is an ordinary "transaction" row in the target household
-- sitting on the source household's party endpoint (type='organisation',
-- name = the source household's name), with source='sync' and two plain
-- source pointers — no cross-household FKs (the isolation convention).
-- The app layer (D33) adds the three named cross-household surfaces that
-- read/write through these columns — the /sync GET and POST handlers and
-- the void-path cascade — and nothing else may touch them.
--
-- This migration is a no-op for existing rows: the columns are nullable,
-- the marker check passes on every non-sync row, and the UNIQUE
-- constraint treats NULLs as distinct.

-- +goose Up

ALTER TABLE "transaction"
    ADD COLUMN sync_source_household_id BIGINT,
    ADD COLUMN sync_source_txn_id BIGINT;

-- Marker and pointers agree in both directions: a row is a synced copy
-- if and only if both pointers are set.
ALTER TABLE "transaction" ADD CONSTRAINT chk_txn_sync_pointers CHECK (
    (source = 'sync') = (sync_source_household_id IS NOT NULL
                         AND sync_source_txn_id IS NOT NULL));

-- 'sync' joins the other non-manually-typed records (the constraint name
-- is preserved; house convention keeps the checker name stable).
ALTER TABLE "transaction" DROP CONSTRAINT transaction_source_check;
ALTER TABLE "transaction" ADD CONSTRAINT transaction_source_check
    CHECK (source IN ('manual','template','import','tracker','sync'));

-- One synced copy of a source record per household: a partner syncing the
-- same source record re-classifies the one row, never doubles it (and the
-- budget). NULLs are distinct in Postgres UNIQUE, so non-sync rows are
-- untouched.
ALTER TABLE "transaction" ADD CONSTRAINT uq_txn_sync_source UNIQUE (
    household_id, sync_source_household_id, sync_source_txn_id);

-- Cascade lookup: find the synced copies of a source record (the
-- void-path cascade is the only writer).
CREATE INDEX idx_txn_sync_source ON "transaction"
    (sync_source_household_id, sync_source_txn_id);

-- +goose Down

-- Down restores the pre-000017 shape only; restoring data is the
-- backup's job (house convention).
DROP INDEX idx_txn_sync_source;
ALTER TABLE "transaction" DROP CONSTRAINT uq_txn_sync_source;
ALTER TABLE "transaction" DROP CONSTRAINT chk_txn_sync_pointers;
ALTER TABLE "transaction" DROP CONSTRAINT transaction_source_check;
ALTER TABLE "transaction" ADD CONSTRAINT transaction_source_check
    CHECK (source IN ('manual','template','import','tracker'));
ALTER TABLE "transaction" DROP COLUMN sync_source_household_id,
    DROP COLUMN sync_source_txn_id;
