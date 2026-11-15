# Schedules: cron expressions and shared ticks

Status: built for #246. A trigger configured as before is the trigger it was,
and a store that holds none of the two new schedule kinds keeps its schema:
every record about it, and every journal entry, is written byte for byte as
before.

Two schedule kinds are added to the existing `schedule` trigger, beside
`interval`, `daily`, `weekly` and `once`:

- **`cron`**: a five-field cron expression in the schedule's IANA
  `timezone`, ticked by the Runner's own clock, as every other schedule is;
- **`tick`**: a **shared interval** the operator names (`15m`, `1h`, `1d`),
  optionally narrowed by a cron expression, ticked only when an external
  scheduler calls `POST /v1/ticks` for that interval.

Both are configured, changed, previewed, enabled and paused through the
trigger routes that exist (`POST /v1/jobs/{job}/triggers`,
`POST /v1/triggers/{trigger}/state`), held to the operator's bounds
(section 4), and state their next run time as every schedule trigger does,
in `nextAt`.

## 1. What was there

- A schedule held as data and changed through the API: the `schedule`
  trigger, one per job and so per scope identifier, saved paused, its
  revision checked on each change (`triggers_store.go`, `schedule.go`).
- The next run time: `Trigger.nextAt`, set when the trigger is enabled and
  moved on by each tick.
- The missed rule: `missed`, `skip` or `latest`, for a schedule ticked more
  than 30 seconds after its due time.
- Several Runners on one store: a due time makes one occurrence, by the
  trigger's row version (D17, [store-postgresql.md](store-postgresql.md),
  section 12) and the occurrence's identity `schedule:<revision>:<due>`.
- The Runner's own clock: once a second it reads every enabled trigger and
  ticks those due (`automationTick`, `triggers_dispatch.go`). There was no
  route an external scheduler could call: a Google Cloud trigger is one
  Scheduler job bound to one trigger, pulled over Pub/Sub.

What is new is a cron expression held to bounds, and **one call that ticks
every trigger bound to one interval**, so that an external scheduler need
hold one job per interval rather than one per trigger, and a Runner that does
not run continuously can still be ticked.

## 2. The two kinds

A `cron` schedule takes `cron`, `timezone`, `startAt` and optionally `endAt`,
and no `everySeconds`, `time` or `weekday`. Its next run time is the first
match of the expression after the last one, at or after `startAt` and before
`endAt`. The Runner's own clock ticks it, with the 30-second grace of every
schedule.

A `tick` schedule takes `tick`, the shared interval's name, `timezone`,
`startAt`, optionally `endAt`, and optionally `cron`. Without `cron` it is
due at every tick of the interval; with it, at the **first tick at or after
each match**, so `{"tick":"15m","cron":"7 9 * * 1-5","timezone":"America/Toronto"}`
runs at the 09:15 tick of each weekday in Toronto. The Runner's own clock
never ticks it: only a call for its interval does (section 5).

A schedule of another kind that names `cron` or `tick` is refused,
`invalid_schedule`.

## 3. The cron expression

**The grammar** is small and stated in full (`cron.go`):

```
expression = field " " field " " field " " field " " field
field      = item *("," item)
item       = "*" | number | number "-" number | ("*" | number "-" number) "/" step
```

The fields are minute (0-59), hour (0-23), day of the month (1-31), month
(1-12) and day of the week (0-6, Sunday 0), one space apart, at most 256
bytes. A number is one or two decimal digits; a range runs upward; a step is
from 1 to the field's largest value. Names (`JAN`, `MON`), `?`, `L`, `W`,
`#`, `7` for Sunday, a step on a single number, a seconds field and the `@`
macros are refused, `invalid_cron`, and the refusal repeats nothing of the
expression.

**The day rule** is the one common cron readers (Vixie cron, cronie) apply:
a day field that starts with `*` (`*` or `*/2`) is unrestricted for this
rule, any other spelling (`1-31/2` included) restricts it; when both day
fields are restricted, a day either one matches matches; when either is
unrestricted, a day both match matches. So `0 0 */2 * 1` runs on odd days
that are Mondays, and `0 0 1-31/2 * 1` on odd days and on Mondays.

**Evaluation and granularity.** An expression matches whole minutes of the
local wall clock in the schedule's zone. A local time that a time-zone
transition skips does not run; one it repeats runs once, at its first
occurrence, as `daily` and `weekly` do. The next and the latest match are
looked for within eight years and two days of the time asked, past the
longest gap between two leap days, so that a transition that skips a yearly
match does not end the schedule. The reader uses the standard library only.

