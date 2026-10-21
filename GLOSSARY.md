# Eventally

A personal household-finance ledger: members of a household share accounts,
transactions, splits and settlements, with access controlled per account.

## Language

### The household

**Household**:
The unit of shared finance. Members belong to a household; all money lives in
its accounts.
_Avoid_: family, group

**Owner**:
The household's founder. Holds implicit full access to everything and is
untouchable by other members' administration.
_Avoid_: root, admin (when meaning the owner specifically)

**Admin**:
A member granted household-level management: can manage other members'
grants and delete any account, but can never administer the owner.
_Avoid_: moderator, manager

**Member**:
A household participant. May hold household-level and account-level grants;
by default holds read access only.

### Access

**Household-level grant**:
Per-member rights over the household itself: read, create, edit, delete
accounts, and the admin flag. A member's edit and delete are scoped to
accounts they created (`created` mode); an admin's (and the owner's) reach any
account (`all` mode).

**Account-level grant**:
Per-member rights over one account: read, create, edit, and a view mode.
All access to a transaction flows from grants on that transaction's accounts.

**View mode**:
Which transactions a read grant exposes on an account: *Related* (the
member's slice) or *All records* (everything on the account).

**Edit mode** / **Delete mode**:
Mode tiers on a grant, bounding which records or accounts a permission
reaches. On an account-level grant each is `Related | All` (edit = All forces
view = All; delete = All forces both; edit requires read, delete requires
read + edit). On a household-level grant each is `created | all` (member's
reach is limited to accounts they created; owner/admin are `all`), with the
same forcing: delete = `all` forces edit = `all`. On recurring templates,
delete is author-scoped instead: only the creator may delete; split parties
may see and (with edit) edit.

**Related**:
The transactions in a member's slice: ones they recorded, in which they are a
split party, or that they paid as a settlement payer.
_Avoid_: own (too ambiguous — "their own" is exactly what "Related" clarifies)

**Grant**:
A permission row as held by a member — at household level or account level.
Only the owner and admins can create, change or remove grants of non-owner
members.
_Avoid_: permission (when meaning the row held by a user; the individual
booleans — read/create/edit/admin — are the permission *kinds*)
