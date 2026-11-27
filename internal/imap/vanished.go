package imap

import (
	"cmp"
	"slices"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// vanishedSince is VANISHED (EARLIER) within the client's uids (RFC 7162 §3.2.5):
// past the history, every uid in range the folder lacks, as the reference answers.
func vanishedSince(box mailbox.Box, folderID, modseq uint64, within imaplib.UIDSet, nextUID uint32, held []*mailbox.MessageMeta) (imaplib.UIDSet, error) {
	ranges := clipUIDs(within, nextUID)
	var out imaplib.UIDSet
	if len(ranges) == 0 {
		return out, nil
	}
	uids, reaches, err := box.Vanished(folderID, modseq)
	if err != nil {
		return nil, err
	}
	if reaches {
		for _, uid := range uids {
			if inRanges(ranges, uid) {
				out.AddNum(imaplib.UID(uid))
			}
		}
		return out, nil
	}
	return gapsIn(ranges, held), nil
}

// gapsIn is the uids in ranges that held does not carry, in one pass over both
// in uid order: the cost is the folder's messages and the ranges, never the
// width of the uid space a long-lived folder has used.
func gapsIn(ranges [][2]uint32, held []*mailbox.MessageMeta) imaplib.UIDSet {
	uids := make([]uint32, len(held))
	for i, m := range held {
		uids[i] = m.UID
	}
	if !slices.IsSorted(uids) {
		slices.Sort(uids)
	}
	var out imaplib.UIDSet
	j := 0
	for _, r := range ranges {
		next := r[0]
		for j < len(uids) && uids[j] < r[0] {
			j++
		}
		for ; j < len(uids) && uids[j] <= r[1]; j++ {
			if uids[j] > next {
				out.AddRange(imaplib.UID(next), imaplib.UID(uids[j]-1))
			}
			next = uids[j] + 1
		}
		if next <= r[1] && next != 0 {
			out.AddRange(imaplib.UID(next), imaplib.UID(r[1]))
		}
	}
	return out
}

// clipUIDs is set as closed ranges in [1, nextUID-1]; an empty set is all of
// it, as a QRESYNC without known uids asks, and "*" is the last uid.
func clipUIDs(set imaplib.UIDSet, nextUID uint32) [][2]uint32 {
	if nextUID <= 1 {
		return nil
	}
	last := nextUID - 1
	if len(set) == 0 {
		return [][2]uint32{{1, last}}
	}
	var out [][2]uint32
	for _, r := range set {
		start, stop := uint32(r.Start), uint32(r.Stop)
		if start == 0 {
			start = last
		}
		if stop == 0 {
			stop = last
		}
		if start > stop {
			start, stop = stop, start
		}
		if start > last {
			continue
		}
		out = append(out, [2]uint32{start, min(stop, last)})
	}
	// In order and apart, so one pass over the folder's uids serves them all.
	slices.SortFunc(out, func(a, b [2]uint32) int { return cmp.Compare(a[0], b[0]) })
	merged := out[:0]
	for _, r := range out {
		if n := len(merged); n > 0 && r[0] <= merged[n-1][1]+1 {
			merged[n-1][1] = max(merged[n-1][1], r[1])
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

func inRanges(ranges [][2]uint32, uid uint32) bool {
	for _, r := range ranges {
		if uid >= r[0] && uid <= r[1] {
			return true
		}
	}
	return false
}
