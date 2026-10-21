# Global username uniqueness with multi-household membership

Decided 2026-10-21 (grill-with-docs session, permission-UI wave, D28). A
username is **one person, unique across the whole app**, and a person may
belong to many households. The user row's `household_id` column is redefined
as `default_household_id` — the household the user lands in at login — and
every membership question routes through `household_member`.

## Why

- Per-household uniqueness (the status quo, `UNIQUE(household_id, username)`)
  lets the same username exist in two households as the same person. That
  ambiguity is exactly what a cross-household switcher and a global `/join`
  page make visible; the user judged cross-household username duplicates as
  "causing issues" and settled on app-global uniqueness.
- Email is not a viable identity here — it is optional on a user row and
  users are created offline at first-run setup.
- A separate identity table was considered and rejected: a larger rewrite
  with no currently visible benefit; the `household_member` join table
  already exists and already models one user in many households.

## Consequences

- Migration 000013: `UNIQUE(household_id, username)` → `UNIQUE(username)`;
  `app_user.household_id` renamed `default_household_id`.
- Every query that lists a household's users must join through
  `household_member` — a user's default household is not the only one they
  belong to. (Four such queries today: participant dropdowns on the capture,
  card, recurring, and review pages.)
- `/join` validates the invite code **before** looking at the username, so
  a username collision there means the person exists and should sign in
  instead (the "smart join" flow).
- Login landing order: `default_household_id` → first remaining household
  (alphabetical) → "create a new household?" prompt, which is why in-app
  household creation (creator becomes owner) lands in this wave.
