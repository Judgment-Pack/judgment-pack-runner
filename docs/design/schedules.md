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
never ticks it, and does not read it (section 8): only a call for its
interval ticks it (section 5).

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

The configuration's optional `limits` member is one object of:

- `triggers`: the most triggers the installation holds, from 1 to 1000,
  default 128;
- `pendingOccurrences`: the most occurrences pending (accepted, preparing or
  waiting) at once, from 1 to 2000, default 100;
- `runQueue`: the most runs queued or running at once, the run queue, from 1
  to 2000, default 100.

Each a whole number, at most once, spelled exactly, none null, and nothing
else; a refusal stops the start. Anything but an object, a member given
twice or a member Runner does not know is refused first; then the three are
checked in this order, `triggers`, `pendingOccurrences`, `runQueue`, so that
of several refused the first is answered every time. Without the member, or without one of the
three, the default holds, as it does on a companion's boot line, which takes
no `limits`. `GET /v1/status` states the limits in force, under `limits`.
Section 9 says what they hold and what Runners that share a store need;
section 8 says how the maximums were chosen.

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
changed or enabled again, and each start reports it (section 9).

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

**What it does.** It reads the list of every enabled trigger, then takes each
in turn under the lock the Runner's own automation takes, held for that one
trigger (section 8): it reads the trigger again, passes it by if it is gone,
paused or not bound to the interval, and otherwise ticks it at the tick, as
the clock ticks a schedule (`scheduleDue`, `triggers_dispatch.go`): a trigger
whose `nextAt` is at or
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
it. Two calls for one tick at the same time may share the triggers between
them: each trigger due is ticked by the call that takes it first, and the
other finds its `nextAt` after the tick, so each call answers the
occurrences it recorded, and the two answers add up to the triggers due.
Two calls at the same time for different ticks of one interval, a late call
for one tick beside the call for the next, may likewise interleave trigger
by trigger: a trigger the later call reaches first has its earlier due time
taken by its missed rule (section 6), and the earlier call then finds its
`nextAt` after that call's tick and records nothing for it, while a trigger
the earlier call reaches first admits the earlier due time on time, and the
later call its own. Either way no due time of a trigger is admitted twice
and no trigger makes a catch-up burst; which call records a trigger's
occurrence, and whether its earlier due time is on time or missed, depends
on the order the two calls reach it. A
Runner found unhealthy part way through a call answers `503`
`runner_unavailable` for it; the triggers ticked before stay ticked, and a
retry of the call after the restart ticks the rest, while the tick is no
more than one interval old (section 6).

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

Measured on one machine: a development machine (Linux under a virtual
machine, no race detector), the postgresql kind on PostgreSQL 17 in a
container on the same machine. N jobs, across several scopes, each with one
enabled `tick` trigger due at one tick, on a Runner whose limits hold them
all; the tick called once, then again. Every trigger's occurrence was
accepted, none skipped `queue-full`.

| Kind | N | The tick | Each trigger due | The same tick again | One pass of the Runner's own clock |
|---|---|---|---|---|---|
| sqlite | 128 | 1.21 s | 9.5 ms | 11 ms | 11 ms |
| sqlite | 1000 | 7.4 s | 7.4 ms | 50 ms | 32 ms |
| sqlite | 2000 | 13.5 s | 6.8 ms | 105 ms | 88 ms |
| postgresql | 128 | 1.28 s | 10 ms | 57 ms | 77 ms |
| postgresql | 1000 | 7.8 s | 7.8 ms | 331 ms | 315 ms |
| postgresql | 2000 | 17.7 s | 8.9 ms | 858 ms | 757 ms |

- **Each trigger due is one transaction**: on the postgresql kind, 14
  statements and one commit (counted by `pg_stat_statements`), at every N. The
  triggers read but not due cost a read each ("the same tick again").
- **The lock is held for one trigger at a time**, about the time of one
  trigger due, 7 to 10 ms. A probe that took the lock every millisecond
  during the tick waited at most 28 to 127 ms on the postgresql kind and 30 to
  40 ms on the sqlite kind (once 1.1 s, at 1000 on sqlite, a stall of the
  disk). Held for the whole fan-out, as before this change, the lock would
  have been held for the whole tick, 1.2 to 18 s.
- **The Runner's own clock read every enabled trigger once a second**, the
  shared-interval ones included, under the lock ("one pass" above), until
  #289 (below). With the
  clock running beside the tick, the tick took 7.6 s at 1000 and 16.3 s at
  2000 on sqlite, and 17.1 s at 1000 and 79 s at 2000 on postgresql, where
  the clock's passes held the lock for about 350 ms of every second at 1000
  and 710 ms at 2000; the clock waited at most 25 ms for the lock.

