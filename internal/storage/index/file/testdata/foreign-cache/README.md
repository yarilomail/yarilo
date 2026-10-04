# A folder a reference install wrote

These are one mailbox's files from a reference install (2.4.5, mdbox) on the
sandbox, copied off its volume unchanged. They are here so the adoption of a
foreign cache rests on bytes another implementation produced.

| file | what it is |
|---|---|
| `dovecot.index.log` | the folder's index; there is no `dovecot.index` beside it, because the base lives in the list index on that deployment |
| `dovecot.index.cache` | the folder's cache: `flags`, `date.sent`, `date.received`, `size.physical`, `imap.bodystructure`, `guid`, `mime.parts` and ten `hdr.*` fields — and **no** `imap.envelope`, which that server builds from the headers on demand |
| `dovecot.map.index`, `dovecot.map.index.log` | the store's map |
| `m.1` | the storage file the map addresses |

## How they were made

```
doveadm mailbox create -u u1@d00001.test Fixture
doveadm save -u u1@d00001.test -m Fixture < internal/imaptext/testdata/crafted.eml
doveadm fetch -u u1@d00001.test "imap.envelope imap.bodystructure size.physical size.virtual guid" mailbox Fixture   # three times
doveadm index -u u1@d00001.test Fixture
```

then the five files above were copied from the install's volume.

The message is `internal/imaptext/testdata/crafted.eml`, and the envelope and
body structure that install cached for it are the fixtures beside it.
