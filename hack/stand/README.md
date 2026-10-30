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
| `YARILO_ARM_CHART` | this checkout | a checkout whose `helm/` and `helm_values/` the arm deploys; checked clean and named in the arm's output as `-- chart:` |

### A release's arm

A release window runs the last release as arm A with its own chart and values,
under this tooling: a worktree at the release tag beside the runner's checkout.

```
git -C ~/stand worktree add ~/stand-v2.4.4 v2.4.4
YARILO_ARM_CHART=~/stand-v2.4.4 bash hack/stand/ab-arm.sh A1 2.4.4 <out-dir>
```

### Overlays

`YARILO_ARM_OVERLAY=<name>` lays `helm_values/values-sandbox-<name>.yaml` over the
stand's values for one arm; `ab-arm.sh` refuses an overlay carrying keys outside
the ones allowed for it. An overlay is never deployed on its own.

- `no-protocol-rule` (#2227): the stand's `components.auth.passdb` list without
  the per-protocol rule, the two `CASE %s` lines, to price the userdb lookup the
  rule adds to every login. It repeats the whole list because helm replaces
  lists, so keep it in step with `values-sandbox.yaml`: the only difference is
  those two lines.

## The acceptance run

```
KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/accept.sh <image-tag> <out-dir>
```

What a rollout after a merge must pass, judged and written as `PASS`/`FAIL`/`SKIP`
lines in `<out-dir>/verdict.txt`; the script exits 1 when any fails. `SKIP` is a
criterion that cannot run yet, with its reason, counted apart. On the
runner, like a window, and recorded in docs-internal `stand-runs.md` and on the
issue it verifies.

| Criterion | How |
|:---|:---|
| smoketest: `SMOKE_CHECKS` passed, 0 failed, and skipped exactly the oauth2 rows named in `SMOKE_OAUTH2_SKIPS`, on mdbox, maildir, sdbox; the verdict reads `56 passed, 0 failed, 3 skipped (oauth2)`, and any other skip fails however the counts add up | `hack/smoketest/run.sh` per type, without the OAuth2 overlay |
| disk = index on maildir and sdbox | file count against `yarctl backend folder info` |
| mail between mailboxes reaches the relay as its sender | from the runner with `curl`: u1→u51, u51→u101, u101→u1 over SMTP 587 and over JMAP (`Email/set` and `EmailSubmission/set` with `"emailId":"#d"` in one request, a Bcc to the third user), each with its own password, then u1→u51 over both logged in as `u1@d00001.test*<submission_relay_master_user>`; every copy must reach INBOX without a Bcc header and is expunged, and relay-mta's `sasl_username` lines since the first send must be exactly the senders, never the master user (#2273). The master password is read from Secret `yarilo-submission-relay` and given to `curl` on stdin, never as an argument; sent messages and replies are in `<out-dir>/cross/` |
| imaptest 0 errors, 0 stalled lines, per type | `hack/imaptest/job.yaml`, one type at a time |
| an aged sdbox and maildir mailbox survive an open | an account with 20+ files modified over a day ago (mtime, as the #2172 sweep judged), counted right before and after an IMAP SELECT |
| a passdb nologin refused as a wrong password | `nologin@d00001.test` (`mailbox.nologin = 1`, set by `hack/db/seed-sandbox.sh`) logs in over IMAP with its password and with a wrong one; both answer the same `NO` (#2229) |
| a per-protocol rule: IMAP refused as a wrong password, POP3 in | `ruleimap@d00001.test` (`mailbox.imap_enabled = 0`; the stand's password query reads it through `CASE %s`) answers IMAP with its password exactly as with a wrong one, and logs in over POP3 (#2227) |
| a per-protocol rule: delivery refused as an unknown recipient | `rulelmtp@d00001.test` (`mailbox.lmtp_enabled = 0`; the stand's user query reads it through `CASE %s`) and a nonexistent address in one LMTP transaction: the login proxy takes both RCPTs, the backend answers both after DATA with the same reply but for the address (`550 5.1.1 … No such user here`), so nothing is stored; the probe's own IMAP login succeeds (#2227) |
| no new vanished-and-present uid | QRESYNC on u53–u70@d00002.test before and after (#2176) |
| no pod restarted or replaced | pod uids and restart counts at the start, once every Deployment has rolled out and no pod is terminating, and at the end; Job pods (smoketest, imaptest) left out |
| no `panic`, `stopped-naming`, `reconcile-unfinished`, `append-failed`, `list-rename`, `reactive-heal`, `message-swept` lines | every container's log since the start |
| an explicit domain rebalance, and back | `yarctl director domains move d00001.test <other backend>`, then a login of u1@d00001.test through the login service must be pinned to the new backend at every director; the same move back restores the stand |
| no director panic | every director's log since the start, previous containers included (#2187) |
| list hold p99 under the 10 s wait | `maildir_lock_hold_seconds` from every backend's IMAP and FTS containers at the start and the end, p50/p99 per site from the delta in `<out-dir>/lock-hold.txt` |

At the first `stopped-naming` line for a folder, the user's UID list, its lock file, the folder's index files and a stat of the named record go to `<out-dir>/evidence/<user>.<folder>/` while the run goes on (#2183).

A command that fails ends the run with `accept: line N: <command> exited <rc>` on stderr, never silently.

Every matched line is kept verbatim in `<out-dir>/lines/<name>.txt`, and every
container's log in `<out-dir>/logs/`: a count alone explains nothing once the
pods are replaced (#2183).

The runner's disk is pruned by the `Prune runner disk` job of `ci-develop.yml` after every develop build: images, containers and build cache older than 48 h go, and the build cache is capped at 10 GB. The same job drops Go build cache entries unused for 48 h (`go/pkg/mod` is left alone). That cache is shared with the seconddns runners on the host, so their first build after a prune is a little longer.

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

## Two folders, one account

```
KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/store-parallel.sh <label> <out-dir>
```

Two imaptest halves, one pod, write flags into `PushA` and `PushB` of one
account at once: the profile that shows a per-account lock serialising writes
two folders kept apart (#714). Run it right after an arm, against the image
that arm deployed, and in the same slot as the other arms of the window.

| knob | default | what it changes |
|:---|:---|:---|
| `YARILO_STORE_TYPES` | `mdbox maildir sdbox` | which storage types run |
| `YARILO_STORE_CLIENTS` | 5 | clients per folder |
| `YARILO_STORE_SECS` | 60 | seconds per type |
| `YARILO_STORE_MSGS` | 100 | messages copied from INBOX into each folder |

Each type prints `store_parallel` (stores per second, from imaptest's totals)
and `store_latency` (count, p50 and p99 bounds of `imap_command_seconds` for
`Store`, and of `yarilo_locks_acquire_wait_seconds` for the `mbox` class, from
the delta across the run). The server's `Store` count must
equal the two halves' total; a gap means the page was read from the wrong pods.

## Push across pods

```
KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/push-latency.sh <label> <out-dir>
```

A JMAP push stream is held open on one backend pod, straight to its JMAP
container over internal mTLS, while mail for the same account is delivered
over LMTP to the pod the director pins it to: the time from each delivery's
`250` to the stream's StateChange naming `Email` is the cross-pod path of push
events. The account is the first candidate the director places, and the stream goes to
a backend pod it is not on.
One run on one image, never alongside a window arm; bash 5, so the runner.

| knob | default | what it changes |
|:---|:---|:---|
| `YARILO_PUSH_TRANSPORT` | `eventsource` | `eventsource`: curl through a port-forward; `websocket`: `yarilo-smoketest -push-listen` run in the JMAP login pod with its internal certificate, enabling push and waiting for the answer to a later request before the first delivery |
| `YARILO_PUSH_PASS` | the sandbox Secret, through `hack/sandbox/creds.sh` | the accounts' password, for the IMAP login that places an account |
| `YARILO_PUSH_POD` | any backend pod the account is not on | the pod holding the stream |
| `YARILO_PUSH_USERS` | `u1`, `u51`, `u101`, `u2`, `u52`, `u102` of the three type domains | candidate accounts, first pinned elsewhere wins; an unplaced one is logged in once over IMAP so the director places it |
| `YARILO_PUSH_COUNT` | 50 | deliveries |
| `YARILO_PUSH_PAUSE` | 2 | seconds between deliveries |
| `YARILO_PUSH_WAIT` | 10 | seconds a delivery's event may take before it counts as lost |

It prints `push_latency` (count, lost, p50, p99 and max in milliseconds). A
lost event fails the run. The stand-runs row takes p99 under 1 s as its bar.

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

A line that cannot be read back to its counter is the same waste one step
later: a window report once called a metric missing that was printed under
another name. What the summary fields are made of:

| summary field | metric |
|:--|:--|
| `logins`, `stalls` | the imaptest log, not a metric |
| `sessions (peak)`, `<ip>=<n>` | the session spread samples, not a metric |
| `walks: full` / `partial` / `untokened` | `imap_maildir_sync_total{result="scanned"}`, `{result="scanned-partial"}`, `{result="scanned-untokened"}` |
| `walks: first-seen` | the same counter with `reason="first-seen"` and `result="scanned"`; 20 accounts log in per run, so anything outside 20-21 is a different start |
| `walks: partial_share` / `cold_share` | partial over all walks; first-seen over full walks |
| `walk cost: full_ms` / `partial_ms` | `imap_maildir_sync_seconds_sum` over `_count`, per result |
| `walk cost: partial_empty` `of` | `maildir_partial_pass_empty_total` over the partial walk count |
| `reconcile: scanned` / `partial` / `skipped` | `imap_maildir_sync_total{result=...}`, the same counter summed without the first-seen split |
| `reconcile: folders_opened` | `quota_folders_opened_total` |
| `listing: list_whole` / `list_tail` | `maildir_uidlist_read_total{mode=...}` |
| `listing: dir_name` / `dir_scan` / `dir_remove` | `maildir_dir_read_total{reason=...}` |
| `listing: miss_none` / `miss_stale` | `maildir_listing_miss_total{reason=...}` |
| `listing: retry_path` / `retry_rename` / `retry_open` | `maildir_listing_retry_total{at=...}` |
| `listing: stat_dir` / `stat_list` | `maildir_cache_stat_total{what=...}` |
| `listing: shut_own` / `shut_expunge` / `shut_other` | `maildir_window_closed_total{by=...}` |
| `listing: stamp_hit` / `stamp_miss` | `maildir_uidlist_stamp_hit_total`, `..._miss_total` |
| `listing: stamp_same` | `fileindex_maildir_stamp_unchanged_total` |
| `guid store: rebuilt` / `stale` / `image_built` | `fileindex_guid_store_rebuilt_total`, `..._stale_total`, `fileindex_guid_image_built_total` |
| `cache: crc_mismatch` | `index_cache_record_crc_mismatch_total` |
| `cache: bodies_opened`, `opens_per_login` | `mailbox_message_opened_total` |
| `cache: journal_write_failed` | `fileindex_journal_write_failed_total` |
| `cache: store_write_failed` | `mailbox_write_failed_total` |

The `*_per_login` figures are the field divided by that arm's login count, which
is the only unit that compares two arms of different lengths.

The CPU profile is taken on every arm; the block profile only under the
profiling overlay, because a pod that accounts for every blocking operation is
not the pod the other arm is.

## The OAuth2 provider

`helm_values/keycloak-sandbox.yaml` runs Keycloak for the OAuth2 rows of #2231,
in its own namespace `keycloak`. It keeps no state: realm `yarilo` is imported
from the file on every start, so a realm change is applied by re-applying and
restarting. Its certificate is a leaf issued in `yarilo-sb` by the stand's
internal CA; only the leaf Secret is copied to `keycloak`, never the CA key:

```
kubectl apply -f hack/internal-ca-sandbox.yaml   # once, in yarilo-sb
kubectl apply -f helm_values/keycloak-sandbox.yaml
kubectl -n yarilo-sb wait certificate/keycloak-tls --for=condition=Ready --timeout=120s
kubectl -n yarilo-sb get secret keycloak-tls -o go-template='{"apiVersion":"v1","kind":"Secret","type":"kubernetes.io/tls","metadata":{"name":"keycloak-tls","namespace":"keycloak"},"data":{"tls.crt":"{{index .data "tls.crt"}}","tls.key":"{{index .data "tls.key"}}","ca.crt":"{{index .data "ca.crt"}}"}}' | kubectl apply -f -
kubectl -n keycloak rollout restart deploy/keycloak   # after a realm change or a renewed leaf
```

Issuer: `https://keycloak.keycloak.svc:8443/realms/yarilo`, HTTPS only. A
renewed leaf is copied again the same way.

The OAuth2 entries live in three overlays, one entry each, so a row of the
#2231 matrix is answered by one mode and no other:

| Overlay | Mode | Matrix rows |
|:---|:---|:---|
| `values-sandbox-oauth2-local.yaml` | `local`: JWKS + the HS256 key | 5, 6, 8–14, 16–19, 21–26, 28; the smoketest oauth2 rows |
| `values-sandbox-oauth2-introspection.yaml` | `introspection` (POST), password grant, forced introspection | 1, 15, 20, 27, 29 |
| `values-sandbox-oauth2-discovery.yaml` | `discovery`, JWKS by default | 7 |

Rows 2–4 (introspection by GET or bearer header, tokeninfo) have no live form:
this Keycloak answers its introspection endpoint by POST only and refuses a
token in the userinfo query, so the `httptest` rows in
`internal/auth/oauth2` are where they are checked.

An overlay is laid on top of `values-sandbox.yaml` only inside an OAuth2
window. The chain stops at the first temporary failure and the entries run
before SQL, so with an overlay every PLAIN login of the stand goes to Keycloak
and Keycloak down stops every login: acceptance runs and A/B arms run without
any of them, and `ab-arm.sh` knows none. A window goes overlay by overlay —
deploy it, run that mode's rows, deploy the next — and ends on the base values
with the same `helm upgrade`. Each step is its own `stand-runs.md` row naming
the overlay and the image:

```
helm upgrade yarilo helm -n yarilo-sb -f helm_values/values-sandbox.yaml \
  -f helm_values/values-sandbox-oauth2-<mode>.yaml --set image.tag=<tag>   # one mode
helm upgrade yarilo helm -n yarilo-sb -f helm_values/values-sandbox.yaml \
  --set image.tag=<tag>                                                    # window ends
```

In an OAuth2 window the smoketest runs only its oauth2 rows, under the local
overlay, once per mailbox type, and all three must pass; that run is its own `stand-runs.md` row, naming
the overlay. The token comes from the password grant of `yarilo-test`, over the
internal CA the Job already mounts:

```
for user in u1@d00001.test u51@d00002.test u101@d00003.test; do
  dir=$(mktemp -d); cp hack/smoketest/run.sh "$dir/"
  awk -v u="$user" '{ gsub(/u1@d00001\.test/, u); print }
    /- -imap-pass=/ { print "        - -only=oauth2"
      print "        - -oauth2-token-url=https://keycloak.keycloak.svc:8443/realms/yarilo/protocol/openid-connect/token"
      print "        - -oauth2-ca=/tls/ca.crt" }' hack/smoketest/job.yaml > "$dir/job.yaml"
  bash "$dir/run.sh" <tag>
done
```

## The policy server

The auth policy rows of #2317 run against wforce, deployed into namespace
`wforce` from the `k8s_wforce` chart; its Secret and yarilo's credentials for
it (`yarilo-auth-policy`: the API header and the hash nonce) are made by
`hack/sandbox/secrets.sh`. wforce tarpits past 3 different failed passwords
per IP+login, refuses past 50 per IP with a message for the client, tarpits
past 20 per login from any IP (k8s_wforce 0.1.2), and logs every field of
every request it gets.

`values-sandbox-wforce.yaml` turns the policy on in full: checked before and
after the exchange, reported after it, not log-only, fail-open. Like the OAuth2
overlays it is laid on only for its own rows, since every login of the stand
then waits on wforce; acceptance runs and A/B arms run without it, and
`ab-arm.sh` knows it not:

```
helm upgrade yarilo helm -n yarilo-sb -f helm_values/values-sandbox.yaml \
  -f helm_values/values-sandbox-wforce.yaml --set image.tag=<tag>   # rows on
KUBECONFIG=~/.kube/ihorru-sbox-nc.yaml bash hack/stand/policy-wforce.sh -v
helm upgrade yarilo helm -n yarilo-sb -f helm_values/values-sandbox.yaml \
  --set image.tag=<tag>                                             # rows off
```

`policy-wforce.sh` forwards wforce's API, takes the passwords through
`hack/sandbox/creds.sh` and runs `go test -tags wforce ./internal/auth/policy/`:
the client's decisions and body fields against wforce, then logins through the
stand's IMAP (`YARILO_IMAP_ADDR`, else a forward to `yarilo-imap-login`), each row reading
back from wforce's log what the server sent and checking what it did with the
answer. Needs go and a route to the stand.
