# Seed-SQL shapes for the two-tier grant model (Gate 5)

Grant management is DB-level only (the grant UI is parked with invitations).
This file documents the canonical shapes for a **two-member household**
(one owner, one member). `user_id` / `account_id` values are placeholders —
look them up first:

```sql
-- Who are the members (and which id to use for user_id)?
SELECT m.user_id, u.username, m.role
FROM household_member m JOIN "app_user" u ON u.id = m.user_id
WHERE m.household_id = 1;

-- Which accounts exist (and which id to use for account_id)?
SELECT id, name, account_type FROM account WHERE household_id = 1 ORDER BY id;
```

The migration 000012 backfill already gives, out of the box:

- **owner**: full household grant (Read+Create+Edit+Delete, `all` modes, admin)
  + a read-only, All-records account grant on **every** account;
- **each member**: read-only household grant (created/created modes)
  + a read-only, All-records account grant on **every** account.

So the defaults below only *tighten or widen* that backfill.

## 1. Member gets a full second "manager" (household-level admin)

```sql
UPDATE household_grant
SET can_create  = TRUE,
    can_edit    = TRUE,
    can_delete  = TRUE,
    edit_mode   = 'all',      -- may edit any account (owner's included)
    delete_mode = 'all',      -- may delete any account (strict delete still applies)
    is_admin    = TRUE        -- may manage every non-owner member's grants
WHERE household_id = 1 AND user_id = :member_user_id;
```

Rules the model enforces regardless: an admin **cannot** change the owner's
grants (the owner row is untouchable), and an admin can add/remove the admin
flag on other members but never on the owner.

## 2. Member gets read+create on ONE account, All records

(Example shape: "give the member their own card account, full visibility,
can add transactions, cannot touch others' rows.")

```sql
UPDATE account_grant
SET can_create = TRUE,
    can_edit   = TRUE,        -- edits are grant-scoped; the mode limits the rows
    edit_mode  = 'Related',   -- only rows they are in (recorded / split / payer)
    view_mode  = 'All'        -- edit=All would FORCE this; here it is explicit
WHERE household_id = 1
  AND user_id      = :member_user_id
  AND account_id   = :their_card_account_id;
```

Note the forcing rules (also enforced by DB CHECK constraints, so the
impossible combinations cannot even be inserted):

| rule | meaning |
|---|---|
| `edit=All ⇒ view=All` | you can't edit rows you can't see |
| `delete=All ⇒ view=All AND edit=All` | deleting everything means seeing+editing everything |
| `edit ⇒ read`, `delete ⇒ read + edit` | the implication chain |

## 3. Member gets read-only on ONE account, All records

(the backfill already does this for every account — shown here because a
tightening pass starts from it)

```sql
UPDATE account_grant
SET can_read  = TRUE,
    can_create = FALSE,
    can_edit   = FALSE,
    can_delete = FALSE,
    view_mode  = 'All'
WHERE household_id = 1 AND user_id = :member_user_id AND account_id = :account_id;
```

## 4. Restrict a member's view of an account to their own slice

```sql
UPDATE account_grant SET view_mode = 'Related'
WHERE household_id = 1 AND user_id = :member_user_id AND account_id = :account_id;
```

"Related" = the rows that member recorded, that they are a split party on,
or that they paid as a settlement payer — **and only within accounts they
have an account grant on**. Remove the account grant and the slice
disappears with it.

## 5. Revoke a member's access to an account

```sql
DELETE FROM account_grant
WHERE household_id = 1 AND user_id = :member_user_id AND account_id = :account_id;
```

Immediate: the member can no longer read *or* act on any record of that
account, including records they themselves created (related-slice rows die
with the grant).

## 6. Revoke the household-level admin from a member

```sql
UPDATE household_grant
SET is_admin = FALSE, can_create = FALSE, can_edit = FALSE, can_delete = FALSE,
    edit_mode = 'created', delete_mode = 'created'
WHERE household_id = 1 AND user_id = :member_user_id;
```

## Creator auto-grant (no SQL needed)

Any account created with `created_by_user_id` set gets that user's
`account_grant` seeded automatically by a DB trigger (Read+Create+Edit,
`All`/`All`/`Related`). Same for recurring templates' authorship, which the
R5 author-scoped delete rule keys off `recurring_template.created_by_user_id`.