On the Runner's own clock, a `cron` schedule is ticked within the clock's
one-second loop, as every schedule is: a match runs within about a second of
its minute, while the Runner runs. A `tick` schedule runs only as finely as
its interval: a match runs at the tick at or after it.

## 4. The operator's settings and the bounds

In [service mode](service-mode.md), the configuration's optional
`schedules` member is one object of:

- `sharedIntervals`: at most 16 distinct names, each a number of minutes
  (`m`) or hours (`h`), or `1d`, from one minute to one day, that divides a
  day evenly, with no sign and no leading zero (`5m`, `15m`, `1h`, `6h`,
  `1d`); its ticks fall at the same times each day, counted from midnight UTC;
- `minimumSeconds` and `maximumSeconds`: the shortest and the longest gap a
  cron expression may have between two matches, each from 60 to 31,622,400
  (366 days), the minimum not above the maximum.

Each member at most once, spelled exactly, none null, and nothing else; a
refusal stops the start, as every refusal of the configuration does. Without
the member, or without one of the two bounds, the defaults hold: no shared
interval, a minimum of 60 seconds and a maximum of 366 days. A companion's
boot line takes no `schedules`: it has the defaults and no shared interval.
`GET /v1/status` states the settings in force, under `schedules`, and lists
the capabilities `cron-schedules` and `shared-interval-ticks`.

**The gaps** of an expression are read on the local wall clock over one
whole 400-year Gregorian cycle, from its last match back round to its first,
so a gap that falls once in four years or more (a match on 29 February) is
counted. A time-zone transition can
shorten one actual gap by its change of offset, usually one hour; the floor
of one minute is the reader's own granularity. On a `tick` schedule the
shortest gap allowed is the larger of the minimum and the interval, since two
matches closer than one interval would fall on one tick.

**Refusals.** Each is `422`, with one code per reason, and writes nothing:

| Code | When |
|---|---|
| `invalid_cron` | The expression is out of the grammar. |
| `cron_never_matches` | It matches no date (`0 0 30 2 *`). |
| `cron_interval_too_short` | Two matches are closer than the minimum, or, on a `tick` schedule, than the interval. |
| `cron_interval_too_long` | Two matches are further apart than the maximum. |
| `unknown_shared_interval` | The interval is one the operator does not name. |
| `invalid_schedule` | A `cron` schedule without its expression or with other timing; a `tick` that is not an interval's name, or that takes `everySeconds`, `time` or `weekday`; another kind naming `cron` or `tick`; the zone, start or end as before. |

The bounds in force are checked wherever a trigger's configuration is: when a
job is created with it, when it is configured or previewed, when it is
enabled, and when a job upgrade carries it to a new revision. A job upgrade
refused by them answers the same `422` code, not `409`
`trigger_upgrade_incompatible`, which stays the answer for a trigger the new
release cannot serve. A trigger
enabled before the operator narrowed them keeps its schedule until it is
changed or enabled again (section 9).

## 5. The shared tick

`POST /v1/ticks` takes `{"interval":"15m"}`, and optionally `"at"`, the tick:
an RFC 3339 time that is a multiple of the interval since midnight UTC, from
the year 2000 to 2200, no more than 30 seconds ahead of the Runner's clock
and no more than one interval behind it (section 6). Without `at`, the tick is the last at or before the Runner's clock plus 30
seconds, so that a scheduler that calls a little early names the tick it
means. It answers `200` with `{"interval","at","occurrences"}`, the number of
occurrences this call recorded, accepted or skipped.

Refused: an interval the operator does not name, `422`
`unknown_shared_interval`; an `at` out of that form, `422` `invalid_tick`; a
body member more, `400`; a Runner held to the manual admission gate, as every
automation is; an unhealthy Runner, `503` `runner_unavailable`.

**It is the installation's.** The control API's credential alone admits it:
the owner bearer, or in service mode the identity token, as every route; a
trigger key is not one. It takes no scope identifier: a request that names
one is refused, `400` `invalid_scope`, before anything is read, as every
route that takes none refuses one
([scope identifiers](scope-identifiers.md), section 3). A scope identifier
is a label and a filter, not an authorization boundary, and the tick ticks
the triggers of every scope, as the Runner's own clock does: a request that
names a scope cannot narrow a tick to it, nor tick another scope's jobs under
it. On the postgresql kind the tick reads the store for every scope, as the
clock does ([store-postgresql.md](store-postgresql.md), section 6).

