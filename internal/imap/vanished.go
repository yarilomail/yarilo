package imap

import (
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
	present := make(map[uint32]struct{}, len(held))
	for _, m := range held {
		present[m.UID] = struct{}{}
	}
	for _, r := range ranges {
		var runStart uint32
		for uid := r[0]; ; uid++ {
			_, here := present[uid]
			switch {
			case !here && runStart == 0:
				runStart = uid
			case here && runStart != 0:
				out.AddRange(imaplib.UID(runStart), imaplib.UID(uid-1))
				runStart = 0
			}
			if uid == r[1] {
				break
			}
		}
		if runStart != 0 {
			out.AddRange(imaplib.UID(runStart), imaplib.UID(r[1]))
		}
	}
	return out, nil
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
	return out
}

func inRanges(ranges [][2]uint32, uid uint32) bool {
	for _, r := range ranges {
		if uid >= r[0] && uid <= r[1] {
			return true
		}
	}
	return false
}
