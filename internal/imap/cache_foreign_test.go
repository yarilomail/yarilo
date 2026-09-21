package imap_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	imap "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/msgcache"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailboxmetrics"
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The strings a cache from the reference carries for this message: its own
// writers' output for the body below (imap-envelope.c:47-87,
// imap-bodystructure.c:26-283). Nothing of ours produced them.
const (
	foreignEnvelope      = `NIL "a listing" (("Ann" NIL "ann" "example.com")) (("Ann" NIL "ann" "example.com")) (("Ann" NIL "ann" "example.com")) (("Bo" NIL "bo" "example.org")) NIL NIL NIL "<listing@example.com>"`
	foreignBodyStructure = `"text" "plain" ("charset" "utf-8") NIL NIL "7bit" 18 2 NIL NIL NIL NIL`
)

// Trap (b): a cache whose field table is the reference's, carrying fields we
// never write, answers the listing without opening the message. The index
// beside it carries no checksum, which is what an index from the reference
// carries (#1714).
func TestAForeignCacheAnswersTheListing(t *testing.T) {
	root := cachedFolder(t)
	home := filepath.Join(root, "test.com", "user")
	info := &mailbox.UserInfo{Username: "user@test.com", Home: home, Driver: "maildir"}

	idx := file.New().OpenUser(info)
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	ic, ok := idx.(interface {
		CachePath(uint64) (string, error)
		EnsureCacheExtension(uint64) (uint32, uint32, error)
		SetCacheOffsets(uint64, map[uint32]mailbox.CacheStamp) error
	})
	if !ok {
		t.Fatal("the index serves no cache")
	}
	indexID, resetID, err := ic.EnsureCacheExtension(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	path, err := ic.CachePath(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	cf, err := mailindex.CreateCache(path, indexID, resetID)
	if err != nil {
		t.Fatal(err)
	}
	// The reference's table, in its own order, including three fields nothing
	// here reads: a foreign cache is not one written to our taste.
	first, err := cf.AddFields([]mailindex.CacheField{
		{Name: "flags", Type: mailindex.CacheFieldBitmask, Size: 4, Decision: mailindex.CacheDecisionYes},
		{Name: "size.physical", Type: mailindex.CacheFieldFixedSize, Size: 8, Decision: mailindex.CacheDecisionYes},
		{Name: "size.virtual", Type: mailindex.CacheFieldFixedSize, Size: 8, Decision: mailindex.CacheDecisionYes},
		{Name: "imap.envelope", Type: mailindex.CacheFieldString, Decision: mailindex.CacheDecisionYes},
		{Name: "imap.bodystructure", Type: mailindex.CacheFieldString, Decision: mailindex.CacheDecisionYes},
		{Name: "mime.parts", Type: mailindex.CacheFieldVariableSize, Decision: mailindex.CacheDecisionYes},
	})
	if err != nil {
		t.Fatal(err)
	}
	size := make([]byte, 8)
	size[0] = byte(len(cachedTestBody))
	size[1] = byte(len(cachedTestBody) >> 8)
	off, err := cf.AppendRecord(0, []mailindex.CacheFieldValue{
		{FieldID: first, Data: []byte{0, 0, 0, 0}},
		{FieldID: first + 1, Data: size},
		{FieldID: first + 2, Data: size},
		{FieldID: first + 3, Data: []byte(foreignEnvelope)},
		{FieldID: first + 4, Data: []byte(foreignBodyStructure)},
		{FieldID: first + 5, Data: []byte{1, 2, 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cf.Close(); err != nil {
		t.Fatal(err)
	}
	// No checksum beside the offset: that is what the reference's index has.
	if err := ic.SetCacheOffsets(f.ID, map[uint32]mailbox.CacheStamp{1: {Offset: off}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	gotSize, env, bs, opens := listingFetch(t, root)
	if opens != 0 {
		t.Errorf("the listing opened %v messages although the cache held every field", opens)
	}
	if env == nil || env.Subject != "a listing" || env.MessageID != "listing@example.com" {
		t.Errorf("envelope from the foreign cache = %+v", env)
	}
	if len(env.To) != 1 || env.To[0].Mailbox != "bo" {
		t.Errorf("recipients from the foreign cache = %+v", env.To)
	}
	if bs == nil || bs.MediaType() != "text/plain" {
		t.Errorf("body structure from the foreign cache = %+v", bs)
	}
	if gotSize != int64(len(cachedTestBody)) {
		t.Errorf("RFC822.SIZE = %d, want %d", gotSize, len(cachedTestBody))
	}
	if opens := mailboxmetrics.MessageOpens("maildir"); opens < 0 {
		t.Fatal(fmt.Sprint("counter is nonsense: ", opens))
	}
}

// A driver's own maildir is untouched by the foreign cache: the trap above
// must not pass because the folder became unreadable.
func TestTheMessageIsStillReadableBesideAForeignCache(t *testing.T) {
	root := cachedFolder(t)
	c := startTestServerIn(t, root)
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Logout().Wait() }()
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	msgs, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{}},
	}).Collect()
	if err != nil {
		t.Fatalf("FETCH: %v", err)
	}
	var got []byte
	for _, v := range msgs[0].BodySection {
		got = v.Bytes
	}
	if string(got) != cachedTestBody {
		t.Errorf("the body came back as %q", string(got))
	}
}

// A cache from the reference holds the headers, not a built envelope: the
// listing must answer from them and open nothing, and the built envelope must
// land in the record so the next listing is a plain hit (#1714).
func TestAForeignCacheOfHeadersAnswersTheListing(t *testing.T) {
	root := cachedFolder(t)
	home := filepath.Join(root, "test.com", "user")
	info := &mailbox.UserInfo{Username: "user@test.com", Home: home, Driver: "maildir"}

	idx := file.New().OpenUser(info)
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	ic, ok := idx.(interface {
		CachePath(uint64) (string, error)
		EnsureCacheExtension(uint64) (uint32, uint32, error)
		SetCacheOffsets(uint64, map[uint32]mailbox.CacheStamp) error
	})
	if !ok {
		t.Fatal("the index serves no cache")
	}
	indexID, resetID, err := ic.EnsureCacheExtension(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	path, err := ic.CachePath(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	cf, err := mailindex.CreateCache(path, indexID, resetID)
	if err != nil {
		t.Fatal(err)
	}
	// Their spelling, their order, and no imap.envelope at all.
	first, err := cf.AddFields([]mailindex.CacheField{
		{Name: "size.physical", Type: mailindex.CacheFieldFixedSize, Size: 8, Decision: mailindex.CacheDecisionYes},
		{Name: "size.virtual", Type: mailindex.CacheFieldFixedSize, Size: 8, Decision: mailindex.CacheDecisionYes},
		{Name: "imap.bodystructure", Type: mailindex.CacheFieldString, Decision: mailindex.CacheDecisionYes},
		{Name: "hdr.SUBJECT", Type: mailindex.CacheFieldHeader, Decision: mailindex.CacheDecisionYes},
		{Name: "hdr.FROM", Type: mailindex.CacheFieldHeader, Decision: mailindex.CacheDecisionYes},
		{Name: "hdr.TO", Type: mailindex.CacheFieldHeader, Decision: mailindex.CacheDecisionYes},
		{Name: "hdr.MESSAGE-ID", Type: mailindex.CacheFieldHeader, Decision: mailindex.CacheDecisionYes},
	})
	if err != nil {
		t.Fatal(err)
	}
	size := make([]byte, 8)
	size[0] = byte(len(cachedTestBody))
	size[1] = byte(len(cachedTestBody) >> 8)
	header := func(line string) []byte {
		b := []byte{1, 0, 0, 0, 0, 0, 0, 0}
		return append(b, line...)
	}
	off, err := cf.AppendRecord(0, []mailindex.CacheFieldValue{
		{FieldID: first, Data: size},
		{FieldID: first + 1, Data: size},
		{FieldID: first + 2, Data: []byte(foreignBodyStructure)},
		{FieldID: first + 3, Data: header("Subject: a listing\r\n")},
		{FieldID: first + 4, Data: header("From: Ann <ann@example.com>\r\n")},
		{FieldID: first + 5, Data: header("To: Bo <bo@example.org>\r\n")},
		{FieldID: first + 6, Data: header("Message-ID: <listing@example.com>\r\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cf.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ic.SetCacheOffsets(f.ID, map[uint32]mailbox.CacheStamp{1: {Offset: off}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	_, env, _, opens := listingFetch(t, root)
	if opens != 0 {
		t.Errorf("the listing opened %v messages although the headers were cached", opens)
	}
	if env == nil || env.Subject != "a listing" || env.MessageID != "listing@example.com" {
		t.Errorf("envelope built from cached headers = %+v", env)
	}
	if len(env.To) != 1 || env.To[0].Mailbox != "bo" {
		t.Errorf("recipients = %+v", env.To)
	}

	// And it was written back, so a reader that knows only imap.envelope finds it.
	idx2 := file.New().OpenUser(info)
	defer idx2.Close() //nolint:errcheck
	f2, err := idx2.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	fc := msgcache.Open(idx2, f2.ID, msgcache.Options{User: "user@test.com", Folder: "INBOX"})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	defer fc.Close()
	msgs, err := idx2.GetMessages(f2.ID, nil)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("messages: %v", err)
	}
	if _, ok := fc.EnvelopeText(msgs[0]); !ok {
		t.Error("the envelope built from the headers was not written back")
	}
}
