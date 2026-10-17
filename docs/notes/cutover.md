# Cutover guide — old spreadsheet system → Eventally

When you (or your partner) run this, the app becomes the sole source of
truth and the old spreadsheet bases get exported and archived. This is the
last section of the MVP build (P9).

> **Private figures live outside this repository.** Exact verified totals,
> pending-row counts, and the flagged-transaction review list are in the
> private import report and the agent's working notes — not here, since
> this repository is public. Check those documents for the numbers to
> confirm before cutover.

## 0. What is ready

- The app (Go + Postgres, `docker-compose.yml`) is feature-complete for the
  MVP: auth (2 users), reference data, transactions with N-way splits, quick
  capture, card views + payments/allocations, reconciliation, dashboard,
  recurring templates, pending-review queue.
- **The one-off import has been run and verified** against the full history
  in the old system. The private import report lists the acceptance numbers
  (recorded balance, outstanding by person, charges/payments/credits,
  integrity checks). If any of them surprise you, stop and ask before
  cutover.

## 1. Deploy on your home server (your step)

The app image is prebuilt by GitHub Actions on every push to `main` and
published to the repo's own container registry:
`ghcr.io/wanderinglesserpanda/eventally:latest`.

1. In the TrueNAS Shell, log in to the registry once (only the app image
   needs it; Postgres comes from Docker Hub):
   ```
   docker login ghcr.io
   # username: your GitHub account
   # password: a personal access token (fine-grained, read-packages scope
   #           on the eventally repo only — Settings → Developer settings)
   ```
   If TrueNAS ever clears the login (e.g. after an OS update), repeat this.
2. **Apps → Discover → ⋮ → Install via YAML** and paste the compose
   snippet (the `docker-compose.yml` in the repo, with the two secrets
   filled in — generate them in the shell with `openssl rand -hex 32` for
   `EVENTALLY_SECRET` and any 8–64 character string of your choice for the
   DB password; special characters `! @ # $ % ^ & * + -` all work).
   Or `docker compose up -d` from a clone on any Docker host.
3. First visit `http://<host>:8090` — the first-run setup page creates the
   household and the first user.
4. Re-run the import script against the new database (one-off, run once)
   and re-check the acceptance numbers from the private import report on
   the new database — they must match to the sen.
5. Spot-check a few pages against the old system:
   - `/cards` — recorded balance; outstanding by person.
   - `/reconcile` for any recent month — recorded balance + delta.
   - Dashboard cards (top categories, budget vs actual, balances,
     outstanding, pending-review count).

## 2. One full monthly cycle in the app (the acceptance gate)

Do one real statement month end-to-end — this is the check that says
"the app can replace the old system for daily life":

1. **Capture** a few transactions, mobile and desktop
   (`/capture`: amount, card, merchant, who-paid).
2. **Pending → review**: approve them in `/review`.
3. **Statement check**: compare captured total against the bank statement
   for the day/period.
4. **Payments**: record at least one **overpayment** and one **partial**
   payment on the card page; verify allocations land on the right splits.
5. **Outstanding**: the by-person summary must match a hand calculation.
6. **Reconciliation**: the month's delta must explain any remainder
   (unpaid items, cashback not yet posted, etc.).

Pass = ready for step 3. Fail = note the gap, fix it, re-run.

## 3. Data cleaning (the imported pending items)

The import deliberately left some rows as **`pending`** for in-app cleaning
(data cleaning happens in the app, not in the old system). The private
import report contains the exact list, including:

- non-card pending rows to review/approve (or void) in `/review`, and
  pending card charges to confirm against the statement;
- a small number of near-duplicate transactions flagged for a human
  decision;
- a handful of repayments whose link to the original charge had to be
  inferred — worth confirming;
- a few rows where a split participant was unnamed in the source, so the
  share was assigned by inference — verify those splits.

## 4. Cut over (only after step 2 passes)

1. Export the old bases (CSV/zip) and store them as archives.
2. Mark them read-only / archive them.
3. The app is now the sole source of truth. Keep the archives for a while —
   they are the rollback path.

## 5. Rollback (if something is wrong)

Do **not** archive the old bases until step 4. Until then, the app is a
copy, not the original — nothing is lost by abandoning a trial run.
