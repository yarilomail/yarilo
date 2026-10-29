package imap

import (
	"log/slog"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// flagRecent is the session flag of RFC 3501 §2.3.2, gone from IMAP4rev2.
const flagRecent = `\Recent`

// selectRecent takes the folder's \Recent messages at SELECT and only reads
// them at EXAMINE; a virtual folder has none (RFC 3501 §6.3.1, §6.3.2).
func (s *session) selectRecent(f *mailbox.Folder, msgs []*mailbox.MessageMeta, examine bool) {
	s.recent = nil
	s.claimsRecent = false
	if s.isVirtualSelected() {
		return
	}
	if examine {
		s.addRecent(f.FirstRecentUID, f.NextUID)
		return
	}
	s.claimsRecent = true
	s.claimRecent(f, msgs)
}

// claimRecent takes what no session holds yet. The unlocked check keeps a
// SELECT with nothing new from the folder hold and its header write.
func (s *session) claimRecent(f *mailbox.Folder, msgs []*mailbox.MessageMeta) {
	end := f.NextUID
	if n := len(msgs); n > 0 && msgs[n-1].UID >= end {
		end = msgs[n-1].UID + 1
	}
	if f.FirstRecentUID != 0 && f.FirstRecentUID >= end {
		return
	}
	from, to, err := s.folderMailbox().ClaimRecent(f)
	if err != nil {
		slog.Warn("imap: recent claim failed", "user", s.username(), "folder", f.Name, "err", err)
		return
	}
	s.addRecent(from, to)
}

func (s *session) addRecent(from, to uint32) {
	if from == 0 {
		from = 1
	}
	if from < to {
		s.recent.AddRange(imaplib.UID(from), imaplib.UID(to-1))
	}
}

// numRecent counts the messages the session holds as \Recent.
func (s *session) numRecent() uint32 {
	var n uint32
	for _, m := range s.knownMsgs {
		if s.recent.Contains(imaplib.UID(m.uid)) {
			n++
		}
	}
	return n
}

// rev2 reports whether the client enabled IMAP4rev2, which has no \Recent.
func (s *session) rev2() bool {
	return s.imapConn != nil && s.imapConn.EnabledCaps().Has(imaplib.CapIMAP4rev2)
}

// withRecent adds \Recent to a message's FETCH FLAGS for the session holding it.
func (s *session) withRecent(flags []imaplib.Flag, uid uint32) []imaplib.Flag {
	if s.rev2() || !s.recent.Contains(imaplib.UID(uid)) {
		return flags
	}
	return append(flags, flagRecent)
}

// recentMeta is m as SEARCH sees it: with \Recent when the session holds it.
func (s *session) recentMeta(m *mailbox.MessageMeta) *mailbox.MessageMeta {
	if !s.recent.Contains(imaplib.UID(m.UID)) {
		return m
	}
	c := *m
	c.Flags = append(append([]string(nil), m.Flags...), flagRecent)
	return &c
}

// recentInFolder is STATUS RECENT: the messages at or above the folder's mark.
func recentInFolder(f *mailbox.Folder, msgs []*mailbox.MessageMeta) uint32 {
	first := f.FirstRecentUID
	if first == 0 {
		first = 1
	}
	var n uint32
	for _, m := range msgs {
		if m.UID >= first {
			n++
		}
	}
	return n
}

// recentArrived settles \Recent for messages a poll found: a SELECT takes
// them, an EXAMINE reads the folder's mark.
func (s *session) recentArrived(f *mailbox.Folder, arrived []*mailbox.MessageMeta) {
	if s.isVirtualSelected() || len(arrived) == 0 {
		return
	}
	if s.claimsRecent {
		for _, m := range arrived {
			if !s.recent.Contains(imaplib.UID(m.UID)) {
				s.claimRecent(f, arrived)
				return
			}
		}
		return
	}
	first := f.FirstRecentUID
	for _, m := range arrived {
		if first == 0 || m.UID >= first {
			s.addRecent(m.UID, m.UID+1)
		}
	}
}

// refuseRecent answers BAD to a client setting \Recent, which only the server
// sets (RFC 3501 §2.3.2), as the reference does.
func refuseRecent(flags []imaplib.Flag) error {
	for _, f := range flags {
		if strings.EqualFold(string(f), flagRecent) {
			return &imaplib.Error{Type: imaplib.StatusResponseTypeBad, Text: `Invalid system flag \RECENT`}
		}
	}
	return nil
}
