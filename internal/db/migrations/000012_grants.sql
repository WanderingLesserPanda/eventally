-- +goose Up
-- 000012_grants: the two-tier permission model (ADR-0001, D27).
--
--   household_grant: one row per (user, household) — rights over accounts
--     AS OBJECTS (read/create/edit/delete) plus the admin flag. Edit and
--     delete are scoped by a `created | all` mode: a member's reach is
--     limited to accounts they created; owner/admins operate at `all`.
--
--   account_grant: one row per (user, household, account) — rights over
--     RECORDS on one account. View/edit/delete are each `Related | All`;
--     Related = the member's slice (recorded by them, split party, or
--     settlement payer). Forcing: edit=All implies view=All, delete=All
--     implies both.
--
--   Superseded and dropped: household_member.can_edit,
--   household_member.record_scope, and the member_account_scope table.
--   (Dev-only data — the grant tables are re-seeded by the backfill below,
--   so no conversion of the old trichotomy is needed.)
--
--   Authorship: account/recurring_template gain created_by_user_id so
--     "accounts I created" (household-level created mode) and R5
--     (recurring delete = author-scoped) are checkable.
CREATE TABLE household_grant (
    user_id       BIGINT NOT NULL,
    household_id  BIGINT NOT NULL,
    can_read      BOOLEAN NOT NULL DEFAULT FALSE,
    can_create    BOOLEAN NOT NULL DEFAULT FALSE,
    can_edit      BOOLEAN NOT NULL DEFAULT FALSE,
    can_delete    BOOLEAN NOT NULL DEFAULT FALSE,
    edit_mode     TEXT    NOT NULL DEFAULT 'created'
                  CHECK (edit_mode IN ('created','all')),
    delete_mode   TEXT    NOT NULL DEFAULT 'created'
                  CHECK (delete_mode IN ('created','all')),
    is_admin      BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (user_id, household_id),
    FOREIGN KEY (user_id, household_id) REFERENCES household_member
        (user_id, household_id) ON DELETE CASCADE,
    FOREIGN KEY (household_id) REFERENCES household(id) ON DELETE CASCADE,
    -- implication chain: delete requires edit, edit requires create,
    -- create requires read; the admin flag is open game (any member with
    -- it is managed like a member — admin grants are enforced in code,
    -- not here).
    CHECK (can_read OR NOT can_create),
    CHECK (can_create OR NOT can_edit),
    CHECK (can_edit OR NOT can_delete),
    -- delete=all forces edit=all (and transitively edit implies read).
    CHECK (delete_mode <> 'all' OR (edit_mode = 'all' AND can_read)),
    -- a row that grants nothing is noise; require at least one right.
    CHECK (can_read OR is_admin)
);
CREATE INDEX idx_hgrant_household ON household_grant (household_id);
CREATE INDEX idx_hgrant_user ON household_grant (user_id);

CREATE TABLE account_grant (
    user_id       BIGINT NOT NULL,
    household_id  BIGINT NOT NULL,
    account_id    BIGINT NOT NULL,
    can_read      BOOLEAN NOT NULL DEFAULT FALSE,
    can_create    BOOLEAN NOT NULL DEFAULT FALSE,
    can_edit      BOOLEAN NOT NULL DEFAULT FALSE,
    can_delete    BOOLEAN NOT NULL DEFAULT FALSE,
    view_mode     TEXT    NOT NULL DEFAULT 'Related'
                  CHECK (view_mode IN ('Related','All')),
    edit_mode     TEXT    NOT NULL DEFAULT 'Related'
                  CHECK (edit_mode IN ('Related','All')),
    delete_mode   TEXT    NOT NULL DEFAULT 'Related'
                  CHECK (delete_mode IN ('Related','All')),
    PRIMARY KEY (user_id, household_id, account_id),
    FOREIGN KEY (user_id, household_id) REFERENCES household_member
        (user_id, household_id) ON DELETE CASCADE,
    FOREIGN KEY (household_id, account_id) REFERENCES account
        (household_id, id) ON DELETE CASCADE,
    -- the same implication chain as the household level.
    CHECK (can_read OR NOT can_create),
    CHECK (can_create OR NOT can_edit),
    CHECK (can_edit OR NOT can_delete),
    -- edit=All forces view=All.
    CHECK (edit_mode <> 'All' OR view_mode = 'All'),
    -- delete=All forces both modes to All.
    CHECK (delete_mode <> 'All' OR (edit_mode = 'All' AND view_mode = 'All'))
);
CREATE INDEX idx_agrant_household ON account_grant (household_id);
CREATE INDEX idx_agrant_user ON account_grant (user_id, household_id);

