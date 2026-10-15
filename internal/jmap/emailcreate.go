package jmap

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/quotedprintable"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// createPart is an EmailBodyPart as a client sends it to create an Email.
type createPart struct {
	PartID      *string      `json:"partId"`
	BlobID      *string      `json:"blobId"`
	Type        string       `json:"type"`
	Charset     *string      `json:"charset"`
	Name        *string      `json:"name"`
	Disposition *string      `json:"disposition"`
	CID         *string      `json:"cid"`
	Language    []string     `json:"language"`
	Location    *string      `json:"location"`
	SubParts    []createPart `json:"subParts"`
}

type createEmail struct {
	MailboxIDs    map[string]bool         `json:"mailboxIds"`
	Keywords      map[string]bool         `json:"keywords"`
	ReceivedAt    *time.Time              `json:"receivedAt"`
	From          []jmapcore.EmailAddress `json:"from"`
	To            []jmapcore.EmailAddress `json:"to"`
	Cc            []jmapcore.EmailAddress `json:"cc"`
	Bcc           []jmapcore.EmailAddress `json:"bcc"`
	ReplyTo       []jmapcore.EmailAddress `json:"replyTo"`
	Sender        []jmapcore.EmailAddress `json:"sender"`
	Subject       *string                 `json:"subject"`
	SentAt        *time.Time              `json:"sentAt"`
	MessageID     []string                `json:"messageId"`
	InReplyTo     []string                `json:"inReplyTo"`
	References    []string                `json:"references"`
	BodyStructure *createPart             `json:"bodyStructure"`
	TextBody      []createPart            `json:"textBody"`
	HTMLBody      []createPart            `json:"htmlBody"`
	Attachments   []createPart            `json:"attachments"`
	BodyValues    map[string]struct {
		Value string `json:"value"`
	} `json:"bodyValues"`
}

// createProps are the Email properties a create may set (RFC 8621 §4.6); the
// server-set ones and anything unknown are refused by name.
var createProps = map[string]bool{
	"mailboxIds": true, "keywords": true, "receivedAt": true,
	"from": true, "to": true, "cc": true, "bcc": true, "replyTo": true, "sender": true,
	"subject": true, "sentAt": true, "messageId": true, "inReplyTo": true, "references": true,
	"bodyStructure": true, "textBody": true, "htmlBody": true, "attachments": true, "bodyValues": true,
}

// mimeNode is one part of the message being built.
type mimeNode struct {
	header message.Header
	body   []byte
	kids   []*mimeNode
}

// emailCreate builds one Email from JSON (RFC 8621 §4.6) and stores it through
// the mutation layer, as Email/import stores a blob.
func (s *Server) emailCreate(ctx context.Context, h *userHandle, raw json.RawMessage) (map[string]any, *jmapcore.SetError) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, invalidProp("", err.Error())
	}
	var bad []string
	for k := range fields {
		if !createProps[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Properties: bad}
	}
	var e createEmail
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, invalidProp("", err.Error())
	}
	targets, serr := h.importTargets(e.MailboxIDs)
	if serr != nil {
		return nil, serr
	}
	root, serr := s.bodyTree(ctx, h, &e)
	if serr != nil {
		return nil, serr
	}
	msg, err := writeEmail(&e, root)
	if err != nil {
		return nil, &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Description: err.Error()}
	}
	keywords := map[string]bool{}
	for k, v := range e.Keywords {
		if v {
			keywords[strings.ToLower(k)] = true
		}
	}
	flags, custom := splitKeywords(keywords)
	received := time.Now()
	if e.ReceivedAt != nil {
		received = *e.ReceivedAt
	}
	return h.storeEmail(msg, targets, flags, custom, received)
}

