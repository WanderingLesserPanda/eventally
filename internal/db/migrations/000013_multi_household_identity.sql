-- 000013: D28 permission-UI wave — multi-household identity + invitations.
--
-- Design: vault D28 + docs/adr/0002-global-username-uniqueness.md.
--
--  1. app_user.household_id -> default_household_id, nullable,
--     ON DELETE SET NULL. The column stops being "which household this user
--     lives in" and becomes "where this user lands at login". Membership
--     questions route through household_member (already a join table,
--     000006). A user survives the deletion of their default household —
--     they may still belong to others; login falls back to their first
--     remaining membership.
--     (Username uniqueness itself needed no change: 000005 already dropped
--     the per-household composite and created uq_user_username on
--     lower(username).)
--  2. household.invite_expiry_days — the household's default invite-code
--     expiry (picklist 1/3/7/14/30, default 3), overridable per invite
--     (000013 code rows carry their own expires_at).
--  3. invite — one-time, single-use household membership codes. The raw
--     code is shown once at creation, then only its SHA-256 hash lives in
--     the row (code_prefix keeps the "K7XQ…" display in the invites list).
--     A code dies by consumption (consumed_at/by), revocation
--     (revoked_at), or expiry (expires_at — whichever first; lookup
--     requires none of the three to be set).

-- +goose Up

ALTER TABLE app_user
    DROP CONSTRAINT user_household_id_fkey;
ALTER TABLE app_user
    ALTER COLUMN household_id DROP NOT NULL;
ALTER TABLE app_user
    RENAME COLUMN household_id TO default_household_id;
ALTER TABLE app_user
    ADD CONSTRAINT user_default_household_id_fkey
        FOREIGN KEY (default_household_id) REFERENCES household(id)
            ON DELETE SET NULL;
CREATE INDEX idx_user_default_household ON app_user (default_household_id);

ALTER TABLE household
    ADD COLUMN invite_expiry_days INTEGER NOT NULL DEFAULT 3
        CHECK (invite_expiry_days IN (1, 3, 7, 14, 30));

CREATE TABLE invite (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    household_id      BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    code_hash         TEXT NOT NULL,          -- sha256 hex of the raw code
    code_prefix       TEXT NOT NULL,          -- first 4 chars, for display
    created_by_user_id BIGINT REFERENCES app_user(id) ON DELETE SET NULL,
    expires_at        TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    consumed_at       TIMESTAMPTZ,
    consumed_by_user_id BIGINT REFERENCES app_user(id) ON DELETE SET NULL,
    revoked_at        TIMESTAMPTZ,
    CHECK (expires_at > created_at),
    CHECK (
        (consumed_at IS NULL AND consumed_by_user_id IS NULL)
        OR (consumed_at IS NOT NULL AND consumed_by_user_id IS NOT NULL)),
    CHECK (
        (revoked_at IS NULL)
        OR (consumed_at IS NULL))             -- a live code can be revoked;
                                              -- a consumed one cannot
);
CREATE INDEX idx_invite_household ON invite (household_id);
CREATE INDEX idx_invite_code ON invite (code_hash);
-- Outstanding codes = un-consumed, un-revoked, unexpired: the 5-outstanding
-- cap and the /join lookup both key off this shape.
CREATE INDEX idx_invite_live ON invite (household_id)
    WHERE consumed_at IS NULL AND revoked_at IS NULL;

-- Smart-join: when /join's username collides with an existing account, the
-- raw code is not echoed again; instead an opaque token (in a 1h HttpOnly
-- cookie) names the (invite, username) pair so the post-login dashboard can
-- offer the modal. The token is NOT the credential — joining re-validates
-- the invite row.
CREATE TABLE invite_join_token (
    token      TEXT PRIMARY KEY,
    invite_id  BIGINT NOT NULL REFERENCES invite(id) ON DELETE CASCADE,
    username   TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_join_token_invite ON invite_join_token (invite_id);

-- +goose Down

-- A NULL default is legal in the new shape; drop those rows before
-- re-tightening (dev-data migration — clobber is acceptable).
DELETE FROM app_user WHERE default_household_id IS NULL;
ALTER TABLE app_user
    DROP CONSTRAINT user_default_household_id_fkey;
ALTER TABLE app_user
    RENAME COLUMN default_household_id TO household_id;
ALTER TABLE app_user
    ALTER COLUMN household_id SET NOT NULL;
ALTER TABLE app_user
    ADD CONSTRAINT user_household_id_fkey
        FOREIGN KEY (household_id) REFERENCES household(id)
            ON DELETE CASCADE;
DROP INDEX IF EXISTS idx_user_default_household;

ALTER TABLE household DROP COLUMN IF EXISTS invite_expiry_days;
DROP TABLE IF EXISTS invite_join_token;
DROP TABLE IF EXISTS invite;