-- Authorship columns (NULL-able, single-column FKs matching the project's
-- user-reference style, e.g. transaction.recorded_by_user_id).
ALTER TABLE account ADD COLUMN created_by_user_id BIGINT
    REFERENCES app_user(id) DEFERRABLE INITIALLY DEFERRED;
CREATE INDEX idx_account_created_by ON account (created_by_user_id);

ALTER TABLE recurring_template ADD COLUMN created_by_user_id BIGINT
    REFERENCES app_user(id) DEFERRABLE INITIALLY DEFERRED;
CREATE INDEX idx_recurring_created_by ON recurring_template (created_by_user_id);

-- Backfill: every member's household row (owner: full + admin; member:
-- read-only — D27 default) and every non-owner member's per-account rows
-- (read-only, All view — the current app shows every member everything,
-- and members default to read access). The owner gets no grant rows:
-- implicit full access in code.
INSERT INTO household_grant (user_id, household_id, can_read, can_create,
                             can_edit, can_delete, edit_mode, delete_mode,
                             is_admin)
SELECT m.user_id, m.household_id,
       TRUE,  m.role = 'owner',  m.role = 'owner',  m.role = 'owner',
       CASE m.role WHEN 'owner' THEN 'all' ELSE 'created' END,
       CASE m.role WHEN 'owner' THEN 'all' ELSE 'created' END,
       m.role = 'owner'
FROM household_member m;

INSERT INTO account_grant (user_id, household_id, account_id, can_read,
                           view_mode)
SELECT m.user_id, m.household_id, a.id, TRUE, 'All'
FROM household_member m
JOIN account a ON a.household_id = m.household_id
WHERE m.role <> 'owner';

-- Seed authorship: the household's earliest user (the same rule the
-- membership backfill used to pick owners) is treated as creator of all
-- pre-existing rows — the real data is dev-only; from now on the app
-- writes created_by_user_id itself.
UPDATE account a SET created_by_user_id = u.id
FROM (
    SELECT household_id, MIN(id) AS id
    FROM app_user GROUP BY household_id
) u WHERE u.household_id = a.household_id;

UPDATE recurring_template t SET created_by_user_id = u.id
FROM (
    SELECT household_id, MIN(id) AS id
    FROM app_user GROUP BY household_id
) u WHERE u.household_id = t.household_id;

-- Creator auto-grant: creating an account immediately gives the creator
-- full access on it (D27: Read+Create+Edit, All modes — a new account is
-- theirs). Inserting here rather than in the app keeps the invariant at
-- the DB level.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION trg_account_grant_for_creator() RETURNS trigger AS $$
BEGIN
    IF NEW.created_by_user_id IS NOT NULL
       AND NEW.created_by_user_id = (
           SELECT m.user_id FROM household_member m
           WHERE m.user_id = NEW.created_by_user_id
             AND m.household_id = NEW.household_id) THEN
        INSERT INTO account_grant (user_id, household_id, account_id,
                                   can_read, can_create, can_edit, can_delete,
                                   view_mode, edit_mode, delete_mode)
        VALUES (NEW.created_by_user_id, NEW.household_id, NEW.id,
                TRUE, TRUE, TRUE, FALSE, 'All', 'All', 'Related')
        ON CONFLICT (user_id, household_id, account_id) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER trg_account_creator_grant
AFTER INSERT ON account
DEFERRABLE INITIALLY IMMEDIATE
FOR EACH ROW EXECUTE FUNCTION trg_account_grant_for_creator();

-- Flush deferred FK events before the destructive section.
SET CONSTRAINTS ALL IMMEDIATE;

-- The superseded permission state.
DROP TABLE member_account_scope;
ALTER TABLE household_member DROP COLUMN can_edit;
ALTER TABLE household_member DROP COLUMN record_scope;

-- +goose Down
SET CONSTRAINTS ALL IMMEDIATE;
ALTER TABLE household_member ADD COLUMN can_edit BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE household_member ADD COLUMN record_scope TEXT NOT NULL DEFAULT 'all'
    CHECK (record_scope IN ('all','accounts','own'));
CREATE TABLE member_account_scope (
    user_id      BIGINT NOT NULL,
    household_id BIGINT NOT NULL,
    account_id   BIGINT NOT NULL,
    PRIMARY KEY (user_id, household_id, account_id),
    FOREIGN KEY (user_id, household_id) REFERENCES household_member
        (user_id, household_id) ON DELETE CASCADE,
    FOREIGN KEY (household_id, account_id) REFERENCES account
        (household_id, id) ON DELETE CASCADE
);
DROP TRIGGER IF EXISTS trg_account_creator_grant ON account;
DROP FUNCTION trg_account_grant_for_creator();
ALTER TABLE recurring_template DROP COLUMN created_by_user_id;
ALTER TABLE account DROP COLUMN created_by_user_id;
DROP TABLE account_grant;
DROP TABLE household_grant;