// bodyTree turns bodyStructure, or textBody/htmlBody/attachments, into the
// tree of parts, as the reference arranges them (§37).
func (s *Server) bodyTree(ctx context.Context, h *userHandle, e *createEmail) (*mimeNode, *jmapcore.SetError) {
	simple := e.TextBody != nil || e.HTMLBody != nil || e.Attachments != nil
	if e.BodyStructure != nil {
		if simple {
			return nil, invalidProp("bodyStructure", "bodyStructure excludes textBody, htmlBody and attachments")
		}
		return s.partNode(ctx, h, e, e.BodyStructure, "bodyStructure")
	}
	if len(e.TextBody) > 1 || len(e.HTMLBody) > 1 {
		return nil, invalidProp("textBody", "at most one textBody and one htmlBody part")
	}
	var text, html *mimeNode
	if len(e.TextBody) == 1 {
		if t := e.TextBody[0].Type; t != "" && !strings.EqualFold(t, "text/plain") {
			return nil, invalidProp("textBody", "textBody is text/plain")
		}
		e.TextBody[0].Type = "text/plain"
		n, serr := s.partNode(ctx, h, e, &e.TextBody[0], "textBody")
		if serr != nil {
			return nil, serr
		}
		text = n
	}
	if len(e.HTMLBody) == 1 {
		if t := e.HTMLBody[0].Type; t != "" && !strings.EqualFold(t, "text/html") {
			return nil, invalidProp("htmlBody", "htmlBody is text/html")
		}
		e.HTMLBody[0].Type = "text/html"
		n, serr := s.partNode(ctx, h, e, &e.HTMLBody[0], "htmlBody")
		if serr != nil {
			return nil, serr
		}
		html = n
	}
	var emails, files, inlined []*mimeNode
	for i := range e.Attachments {
		a := &e.Attachments[i]
		n, serr := s.partNode(ctx, h, e, a, "attachments")
		if serr != nil {
			return nil, serr
		}
		switch {
		case strings.EqualFold(a.Type, "message/rfc822"):
			emails = append(emails, n)
		case a.Disposition != nil && strings.EqualFold(*a.Disposition, "inline") && (text != nil || html != nil):
			inlined = append(inlined, n)
		default:
			files = append(files, n)
		}
	}
	var mailPart *mimeNode
	switch {
	case len(emails) > 1:
		mailPart = multipart("digest", emails...)
	case len(emails) == 1:
		mailPart = emails[0]
	}
	var body *mimeNode
	switch {
	case text != nil && html != nil:
		body = multipart("alternative", text, html)
	case text != nil:
		body = text
	default:
		body = html
	}
	if body != nil && len(inlined) > 0 {
		body = multipart("related", append([]*mimeNode{body}, inlined...)...)
	}
	switch {
	case len(files) > 0:
		head := []*mimeNode{}
		if body != nil {
			head = append(head, body)
		}
		if mailPart != nil {
			head = append(head, mailPart)
		}
		return multipart("mixed", append(head, files...)...), nil
	case body != nil && mailPart != nil:
		return multipart("mixed", body, mailPart), nil
	case body != nil:
		return body, nil
	case mailPart != nil:
		return mailPart, nil
	}
	return &mimeNode{header: leafHeader("text/plain", nil), body: nil}, nil
}

func multipart(sub string, kids ...*mimeNode) *mimeNode {
	var h message.Header
	h.SetContentType("multipart/"+sub, nil)
	return &mimeNode{header: h, kids: kids}
}

func leafHeader(ctype string, params map[string]string) message.Header {
	var h message.Header
	h.SetContentType(ctype, params)
	return h
}

// partNode builds one part: a multipart from its subParts, or a leaf whose
// body is a bodyValue (by partId) or a blob of the caller's own account.
func (s *Server) partNode(ctx context.Context, h *userHandle, e *createEmail, p *createPart, prop string) (*mimeNode, *jmapcore.SetError) {
	ctype := strings.ToLower(p.Type)
	if ctype == "" {
		ctype = "text/plain"
	}
	if strings.HasPrefix(ctype, "multipart/") {
		if p.PartID != nil || p.BlobID != nil || len(p.SubParts) == 0 {
			return nil, invalidProp(prop, "a multipart part has subParts and no body")
		}
		n := multipart(strings.TrimPrefix(ctype, "multipart/"))
		for i := range p.SubParts {
			kid, serr := s.partNode(ctx, h, e, &p.SubParts[i], prop)
			if serr != nil {
				return nil, serr
			}
			n.kids = append(n.kids, kid)
		}
		return n, nil
	}
	if (p.PartID == nil) == (p.BlobID == nil) || len(p.SubParts) > 0 {
		return nil, invalidProp(prop, "a part names exactly one of partId and blobId")
	}
	params := map[string]string{}
	var body []byte
	encoding := "base64"
	if p.PartID != nil {
		v, ok := e.BodyValues[*p.PartID]
		if !ok {
			return nil, invalidProp("bodyValues", "no bodyValue for partId "+*p.PartID)
		}
		if !strings.HasPrefix(ctype, "text/") {
			return nil, invalidProp(prop, "a bodyValue is text")
		}
		body, encoding = []byte(v.Value), "quoted-printable"
		params["charset"] = "utf-8"
	} else {
		raw, err := s.readBlob(ctx, h, *p.BlobID)
		if err != nil {
			return nil, &jmapcore.SetError{Type: "blobNotFound", Properties: []string{prop}, Description: *p.BlobID}
		}
		body = raw
		if p.Charset != nil {
			params["charset"] = *p.Charset
		}
		if ctype == "message/rfc822" {
			encoding = "8bit"
		}
	}
	if p.Name != nil {
		params["name"] = *p.Name
	}
	hdr := leafHeader(ctype, params)
	hdr.Set("Content-Transfer-Encoding", encoding)
	if p.Disposition != nil || p.Name != nil {
		disp := "attachment"
		if p.Disposition != nil {
			disp = strings.ToLower(*p.Disposition)
		}
		dp := map[string]string{}
		if p.Name != nil {
			dp["filename"] = *p.Name
		}
		hdr.SetContentDisposition(disp, dp)
	}
	if p.CID != nil {
		hdr.Set("Content-Id", "<"+strings.Trim(*p.CID, "<>")+">")
	}
	if len(p.Language) > 0 {
		hdr.Set("Content-Language", strings.Join(p.Language, ", "))
	}
	if p.Location != nil {
		hdr.Set("Content-Location", *p.Location)
	}
	return &mimeNode{header: hdr, body: body}, nil
}

