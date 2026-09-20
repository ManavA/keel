# Backup and restore runbook

Back up three things: Postgres, Meilisearch, and the media bucket. Postgres
is the source of truth — search documents and stored media can both be
rebuilt from it, slowly, which is why the database backup is the one that
decides how much you can lose.

## Postgres

Take a full logical dump with `pg_dump` in custom format. It includes the
data, the schema, and the migration ledger (`schema_migrations`), which is
what makes the restore behave (see below). Run it against the primary on a
schedule — nightly at minimum, and keep seven daily dumps plus four weekly
ones. A dump you have never restored is a hope, not a backup: restore one to
a scratch database on a schedule too, and follow the checklist at the end.

```
pg_dump -h $PGHOST -p $PGPORT -U $PGUSER -Fc -f keel-$(date +%F).dump keel
```

`pg_dump` needs nothing but a connection. It does not lock callers out: it
takes a consistent snapshot while the service keeps running. The one thing
it does not give you is anything committed after it started. If losing up
to a day of writes is unacceptable, that is what point-in-time recovery is
for (next section), not a more frequent dump.

To restore to a fresh database:

```
createdb keel_restored
pg_restore -h $PGHOST -p $PGPORT -U $PGUSER -d keel_restored keel-2026-09-19.dump
```

Point the service at the restored database and run the migrations before
serving traffic. What happens then depends on the ledger — stated exactly
in "The migration ledger after a restore" below. The short version: a full
dump restores the ledger with everything else, so the migration run applies
nothing and the service starts on the restored schema as-is.

If the dump predates a migration file the binary carries (the backup is
older than the deploy), that file is pending and runs normally. This is the
ordinary case after a restore, not an error: the run applies exactly the
migrations committed since the backup was taken.

### Point-in-time recovery

On Cloud SQL, enable automated backups and point-in-time recovery on the
instance. The dump above still matters — it is portable and restorable
anywhere, while a Cloud SQL backup restores only to Cloud SQL — but PITR
covers the writes committed after the last dump. Recovery is to a new
instance, not in place: create the clone at the target timestamp, verify it
with the checklist below, then point the service at it.

## Meilisearch

Meilisearch holds a copy of what Postgres already has, plus index settings.
Back it up with a snapshot:

```
curl -H "Authorization: Bearer $MEILI_MASTER_KEY" -X POST $MEILI_URL/snapshots
```

This writes one `data.ms.snapshot` file into the snapshot directory. Copy it
off the host on the same schedule as the database dump. Snapshots are
asynchronous: the POST enqueues a task, so confirm it finished before
copying:

```
curl -H "Authorization: Bearer $MEILI_MASTER_KEY" $MEILI_URL/tasks/<taskUid>
```

Restore by starting a fresh Meilisearch with the snapshot:

```
meilisearch --import-snapshot /path/to/data.ms.snapshot
```

The snapshot carries documents and settings together, so a restored index
needs no settings repair. When the snapshot is older than the database —
documents committed after it was taken — reindex from Postgres, which is the
source of truth, and then confirm the settings landed with
`Searcher.SetupIndexAndVerify` (for the drift it checks and the repair it
runs, see the `search/meili` package doc). A missing filterable attribute
after a restore reads as rejected queries, not as missing data, so run the
drift check even when document counts look right.

If there is no snapshot at all, the same two steps — reindex from Postgres,
then `SetupIndexAndVerify` — rebuild the index from nothing. It takes
longer; that is the only difference.

## Media bucket

The media in GCS is written once and read often, so the failure mode is
accidental deletion or overwrite, not gradual drift. Turn on object
versioning on the bucket:

```
gcloud storage buckets update gs://$BUCKET --versioning
```

Versioning keeps every generation of an overwritten or deleted object, so
recovery is copying the live generation back:

```
gcloud storage ls -a gs://$BUCKET/<object>
gcloud storage cp gs://$BUCKET/<object>#<generation> gs://$BUCKET/<object>
```

Set a lifecycle rule to delete noncurrent generations after the same
retention you keep database dumps, or versioning quietly doubles storage.
`LocalStore` has no equivalent: back up its directory with the host, and
treat a lost local directory the same as a lost snapshot — re-derive it
from the database rows that reference it.

## The migration ledger after a restore

Stated exactly, because getting this wrong replays schema changes against
live tables. `migrate.Run` keeps one row per applied file in the
`schema_migrations` table (filename primary key, checksum, timestamp), and
each migration plus its ledger row commits in a single transaction. On every
run it creates the ledger table if missing, reads which filenames are
recorded, applies only the rest in lexical order, and stops at the first
error. Three cases follow:

1. **The restore includes the ledger (the normal `pg_dump`).** The ledger
   after restore is exactly what it was when the backup was taken: same
   rows, same checksums. The next `migrate.Run` reports every recorded file
   as skipped and applies only files added since the backup. Verified by
   restoring a dump and re-running: `applied=[] skipped=[both files]`, row
   counts and checksums unchanged.
2. **The restored database has no matching ledger rows** — a backup taken
   before the ledger existed, a dump that excluded the table, a dropped
   ledger, or a brand-new database. `Run` recreates the empty ledger and
   replays every file from the beginning. This is why each file must be
   safe to apply twice (`create table if not exists`, `add column if not
   exists`): verified by dropping the ledger on a restored database and
   re-running — both files applied again, data untouched. A file that is
   not idempotent fails here, and the run stops naming it with nothing from
   it committed.
3. **A file edited after it was applied never runs its new content.**
   The ledger keys on the filename, so after a restore the checksum check
   behaves exactly as on the original: `Run` returns `ErrChecksumDrift`
   naming the file, after applying everything pending. If the restore is
   meant to carry a schema change, ship it as a new migration file.

The `Baseline` option does not change any of this. It only affects a
database that already has tables but no ledger from before `migrate` was
introduced, recording everything up to the baseline as applied instead of
replaying it. A restored database either has its ledger (case 1) or it does
not (case 2).

## Restore-verification checklist

Run these against the restored database before routing traffic to it. Each
one catches a different silent failure:

- [ ] `migrate.Run` output: `applied` names only the migrations committed
  since the backup, or nothing. Anything else means the ledger did not
  survive the restore — see case 2 above.
- [ ] Ledger contents match the backup:
  `select filename, substr(checksum, 1, 12) from schema_migrations order by filename`.
- [ ] Row counts on the tables that matter, compared against the source at
  backup time. A restore that applied cleanly but to the wrong dump is
  otherwise indistinguishable from the right one.
- [ ] `/readyz` returns 200 on a service pointed at the restore. It runs
  the dependency checks; `/healthz` only says the process is running.
- [ ] Meilisearch: document count, one search for a known term, and the
  settings drift check. Counts verify the documents; only the drift check
  verifies the settings.
- [ ] Media: fetch one public object and one signed URL per bucket. A
  restored database pointing at an unrestored bucket serves broken images
  with no error anywhere.
- [ ] One end-to-end pass that writes: sign in, search, save. A restore
  verified only with reads hides a read-only connection string, which
  surfaces at the first write in production.
