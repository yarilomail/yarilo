# Sandbox helpers

- `creds.sh`, `secrets.sh`: the stand's test credentials and Secrets.
- `dns-zone.sh`: writes `stand-dns/test.db` from the `dkim` table, one TXT per row with a public key, plus SOA and NS. No A or MX: mail to `.test` domains goes by the relays' transport maps.
- `dns-apply.sh`: installs `stand-dns` (CoreDNS serving `test.` from that file) and adds a `test:53` forward block to the cluster CoreDNS, so rspamd and every pod resolve the DKIM records. It can be run again.

A new or removed `dkim` row: run `dns-zone.sh`, commit `test.db`, run `dns-apply.sh`.

Re-enabling the microk8s `dns` addon rewrites the cluster Corefile; run `dns-apply.sh` again afterwards.
