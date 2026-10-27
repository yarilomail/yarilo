package lmtp

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailops"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// deliverOne saves one message through the mutation layer.
func deliverOne(ops *mailops.Ops, box mailbox.Box, folder string, r io.ReadSeeker, size int64, username, from string, flags []string) (uint32, mailbox.Folder, [16]byte, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, mailbox.Folder{}, [16]byte{}, fmt.Errorf("lmtp: seek: %w", err)
	}
	// Sieve names keywords as freely as system flags; the record keeps the two
	// apart (#1605).
	sysFlags, kws := mailbox.SplitStoredFlags(flags)
	saved, err := ops.Save(box, folder, nil, mailops.SaveRequest{Body: r, Size: size, Flags: sysFlags, Keywords: kws})
	if err != nil {
		var f mailbox.Folder
		if saved.Folder != nil {
			f = *saved.Folder
		}
		return 0, f, [16]byte{}, fmt.Errorf("lmtp: %w", err)
	}
	slog.Info("lmtp: delivered", "from", from, "to", username, "folder", folder,
		"uid", saved.Meta.UID, "file", saved.Filename, "size", size)
	return saved.Meta.UID, *saved.Folder, saved.Meta.GUID, nil
}

// stripDetail removes the +detail part from an address: user+tag@domain → user@domain.
func stripDetail(addr string) string {
	addr = strings.TrimSpace(strings.Trim(addr, "<>"))
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return addr
	}
	local, domain := addr[:at], addr[at+1:]
	if plus := strings.Index(local, "+"); plus >= 0 {
		local = local[:plus]
	}
	return local + "@" + domain
}

// resolveMailbox maps RCPT TO address to (username, folder).
// user+folder@domain → username=user@domain, folder=folder; otherwise folder=INBOX.
func resolveMailbox(rcpt string) (username, folder string, err error) {
	rcpt = strings.TrimSpace(strings.Trim(rcpt, "<>"))
	local, domain, hasDomain := rcpt, "", false
	if at := strings.LastIndex(rcpt, "@"); at >= 0 {
		local, domain, hasDomain = rcpt[:at], rcpt[at+1:], true
	}

	folder = "INBOX"
	if plus := strings.Index(local, "+"); plus >= 0 {
		folder = local[plus+1:]
		local = local[:plus]
	}
	if local == "" {
		return "", "", fmt.Errorf("no user in rcpt %q", rcpt)
	}
	// A recipient without a domain (<Postmaster>) is looked up as it is.
	if !hasDomain {
		return local, folder, nil
	}
	return local + "@" + domain, folder, nil
}

// unnamedHost is what a header says when the installation has no name.
//
// It exists only for a hostname explicitly configured as empty: the default is
// os.Hostname(), so reaching this means an operator asked for it. Not a
// sensible name and not meant to be -- it is visible enough in a Received
// header and a Message-ID that the missing setting gets found (#1506).
const unnamedHost = "yarilo"

// buildReceivedHeader names the host that accepted the message.
func buildReceivedHeader(from, host string) string {
	if host == "" {
		host = unnamedHost
	}
	return fmt.Sprintf("Received: from %s by %s with LMTP; %s\r\n",
		from, host, time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 +0000"))
}

// hasMessageID reports whether the message already carries a Message-ID.
//
// The header section only, up to the blank line: a body can contain anything,
// including a quoted copy of another message's headers, and treating that as
// this message's identity would leave the real one missing on exactly the mail
// most likely to be a reply.
//
// Field names are case-insensitive (RFC 5322 §1.2.2), and only a line that
// starts at column zero begins a field -- a leading space or tab is the
// continuation of the one before it.
func hasMessageID(data []byte) bool {
	const name = "message-id:"
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		line := data
		if end >= 0 {
			line = data[:end]
			data = data[end+1:]
		} else {
			data = nil
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			return false // end of the header section
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if len(line) >= len(name) && strings.EqualFold(string(line[:len(name)]), name) {
			return true
		}
	}
	return false
}

// buildMessageID makes an identifier for a message that arrived without one.
//
// 128 bits of randomness, so it is unique among all messages rather than among
// the messages of one host or one run -- which is what RFC 5322 §3.6.4 asks
// for, and what a counter or a hash of the recipient would not give.
//
// The domain part is the hostname this LMTP server announces itself with. It is
// not read from the Received header, which carries a fixed literal rather than
// a configured name.
func buildMessageID(host string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform we run on, and a delivery
		// is not the place to decide what to do if it did.
		panic("lmtp: crypto/rand: " + err.Error())
	}
	if host == "" {
		host = unnamedHost
	}
	return fmt.Sprintf("Message-ID: <%x@%s>\r\n", b, host)
}
