# 2026-08-24 Goal scheduler dsh alignment

Status: implemented and verified.

## Scope

Goal scheduler is now an enabled capability when schedule.enabled is true.
It is session-local and driven by the current session event log.

## dsh-aligned behavior

- after_seconds, absolute at, and fixed-rate every_seconds schedules.
- Fixed-rate schedules require at least 300 seconds.
- schedule/change facts are folded to restore active schedules after restart
  or session resume.
- One-shot reminders are delivered in due-time order, one at a time.
- Overdue fixed-rate schedules are delivered as one latest-occurrence batch;
  missed occurrences are not replayed.
- The scheduler derives a dynamic next wake time and wakes early when a schedule
  is created or deleted.
- Reminder text is framed as untrusted reminder content before the normal
  runTurn and Goal idle continuation.

## Go integration

The composition root appends schedule changes to the active session log,
restores them on new/resumed sessions, and runs a process-level timer. A mutex
prevents a scheduled continuation from overlapping another scheduled
continuation. Dispatch is appended only after the reminder turn succeeds, so a
failed turn leaves the due record retryable.

## Explicit differences

- The scheduler currently serves the live session only; there is no
  multi-session scheduler service.
- Go performs the follow-up turn synchronously after admission. As in dsh,
  a crash in the narrow interval before dispatch can cause a duplicate
  reminder after recovery.
- The event log is the durable source, but there is no separate dsh-style
  persistence barrier or scheduler database.

## Verification

go test ./..., go vet ./..., go build ./..., gofmt, and git diff --check pass.