// writeEmail renders the top-level headers over root and the tree beneath.
func writeEmail(e *createEmail, root *mimeNode) ([]byte, error) {
	setBoundaries(root)
	mh := mail.Header{Header: root.header}
	mh.Set("MIME-Version", "1.0")
	for key, list := range map[string][]jmapcore.EmailAddress{
		"From": e.From, "To": e.To, "Cc": e.Cc, "Bcc": e.Bcc, "Reply-To": e.ReplyTo, "Sender": e.Sender,
	} {
		if len(list) > 0 {
			mh.SetAddressList(key, mailAddresses(list))
		}
	}
	if e.Subject != nil {
		mh.SetSubject(*e.Subject)
	}
	date := time.Now()
	if e.SentAt != nil {
		date = *e.SentAt
	}
	mh.SetDate(date)
	if len(e.MessageID) > 0 {
		mh.SetMsgIDList("Message-Id", e.MessageID)
	} else {
		mh.SetMessageID(newMessageID(e.From))
	}
	if len(e.InReplyTo) > 0 {
		mh.SetMsgIDList("In-Reply-To", e.InReplyTo)
	}
	if len(e.References) > 0 {
		mh.SetMsgIDList("References", e.References)
	}
	var out bytes.Buffer
	if err := textproto.WriteHeader(&out, mh.Header.Header); err != nil {
		return nil, err
	}
	if err := writeBody(&out, root); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// setBoundaries gives every multipart its boundary before anything is written:
// a part's header is written by its parent.
func setBoundaries(n *mimeNode) {
	if n.kids == nil {
		return
	}
	var rnd [12]byte
	_, _ = rand.Read(rnd[:])
	ctype, params, _ := n.header.ContentType()
	params["boundary"] = hex.EncodeToString(rnd[:])
	n.header.SetContentType(ctype, params)
	for _, kid := range n.kids {
		setBoundaries(kid)
	}
}

// writeBody writes n's body: its parts, or its content in its encoding. The
// content is written as given, in whatever charset it names.
func writeBody(w io.Writer, n *mimeNode) error {
	if n.kids != nil {
		_, params, _ := n.header.ContentType()
		mw := textproto.NewMultipartWriter(w)
		if err := mw.SetBoundary(params["boundary"]); err != nil {
			return err
		}
		for _, kid := range n.kids {
			pw, err := mw.CreatePart(kid.header.Header)
			if err != nil {
				return err
			}
			if err := writeBody(pw, kid); err != nil {
				return err
			}
		}
		return mw.Close()
	}
	switch strings.ToLower(n.header.Get("Content-Transfer-Encoding")) {
	case "base64":
		lw := &lineWrapper{w: w}
		enc := base64.NewEncoder(base64.StdEncoding, lw)
		if _, err := enc.Write(n.body); err != nil {
			return err
		}
		if err := enc.Close(); err != nil {
			return err
		}
		return lw.end()
	case "quoted-printable":
		qw := quotedprintable.NewWriter(w)
		if _, err := qw.Write(n.body); err != nil {
			return err
		}
		return qw.Close()
	}
	_, err := w.Write(n.body)
	return err
}

// lineWrapper breaks base64 at 76 characters (RFC 2045 §6.8).
type lineWrapper struct {
	w   io.Writer
	col int
}

func (l *lineWrapper) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		if l.col == 76 {
			if _, err := l.w.Write([]byte("\r\n")); err != nil {
				return n, err
			}
			l.col = 0
		}
		chunk := p[:min(len(p), 76-l.col)]
		if _, err := l.w.Write(chunk); err != nil {
			return n, err
		}
		l.col += len(chunk)
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

func (l *lineWrapper) end() error {
	if l.col == 0 {
		return nil
	}
	_, err := l.w.Write([]byte("\r\n"))
	return err
}

func mailAddresses(list []jmapcore.EmailAddress) []*mail.Address {
	out := make([]*mail.Address, 0, len(list))
	for _, a := range list {
		addr := &mail.Address{Address: a.Email}
		if a.Name != nil {
			addr.Name = *a.Name
		}
		out = append(out, addr)
	}
	return out
}

// newMessageID mints an id in the domain of the first sender.
func newMessageID(from []jmapcore.EmailAddress) string {
	var rnd [16]byte
	_, _ = rand.Read(rnd[:])
	domain := "localhost"
	if len(from) > 0 {
		if _, d, ok := strings.Cut(from[0].Email, "@"); ok && d != "" {
			domain = d
		}
	}
	return fmt.Sprintf("%s@%s", hex.EncodeToString(rnd[:]), domain)
}
