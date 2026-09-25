package ftsproto_test

import (
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/ftsproto"
)

// setService answers LOOKUPIN with what it is given and keeps what it was asked.
type setService struct {
	slowService
	user    string
	folders []fts.MailboxRef
	query   fts.Query
	answer  fts.SetResult
}

func (s *setService) LookupIn(user string, folders []fts.MailboxRef, q fts.Query) (fts.SetResult, error) {
	s.user, s.folders, s.query = user, folders, q
	return s.answer, nil
}

// The folder set, the query and every hit cross the wire intact: a copy in two
// folders is two hits, and losing either is a message the client never sees.
func TestLookupInRoundTrip(t *testing.T) {
	svc := &setService{answer: fts.SetResult{
		Definite: []fts.FolderHit{{Folder: "aa", UID: 1}, {Folder: "bb", UID: 3}},
		Maybe:    []fts.FolderHit{{Folder: "bb", UID: 7}},
	}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go ftsproto.Serve(ln, svc)       //nolint:errcheck
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	r, err := ftsproto.Dial(ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() }) //nolint:errcheck

	folders := []fts.MailboxRef{{Name: "INBOX", GUID: "aa", UIDValidity: 1}, {Name: "Archive\tx", GUID: "bb", UIDValidity: 2}}
	q := fts.Query{Terms: []fts.Term{{Field: fts.FieldBody, Words: []fts.Word{{Variants: []string{"wolf"}}}}}}
	got, err := r.LookupIn("u@x", folders, q)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, svc.answer) {
		t.Errorf("the client read %+v, want %+v", got, svc.answer)
	}
	if svc.user != "u@x" || !reflect.DeepEqual(svc.folders, folders) || !reflect.DeepEqual(svc.query, q) {
		t.Errorf("the service was asked %q %+v %+v", svc.user, svc.folders, svc.query)
	}
}
