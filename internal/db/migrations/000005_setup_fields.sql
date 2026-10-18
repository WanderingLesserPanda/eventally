-- +goose Up
-- 000005_setup_fields: first-run setup page (initial setup redesign).
--
-- The setup page now collects more than a username + household name:
--   * user.email          contact for the household admin (login is still
--                         by username alias; display_name is what shows on
--                         transactions).
--   * household.language  UI language — 'en' is the only supported value
--                         for now; the CHECK keeps that honest in the DB.
--   * household.first_day_of_week  1..7 (ISO: Monday..Sunday). Stored so
--                         month/week views can honour it later.
--
-- base_currency stays as-is (already defaults to 'MYR', D13); the setup
-- page exposes it but is limited to MYR for now.
ALTER TABLE household
    ADD COLUMN language TEXT NOT NULL DEFAULT 'en'
        CHECK (language IN ('en')),
    ADD COLUMN first_day_of_week SMALLINT NOT NULL DEFAULT 1
        CHECK (first_day_of_week BETWEEN 1 AND 7);

ALTER TABLE "user"
    ADD COLUMN email TEXT NOT NULL DEFAULT '';

-- At most one address per household. Empty emails are allowed (pre-dates
-- this column), but two non-empty duplicates are not — case-insensitive.
CREATE UNIQUE INDEX uq_user_household_email
    ON "user" (household_id, lower(email)) WHERE email <> '';

-- Usernames are GLOBAL: the login matches on username alone (no
-- household scope), so a global unique index is the real invariant.
-- Uniqueness is CASE-INSENSITIVE (lower(username)): Bob01 and bob01
-- cannot both exist. Login matching itself stays CASE-SENSITIVE
-- (handleLoginPost), so a user who creates "Bob01" signs in as "Bob01"
-- — a deliberate split the user asked for (dedupe forgiving, credential
-- exact). It subsumes the (household_id, username) constraint from
-- 000001_init, which is dropped here to keep the schema honest.
ALTER TABLE "user" DROP CONSTRAINT user_household_id_username_key;
CREATE UNIQUE INDEX uq_user_username ON "user" (lower(username));

-- +goose Down
DROP INDEX IF EXISTS uq_user_username;
ALTER TABLE "user"
    ADD CONSTRAINT user_household_id_username_key
    UNIQUE (household_id, username);
DROP INDEX IF EXISTS uq_user_household_email;
ALTER TABLE "user" DROP COLUMN IF EXISTS email;
ALTER TABLE household
    DROP COLUMN IF EXISTS first_day_of_week,
    DROP COLUMN IF EXISTS language;