**The own clock's read (#289).** The clock's read leaves out every schedule
bound to a shared interval, in its statement on both kinds
(`enabledTriggers(ownClock)`: not a `schedule` trigger whose schedule's kind
is `tick`), so that a pass reads, and holds the lock for, only the triggers
the clock may tick. A trigger changed to a shared interval after the list was
read is still passed by when it is read (`scheduleTick`). Measured again on
the postgresql kind, on the same machine: N enabled shared-interval triggers
across four scopes, each due at one tick; one pass of the clock alone (the
median of seven), then the tick with the clock running beside it as its loop
runs it, a pass and then the next whole second. Before is the Runner of
#288, after is this change.

| N | One pass of the clock, before | After | The tick with the clock beside it, before | After |
|---|---|---|---|---|
| 1000 | 390 ms | 19 ms | 17.8 s | 12.0 s |
| 2000 | 673 ms | 27 to 30 ms | 2 min 45 s | 22 to 36 s (two runs) |

During the tick the clock's passes still took 160 to 220 ms on average:
each dispatches up to ten of the occurrences the tick has admitted into runs
(section 9), under the lock. The tick now shares the lock with that
dispatch, not with the read.

These are one machine's figures, not bounds: they vary with the disk, the
network to the database, the store's size, and the load beside it. The tests
report their own figure and hold only the counts.

**The maximums.** `triggers` is at most 1000. Before #289, at 2000 on the
postgresql kind the Runner's own clock spent about three quarters of every
second reading the triggers, and a tick with the clock running took longer
than one minute, the shortest shared interval; at 1000 the tick took under
20 s on both kinds and the clock's pass about a third of a second. Since
#289 the figures would allow 2000 triggers bound to shared intervals: the
clock's pass about 30 ms, and a tick with the clock beside it under one
minute. The maximum stays, because the limit counts every trigger and the
clock still reads, once a second, each enabled trigger not bound to a
shared interval (another schedule, a file, an event or a cloud trigger), at
the cost the first table shows: 2000 of those on the postgresql kind would
take about three quarters of every second again. A larger maximum waits for
a read that costs only the triggers due, by their next time, or for a limit
of its own for shared-interval triggers. `pendingOccurrences` is at
most 2000: the tick's check of the queue is one count in each trigger's
transaction (a row on the postgresql kind), and the ticks at 2000 cost no
more per trigger than at 128. A larger queue is a longer wait for its last
occurrence, which the run queue (section 9) takes in turn.

