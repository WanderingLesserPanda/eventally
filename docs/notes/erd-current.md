# Eventally ERD — current schema (migration 000011, v11)

Rendered from the live database state after the Gates 1–4 rework.
**Bold-bordered** tables are the permission model being re-designed in ADR-0001
(`household_member.can_edit` / `record_scope` + `member_account_scope` are
superseded — see the preview at the bottom).

```mermaid
erDiagram
    household ||--o{ app_user : "members belong to"
    household ||--o{ account : "owns"
    household ||--o{ category : "owns"
    household ||--o{ merchant : "owns"
    household ||--o{ party : "owns"
    household ||--o{ session : "auth"
    household ||--o{ transaction : "records"
    household ||--o{ settlement : "records"
    household ||--o{ recurring_template : "schedules"
    household ||--o{ reconciliation : "reconciles"

    app_user ||--o| household_member : "membership row"
    app_user ||--o{ member_account_scope : "scoped to"
    app_user |o--o| party : "has a party row"
    app_user |o--o{ transaction : "records (NULL-able)"
    app_user ||--o{ settlement : "pays as"
    app_user |o--o{ reconciliation : "records"

    household_member ||--o{ member_account_scope : "when record_scope=accounts"
    account ||--o{ member_account_scope : "scoped account"

    account |o--o| account : "parent card (family)"
    account ||--o{ transaction : "origin/destination endpoint"
    account ||--o{ settlement : "settlement account (NULL-able)"
    account ||--o{ recurring_template : "generated into"
    account ||--o{ reconciliation : "per card/month"
    account ||--o{ transaction_split : ""

    transaction |o--o| transaction : "parent txn (correction)"
    transaction ||--o{ transaction_split : "shares"
    transaction |o--o| recurring_template : "generated from"
    transaction ||--o{ settlement : "settled via (NULL-able)"
    transaction |o--o| category : "classified as"
    transaction |o--o| merchant : "at"
    transaction |o--o| party : "origin/destination"

    transaction_split ||--o{ settlement_allocation : "allocated against"
    settlement ||--o{ settlement_allocation : "pays"

    category |o--o| category : "parent"
    category ||--o{ transaction : "classifies"
    category |o--o| merchant : "default for"
    category ||--o{ recurring_template : "for"

    merchant ||--o{ transaction : "at"
    merchant ||--o{ recurring_template : "at"

    party ||--o{ transaction_split : "split participant"
    party ||--o{ transaction : "origin/destination"

    household {
        bigint id PK
        text name
        text base_currency
        text language
        smallint first_day_of_week
    }
    app_user {
        bigint id PK
        bigint household_id FK
        text username
        text password_hash
        text display_name
        text email
    }
    household_member {
        bigint user_id FK PK
        bigint household_id FK PK
        text role "owner | member -- SUPERSEDED: role stays, rest re-designed"
        boolean can_edit "SUPERSEDED by household-level grant"
        text record_scope "all | accounts | own -- SUPERSEDED"
    }
    member_account_scope {
        bigint user_id FK PK
        bigint household_id FK PK
        bigint account_id FK PK
    }
    account {
        bigint id PK
        bigint household_id FK
        text name
        text account_type
        text balance_type
        text card_type
        numeric credit_limit
        bigint parent_id FK "card family (D19)"
        boolean is_active
    }
    category {
        bigint id PK
        bigint household_id FK
        text name
        bigint parent_id FK
        text transaction_class
        numeric budget_per_month
    }
    merchant {
        bigint id PK
        bigint household_id FK
        text name
        bigint default_category_id FK
    }
    party {
        bigint id PK
        bigint household_id FK
        text name
        text type "person | business | other"
        text relationship_type
        bigint app_user_id FK NULL
    }
    transaction {
        bigint id PK
        bigint household_id FK
        bigint recorded_by_user_id FK NULL
        date transaction_date
        text class "expense | income | transfer"
        text total_amount
        text charged_currency
        numeric exchange_rate
        text record_status "pending | approved"
        bigint parent_transaction_id FK
        text source "manual | recurring"
        bigint recurring_template_id FK NULL
        bigint origin_account_id FK NULL
        bigint destination_account_id FK NULL
        bigint origin_party_id FK NULL
        bigint destination_party_id FK NULL
    }
    transaction_split {
        bigint id PK
        bigint transaction_id FK
        bigint party_id FK
        numeric amount
    }
    settlement {
        bigint id PK
        bigint household_id FK
        bigint account_id FK NULL "money comes from here"
        bigint paid_by_user_id FK
        numeric amount
        date payment_date
        bigint ledger_transaction_id FK NULL
    }
    settlement_allocation {
        bigint id PK
        bigint payment_id FK "settlement.id"
        bigint split_id FK "transaction_split.id"
        numeric amount
    }
    recurring_template {
        bigint id PK
        bigint household_id FK
        text name
        bigint merchant_id FK NULL
        bigint category_id FK NULL
        bigint account_id FK "generated txn lands here"
        numeric amount
        jsonb split_spec "parties via app_user ids"
        smallint day_of_month
    }
    reconciliation {
        bigint id PK
        bigint household_id FK
        bigint account_id FK
        date statement_month
        numeric actual_balance
        numeric recorded_balance
        bigint recorded_by_user_id FK NULL
    }
    session {
        text id PK
        bigint household_id FK
        bigint user_id FK
        text csrf_token
        timestamptz expires_at
    }
```

## Notes on the permission-relevant edges

- **`transaction` → accounts**: at least one of `origin_account_id` /
  `destination_account_id` / a party endpoint is always set (Gate 3c CHECK) —
  so every transaction is reachable through account grants. The ERD shows the
  two account FKs; party endpoints are the fallback for pure "outside"
  movements.
- **`settlement` → accounts**: touches **two+ accounts** per settlement —
  its own `account_id` *plus* the transaction account(s) behind each of its
  `settlement_allocation.split_id` links. This is the "all-involvement" rule
  pending Q7: a permission check on a settlement must cover both legs.
- **`recurring_template`**: exactly one `account_id`; the split parties live in
  the JSONB `split_spec` (no FK — they resolve to `party`/`app_user` at
  generation time).
- **`party.app_user_id`**: a member's own party row, seeded by trigger when
  the membership is inserted (Gate 3c) — the join that makes "member is a
  split participant" checkable.

## Preview: the two-tier grant model (ADR-0001 — NOT in the database yet)

```mermaid
erDiagram
    household_member ||--o{ household_grant : "one row"
    household_member ||--o{ account_grant : "one row per account"

    household_grant {
        bigint user_id FK PK
        bigint household_id FK PK
        boolean can_read
        boolean can_create
        boolean can_edit
        boolean can_delete
        text edit_mode "created | all"
        text delete_mode "created | all -- forces edit_mode=all"
        boolean is_admin
    }
    account_grant {
        bigint user_id FK PK
        bigint household_id FK PK
        bigint account_id FK PK
        boolean can_read
        boolean can_create
        boolean can_edit
        boolean can_delete
        text view_mode "Related | All"
        text edit_mode "Related | All -- forces view_mode=All"
        text delete_mode "Related | All -- forces both"
    }
```

Supersedes: `household_member.can_edit`, `household_member.record_scope`, and
the `member_account_scope` table (all dropped in a new migration).
Owner: implicit full, no rows needed. Members default: read-only.