**What it does.** Under the lock the Runner's own automation takes, it reads
every enabled trigger, passes by those paused or not bound to the interval,
and ticks each of the others at the tick, as the clock ticks a schedule
(`scheduleDue`, `triggers_dispatch.go`): a trigger whose `nextAt` is at or
before the tick admits its due time's occurrence, by its missed rule when
that time is before the tick (section 6), and moves `nextAt` on to its next
tick after the tick, in one transaction that writes the trigger first, at the
row version the tick read (D17). Each occurrence a call records is journaled
by the call's requester, the installation or, in service mode, the caller
its identity token names, as an event published through the control API is
by its publisher; the Runner's own clock's are the trigger's, at its
revision.

**A repeated or late call records nothing twice.** A second call for the same
tick, on this Runner or another of the store, finds `nextAt` after the tick
and records nothing; one that read the trigger before the first call wrote
it meets the version moved on, and records nothing, failing nothing; the
occurrence's identity, `schedule:<revision>:<due>`, keeps a second
occurrence of one due time out whatever the order. A call for a tick before
the trigger's `nextAt` records nothing; one for a tick more than one interval
behind the Runner's clock is refused (section 6). A `tick` trigger enabled,
or paused and enabled again, is next due at its first tick more than 30
seconds after the Runner's clock: a call may tick a time up to 30 seconds
before it, and that tick is not due again under the trigger's new revision,
whose identity would differ. No lock, row or column is added for
it.

## 6. The missed rule

A due time before the tick a call names is one whose own tick no call named
(the scheduler was down, or skipped it): the trigger's `missed` decides, as on
the Runner's own clock, with no grace, since the call's tick is the clock.

**Lateness is judged against the Runner's clock as well.** A call may name a
tick no more than one interval behind the Runner's clock; an older one is
refused, `422` `invalid_tick`, and records nothing. So a due time more than
one interval before the Runner's clock is always before the tick a call can
name, and is missed. A scheduler down from 10:00 to 11:00 that replays the
15-minute ticks 10:00, 10:15, ... 11:00 at 11:00:05 has the first four
refused; the 11:00 call finds the 10:00 due time missed, and `latest` makes
one occurrence, at 11:00, while `skip` records one skipped. A retry of a
call that failed is taken while its tick is no more than one interval old.

- `skip` records one skipped occurrence, reason `missed`, from the first due
  time missed through the tick called, and the next run is the next tick;
- `latest` admits one occurrence at the latest due tick at or before the tick
  called, reason `latest-after-missed`.

Neither creates a catch-up burst, and paused time is not backfilled. On the
Runner's own clock a `cron` schedule keeps the 30-second grace of every
schedule.

## 7. The store

A store's `schema` row says which Runners may open it. A Runner before this
change does not read a `cron` or a `tick` schedule, so a store that holds one
is raised to schema **7**, which such a Runner refuses.

- On the sqlite and in-memory kinds the schema is raised in the transaction
  that first writes such a trigger, by a job's creation or a trigger's
  configuration, from any earlier schema.
- On the postgresql kind the migration writes the schema
  ([store-postgresql.md](store-postgresql.md), D15): `0005_schedules.sql`
  raises the `schema` row to `7` and changes nothing else. A store without it
  is refused at the start until it is migrated, and a Runner before this
  change refuses a store migrated with it, since its applied set is not that
  Runner's own.

## 8. The cost of one tick

`TestASharedTickAtSeveralHundredJobsReportsItsCost` makes 400 jobs across
four scopes, each with one enabled `tick` trigger due at one tick (written
past the installation's trigger limit, which is 128), and calls the tick
once, then again. On a development machine (Linux under a virtual machine,
the sqlite kind, no race detector), one run measured:

| Call | Time |
|---|---|
| The tick, 400 triggers due: 100 occurrences accepted, 300 skipped `queue-full` | 3.56 s, about 9 ms a trigger |
| The same tick again: 400 triggers read, none due | 22 ms |

This is one machine's figure, not a bound: it varies with the disk, the
store's kind and size, and the load beside it. The test reports it and holds
only the counts. Each trigger due costs one transaction, so the cost grows
with the triggers due at the tick; the triggers read but not due cost a read
each. The postgresql kind was not measured.

## 9. Limits, and not in this change

- **The installation's limits hold.** An installation holds at most 128
  triggers, and at most 100 occurrences await admission: a tick that finds
  more due than the queue has room for records the rest `skipped`, reason
  `queue-full`, as the clock does. Several hundred jobs on one tick need
  those limits raised, which this change does not do.
- **The bounds are the installation's,** not a job's or a scope's.
- A trigger enabled before the operator narrowed the bounds is not paused
  or refused at its next tick.
- No seconds field, no names, no `L`, `W` or `#`, no `@` macros.
- No shared interval on a companion's boot line.
- The `interval`, `daily`, `weekly` and `once` kinds are unchanged, and not
  held to the operator's bounds.
