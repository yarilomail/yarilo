# Stand measurement tooling

A window is a set of arms run back to back in one slot. `ab-arm.sh` is one arm:
it deploys a tag, restores the same start, runs the three storage types under
imaptest, and records what each run cost.

Windows run on the runner (`sb-run-01`), from a clean checkout, with
`YARILO_ARM_COMMIT` pinned to the commit the tooling was read at. A window that
runs from a laptop measures the same cluster with different tooling, and the
checkout guard then guards nothing.

## The arm

```
KUBECONFIG=~/.kube/sbox.yaml \
  bash hack/stand/ab-arm.sh <arm-label> <image-tag> <out-dir>
```

| knob | default | what it changes |
|:---|:---|:---|
| `YARILO_ARM_FILL` | 200 | messages per account before the runs; 0 measures the login path on empty mailboxes |
| `YARILO_ARM_REPEATS` | 1 | runs per type; each run gets its own name, counters, log and profile |
| `YARILO_ARM_KEEP_STORE` | 0 | take the previous arm's mailboxes instead of wiping and filling |
| `YARILO_ARM_TYPES` | `mdbox maildir sdbox` | which storage types the arm wipes, fills and runs |
| `YARILO_ARM_OVERLAY` | — | a second values file from `helm_values/values-sandbox-<name>.yaml` |
| `YARILO_ARM_BLOCKPROFILE` | 0 | the profiling overlay; this makes the arm a latency arm, not a throughput one |

## The quick arm

A question about one storage type does not need the other two: a maildir
counter reads zero under mdbox and sdbox, so two thirds of such an arm is paid
for and never read. `YARILO_ARM_TYPES=maildir` with a smaller
`YARILO_ARM_FILL` is the quick arm, and it answers **per-login ratios inside
one type**: stats per login, listings per login, misses per login.

It is **not** a throughput arm and **not** a release check, and its numbers do
not compare with a full arm's: a different fill is a different start. Compare a
quick arm only with another quick arm of the same fill, in the same slot.

Both knobs are printed beside the tag, so a window says what kind of arm it
was. The first-seen count stays the control: 20 accounts log in per run, so a
`first-seen` outside 20-21 is a different start rather than a different image,
and the arm marks it as `first_seen_off=1`.

## The same start

Mailbox fill moves throughput further than most things under test, so every arm
starts from the same place: the accounts are emptied, seeded and filled again,
and the start is printed as a number rather than as a step that ran.

## The carried store, and what it is not

`YARILO_ARM_KEEP_STORE=1` answers one question: **what a first listing over
state an older version wrote costs.** It is read as run 1 against run 2 of the
same arm, with `YARILO_ARM_REPEATS=2` — one image, one store, one slot, and only
the cache state differing between the two runs.

It is **not** a throughput comparison with the arm before it. Every arm's own
runs deliver mail, so a carried arm always measures a bigger store: in `win407`
the three arms started from 39874, 45449 and 47521 messages, and reading the
login drop between them as an image difference was wrong -- most of it was size.
A question about two images needs two arms that both wipe and fill.

## Reading a window

Each run prints its own lines: logins and stalls, session spread, what drove the
walks, what each kind of walk cost, the reconcile per login, and the cache
counters. A counter that is collected and never printed is a number nobody
reads, so every metric the arm scrapes has a line.

The CPU profile is taken on every arm; the block profile only under the
profiling overlay, because a pod that accounts for every blocking operation is
not the pod the other arm is.

## Keeping the runner's disk

`runner-prune.service` and `runner-prune.timer` are the single source of truth
for what the CI runner deletes. Prune by age, never by count: the recent tags
are the ones a rollback reaches for.

Install once on the runner host, and again after it is rebuilt:

```sh
sudo cp hack/stand/runner-prune.{service,timer} /etc/systemd/system/
sudo systemctl enable --now runner-prune.timer
systemctl list-timers runner-prune.timer
```

A change made on the host and not in these files does not count: the next
reinstall drops it. The window that made this necessary ran the disk to 85%
(32.7 GB of images, 27.7 GB of build cache, no live container).
