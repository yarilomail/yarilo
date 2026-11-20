package sortedfile

import "sort"

// KV is one entry a merge brings from the log.
type KV struct{ Key, Val []byte }

// SortKVs puts entries from the log in the file's order; of two with one
// key, the later one stays, as the log's last word on it.
func SortKVs(kvs []KV) []KV {
	sort.SliceStable(kvs, func(i, j int) bool { return Less(kvs[i].Key, kvs[j].Key) })
	out := kvs[:0]
	for i, kv := range kvs {
		if i+1 < len(kvs) && string(kvs[i+1].Key) == string(kv.Key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// MergeSection writes section id as the base's entries merged with updates,
// which SortKVs ordered; an update replaces the base's entry of its key. The
// base is read in order and not held: what a merge keeps is the updates.
func MergeSection(w *Writer, base *Reader, id uint32, updates []KV) error {
	if err := w.Begin(id); err != nil {
		return err
	}
	i := 0
	emitUpTo := func(key []byte) error {
		for ; i < len(updates) && Less(updates[i].Key, key); i++ {
			if err := w.Add(updates[i].Key, updates[i].Val); err != nil {
				return err
			}
		}
		return nil
	}
	if base != nil {
		if err := base.Each(id, func(k, v []byte) error {
			if err := emitUpTo(k); err != nil {
				return err
			}
			if i < len(updates) && string(updates[i].Key) == string(k) {
				i++
				return w.Add(k, updates[i-1].Val)
			}
			return w.Add(k, v)
		}); err != nil {
			return err
		}
	}
	for ; i < len(updates); i++ {
		if err := w.Add(updates[i].Key, updates[i].Val); err != nil {
			return err
		}
	}
	return w.End()
}
