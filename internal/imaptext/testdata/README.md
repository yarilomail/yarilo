# Reference envelope and body-structure fixtures

`crafted.eml` is one message written to be awkward in every way the two
encodings differ: a Q-encoded subject carrying a quote and a backslash, a
B-encoded display name, a quoted local part, an empty address group, a folded
`References`, and a `multipart/alternative` with no children.

`reference-envelope.txt` holds what a reference implementation cached for it,
pasted from the run described below. `reference-bodystructure.txt` holds the
same value from that run; it is byte-identical to what this package writes,
which is what the run established.

## How they were made

Against a reference install, version 2.4.5, mdbox, on the sandbox (`ns
dovecot`), no daemon configuration beyond the storage driver and a passwd-file
user:

```
doveadm save -u u1@d00001.test -m INBOX < crafted.eml
# three times, so the fields are cached rather than computed per fetch
doveadm fetch -u u1@d00001.test \
  "imap.envelope imap.bodystructure size.physical size.virtual date.sent date.received hdr.references guid" \
  mailbox INBOX
```

The two files are the `imap.envelope` and `imap.bodystructure` values of the
last fetch, byte for byte, with the trailing newline `doveadm` adds removed.

A literal in the envelope is written as the reference writes it: `{46}` then
CRLF then the bytes.
