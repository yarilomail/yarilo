package main

import (
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"
)

// basicAuth signs every request of a client library that knows no TLS options.
type basicAuth struct {
	base       http.RoundTripper
	user, pass string
}

func (b basicAuth) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.SetBasicAuth(b.user, b.pass)
	return b.base.RoundTrip(r)
}

// jmapLibClient is the go-jmap client on the probed address. The session names
// the public base URL; its URLs are pointed at -jmap-host, as the other probes do.
func jmapLibClient() (*gojmap.Client, gojmap.ID, error) {
	origin := "https://" + net.JoinHostPort(jmapHost(), *flagJMAPPort)
	hc := jmapClient()
	hc.Transport = basicAuth{base: hc.Transport, user: *flagJMAPUser, pass: *flagJMAPPass}
	c := &gojmap.Client{SessionEndpoint: origin + "/.well-known/jmap", HttpClient: hc}
	if err := c.Authenticate(); err != nil {
		return nil, "", fmt.Errorf("session: %w", err)
	}
	for _, u := range []*string{&c.Session.APIURL, &c.Session.UploadURL, &c.Session.DownloadURL} {
		if i := strings.Index(*u, "://"); i >= 0 {
			if j := strings.Index((*u)[i+3:], "/"); j >= 0 {
				*u = origin + (*u)[i+3+j:]
			}
		}
	}
	account := c.Session.PrimaryAccounts[mail.URI]
	if account == "" {
		return nil, "", fmt.Errorf("no primary mail account in the session")
	}
	return c, account, nil
}

func jmapCallLib[T any](c *gojmap.Client, m gojmap.Method) (T, error) {
	var zero T
	req := &gojmap.Request{}
	req.Invoke(m)
	resp, err := c.Do(req)
	if err != nil {
		return zero, fmt.Errorf("%s: %w", m.Name(), err)
	}
	if len(resp.Responses) != 1 {
		return zero, fmt.Errorf("%s: %d responses", m.Name(), len(resp.Responses))
	}
	got, ok := resp.Responses[0].Args.(T)
	if !ok {
		return zero, fmt.Errorf("%s answered %#v", m.Name(), resp.Responses[0].Args)
	}
	return got, nil
}

// checkJMAPRoundTrip is #712's acceptance through a client library: upload and
// import, mark read, move, destroy, each seen by Email/changes on a second
// client.
func checkJMAPRoundTrip() error {
	writer, account, err := jmapLibClient()
	if err != nil {
		return err
	}
	watcher, _, err := jmapLibClient()
	if err != nil {
		return err
	}
	got, err := jmapCallLib[*email.GetResponse](watcher, &email.Get{Account: account, IDs: []gojmap.ID{}})
	if err != nil {
		return err
	}
	state := got.State
	// seen asks the second client what changed since its last state.
	seen := func(step string, id gojmap.ID, pick func(*email.ChangesResponse) []gojmap.ID) error {
		ch, err := jmapCallLib[*email.ChangesResponse](watcher, &email.Changes{Account: account, SinceState: state})
		if err != nil {
			return fmt.Errorf("%s: %w", step, err)
		}
		state = ch.NewState
		if !slices.Contains(pick(ch), id) {
			return fmt.Errorf("%s: Email/changes did not report %s: %+v", step, id, ch)
		}
		return nil
	}

	boxes, err := jmapCallLib[*mailbox.GetResponse](writer, &mailbox.Get{Account: account})
	if err != nil {
		return err
	}
	var inbox gojmap.ID
	for _, mb := range boxes.List {
		if mb.Role == mailbox.RoleInbox {
			inbox = mb.ID
		}
	}
	if inbox == "" {
		return fmt.Errorf("no inbox among %d mailboxes", len(boxes.List))
	}

	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	raw := fmt.Sprintf("From: smoketest@yarilo.invalid\r\nMessage-ID: <rt-%s@smoketest>\r\nSubject: jmap round trip %s\r\n\r\nbody\r\n", stamp, stamp)
	up, err := writer.Upload(account, strings.NewReader(raw))
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	imp, err := jmapCallLib[*email.ImportResponse](writer, &email.Import{Account: account,
		Emails: map[string]*email.EmailImport{"c1": {BlobID: up.ID, MailboxIDs: map[gojmap.ID]bool{inbox: true}}}})
	if err != nil {
		return err
	}
	created := imp.Created["c1"]
	if created == nil {
		return fmt.Errorf("Email/import created nothing: %+v", imp.NotCreated)
	}
	id := created.ID
	if err := seen("import", id, func(c *email.ChangesResponse) []gojmap.ID { return c.Created }); err != nil {
		return err
	}

	set := func(step string, s *email.Set) error {
		r, err := jmapCallLib[*email.SetResponse](writer, s)
		if err != nil {
			return fmt.Errorf("%s: %w", step, err)
		}
		if len(r.NotUpdated)+len(r.NotDestroyed) > 0 {
			return fmt.Errorf("%s refused: %+v %+v", step, r.NotUpdated, r.NotDestroyed)
		}
		return nil
	}
	if err := set("mark read", &email.Set{Account: account, Update: map[gojmap.ID]gojmap.Patch{id: {"keywords/$seen": true}}}); err != nil {
		return err
	}
	if err := seen("mark read", id, func(c *email.ChangesResponse) []gojmap.ID { return c.Updated }); err != nil {
		return err
	}

	made, err := jmapCallLib[*mailbox.SetResponse](writer, &mailbox.Set{Account: account,
		Create: map[gojmap.ID]*mailbox.Mailbox{"m1": {Name: "smoketest-" + stamp}}})
	if err != nil {
		return err
	}
	if made.Created["m1"] == nil {
		return fmt.Errorf("Mailbox/set created nothing: %+v", made.NotCreated)
	}
	target := made.Created["m1"].ID
	defer jmapCallLib[*mailbox.SetResponse](writer, &mailbox.Set{Account: account, //nolint:errcheck // cleanup
		Destroy: []gojmap.ID{target}, OnDestroyRemoveEmails: true})
	if err := set("move", &email.Set{Account: account, Update: map[gojmap.ID]gojmap.Patch{id: {"mailboxIds": map[gojmap.ID]bool{target: true}}}}); err != nil {
		return err
	}
	if err := seen("move", id, func(c *email.ChangesResponse) []gojmap.ID { return c.Updated }); err != nil {
		return err
	}

	if err := set("destroy", &email.Set{Account: account, Destroy: []gojmap.ID{id}}); err != nil {
		return err
	}
	return seen("destroy", id, func(c *email.ChangesResponse) []gojmap.ID { return c.Destroyed })
}
