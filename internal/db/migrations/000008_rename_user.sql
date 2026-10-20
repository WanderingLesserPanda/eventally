-- +goose Up
-- 000008_rename_user: "user" is a Postgres reserved word and was the cause
-- of constant quoting churn. The table is renamed to app_user (schema
-- rework, Gate 3a). pass_hash is renamed to password_hash for clarity.
--
-- Only identifiers change — no data, no constraints, no behavior. Every
-- code site that used to write `"user"` now writes `app_user`, and
-- pass_hash becomes password_hash.

ALTER TABLE "user" RENAME TO app_user;
ALTER TABLE app_user RENAME COLUMN pass_hash TO password_hash;

-- +goose Down
ALTER TABLE app_user RENAME COLUMN password_hash TO pass_hash;
ALTER TABLE app_user RENAME TO "user";