**The run queue (#289).** One shared tick admitted 300 occurrences, one for
each of 300 jobs, and the Runner's clock dispatched them into runs, on one
machine (indicative figures):

| Kind | Each run | `runQueue` | Every occurrence a run | Every run ended | Most runs queued or running |
|---|---|---|---|---|---|
| sqlite | at once | 100 | 29.3 s | 29.7 s | 14 |
| sqlite | at once | 300 | 29.4 s | 29.6 s | 35 |
| postgresql | at once | 100 | 29.3 s | 29.5 s | 7 |
| postgresql | at once | 300 | 29.5 s | 29.7 s | 7 |
| sqlite | 0.2 s | 100 | 51 s | 75.8 s | 100 |
| sqlite | 0.2 s | 300 | 29.2 s | 78.3 s | 192 |
| postgresql | 0.2 s | 100 | 51.2 s | 76.6 s | 100 |
| postgresql | 0.2 s | 300 | 29.4 s | 77.1 s | 187 |

The clock dispatches up to ten occurrences a pass, one pass a second, so
300 occurrences are runs after about 30 s on both kinds while the run queue
has room. With runs that end at once it never filled. With runs of 0.2 s
each, which the one worker ends in turn, the default queue filled and the
last occurrences waited for runs to end; with `runQueue` at 300 they were
runs after 30 s, and every run ended no sooner. A larger run queue moves
the wait from the occurrences pending to the runs queued; it does not end
the work sooner. `runQueue` is at most 2000, as `pendingOccurrences` is:
every occurrence pending can be a run queued.

**One call holds the automation lock for one trigger at a time.** The tick
takes the lock the Runner's own automation takes for each trigger, reads the
trigger again under it, ticks it, and lets it go before the next: the
Runner's own clock, the trigger routes and another tick call wait at most
one trigger's tick, not the fan-out. Each hold is one transaction and, for a
`constant` or `file` input, the read and check of that input under the input
root (a `mapped-sources` input is not read at the tick: its occurrence is
recorded pending preparation). So the 30-second grace of the Runner's own
clock is not consumed by a long fan-out, at any N. Releasing the lock between
triggers is safe:

- *Rows written at the version read (D17).* Each trigger is read again under
  the lock and written in one transaction at the row version read
  ([store-postgresql.md](store-postgresql.md), D17); on the sqlite kind its
  lock and the automation lock keep other writers out of that transaction. A
  request served between two triggers (a pause, a change, a disable) is seen
  by the fan-out when it reaches that trigger, as a tick of that one trigger
  sees it; a write by another Runner meets the version moved on, as before.
- *Claims and leases (D18).* The tick claims nothing and holds no lease: the
  occurrences it admits are recorded unclaimed, and preparing or running them
  is claimed later, by the dispatcher, under its own lease.
- *The queue.* The queue's count is read in each trigger's transaction, so an
  occurrence admitted between two triggers is counted, and the limit holds.
- *What the call no longer has* is one instant over its fan-out: a trigger
  paused between two triggers of the call is passed by, and two calls at the
  same time share the triggers (section 5). Two calls at the same time for
  different ticks, a late replay of one tick beside the next, may mix trigger
  by trigger: a trigger the later call reaches first has the earlier due
  time taken by its missed rule, and the earlier call records nothing for
  it, while the triggers the earlier call reaches first admit it on time
  (section 5). No due time is admitted twice. The list of triggers is read
  once, at the start: a trigger enabled during the call is next due after the
  tick (section 5) and is not missed.

## 9. Limits, and not in this change

- **The installation's limits are the operator's** (section 4). A companion
  keeps the defaults, 128 triggers, 100 pending occurrences and 100 runs
  queued or running. The trigger
  past the limit, configured or made with its job, is refused, `422`
  `trigger_limit`, stating the limit ("This local runner supports up to 1000
  triggers."). A tick that finds more due than the queue has room for records
  the rest `skipped`, reason `queue-full`, as the clock does; an event or a
  record delivered at the full queue is refused, `429` `queue_full`, its
  message stating the limit. The run queue, the runs queued or running, holds
  a run's submission and its retry, refused `429` `queue_full` at the limit,
  and an occurrence's dispatch, which waits at the full queue until a run
  ends, as before; only the number is the operator's (#289).
- **Runners that share a store are configured with the same limits.** Each
  Runner holds the store to its own: on both kinds the Runner reads the count
  against its configured limit, and on the postgresql kind the `triggers` row
  of `counts` is raised only while below the limit of the Runner that raises
  it ([store-postgresql.md](store-postgresql.md), D9). Runners configured
  differently each refuse at their own number, and nothing breaks: the one
  with the lower limit refuses a new trigger, or admits no occurrence, once
  the store holds its number, and goes on ticking, changing and running those
  the store holds; the one with the higher goes on to its own. No store row
  records the limits, and a Runner restarted with other limits holds the new
  ones from its start, the triggers already held past a lowered limit kept.
- **The bounds are the installation's,** not a job's or a scope's.
- **Every Runner on one store must name the same shared intervals and
  bounds.** The settings are the configuration's, not the store's: a trigger
  enabled under one Runner's settings stays enabled under another's, and
  under the next start's. A trigger bound to a shared interval a Runner does
  not name is never ticked by that Runner, whose tick call for the interval is
  refused (`422` `unknown_shared_interval`); one whose cron expression is
  outside a Runner's bounds goes on being ticked on its schedule until it is
  changed or enabled again, which the bounds in force then hold. Neither is
  paused, changed or refused at its tick. Each start holds every enabled
  trigger, of every scope, to its settings, as enabling it does, and reports
  each one refused (#286):
  - in the [journal](activity-journal.md), one `trigger.outside-settings`
    entry for each, by the Runner, naming the trigger at its revision, its
    job and release, with the refusal's `code` and the scope identifier as
    the entry's own member, never in its text;
  - in `GET /v1/status`, under `triggersOutsideSettings`: when the start
    checked, and under `unknownSharedInterval` and `outsideBounds` the count
    and the trigger ids, as the start found them.

  Each start under the same settings reports the same triggers again, one
  entry each. A start under manual admission checks nothing. The health
  route, which answers without a credential in service mode, says nothing of
  them ([service-mode.md](service-mode.md), section 6).
- No seconds field, no names, no `L`, `W` or `#`, no `@` macros.
- No shared interval on a companion's boot line.
- The `interval`, `daily`, `weekly` and `once` kinds are unchanged, and not
  held to the operator's bounds.
