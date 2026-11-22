// Package flatcurve is the Xapian-backed FTS engine, on-disk compatible
// with the flatcurve index format: per-mailbox "fts-flatcurve" directory
// of Xapian shards (current.### write shard, index.### sealed shards), term
// prefixes A (all headers), H<NAME> (indexed header), B (header existence).
// A document is recognised by its terms (the message GUID and its folder),
// not by its docid: every shard numbers from one, and a merge renumbers.
// See https://doc.yarilomail.org/latest/features/fts.html.
//
// Requires cgo + libxapian; compiled only with the "flatcurve" build tag
// (yarilo-fts binary). Raw Xapian access goes through
// github.com/0kaba0hub/go-xapian.
package flatcurve
