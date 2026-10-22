# Ownership transfer: owner-only initiation, old owner becomes admin

Decided 2026-10-22 (grill-with-docs session, D30 wave). The household owner
may hand ownership to any existing member. The old owner is demoted to an
**admin** — not a plain member — and the transfer requires the owner's
password, re-entered in the confirmation modal.

## Why

- Ownership is identity-level authority: `perm.go` treats the owner as
  implicit full access with no grant rows consulted, and the owner row is
  untouchable by everyone else's administration. Transferring changes *who is
  untouchable in the household* — the most powerful act the app has.
- It is effectively **irreversible from the initiator's side**: once the
  transfer is actioned, the old owner can no longer transfer it back — only
  the new owner (or another admin) can change access from then on. That is
  why a plain warning modal is not enough: the confirmation requires a
  password re-entry.
- Owner-only initiation. Letting an admin initiate would be a seizure —
  surprising and dangerous — so the act stays with the person who alone holds
  it today.
- The recipient is any current member, with no admin prerequisite: the
  transfer itself confers owner, so requiring a lower rung first would be
  redundant ceremony.

## Consequences

- Migration-free: `household_member.role` swaps (`owner` → `member` for the
  old owner, `member` → `owner` for the recipient) and the two
  `household_grant` rows swap shapes — the old owner's row becomes a full
  admin row, the recipient's becomes the owner-shaped row.
- The old owner **keeps full household access** as an admin, but that
  access is now subject to administration: the new owner and other admins
  can edit or revoke it, including the admin flag. (The alternative —
  demoting the old owner to a plain member — was rejected: it silently strips
  full access from the person who just gave it up.)
- The locked owner row in Settings → Grants follows the **new** owner; the
  old owner's row becomes an ordinary editable admin row.
- UI lives on Settings → Household tab (identity-level action, next to the
  default-household picker), with the confirmation modal listing the
  consequences and a password field at the bottom.
- Only the target household is affected: the old owner's other memberships —
  including ownership of other households — are untouched.
