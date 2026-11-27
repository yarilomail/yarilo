package protocol

import (
	"errors"
	"reflect"
	"testing"
)

type lookupOnly struct{}

func (lookupOnly) Lookup(string) (*UserInfo, error) { return nil, nil }

type listing struct {
	users []string
	err   error
}

func (listing) Lookup(string) (*UserInfo, error) { return nil, nil }
func (l listing) Iterate() ([]string, error)     { return l.users, l.err }

// A chain lists the users of every entry that can, in order and once each, and
// passes over one that cannot: a static entry beside SQL no longer turns LIST
// off for the whole chain (#2612).
func TestAUserdbChainListsWhatItsEntriesCan(t *testing.T) {
	broken := errors.New("db down")
	for _, tc := range []struct {
		name    string
		chain   UserdbChain
		want    []string
		wantErr error
	}{
		{"sql then static", UserdbChain{listing{users: []string{"a", "b"}}, lookupOnly{}}, []string{"a", "b"}, nil},
		{"static first", UserdbChain{lookupOnly{}, listing{users: []string{"a"}}}, []string{"a"}, nil},
		{"a name in two entries once", UserdbChain{listing{users: []string{"a", "b"}}, listing{users: []string{"b", "c"}}}, []string{"a", "b", "c"}, nil},
		{"no entry can", UserdbChain{lookupOnly{}, lookupOnly{}}, nil, ErrNoEnumeration},
		{"an entry fails", UserdbChain{listing{users: []string{"a"}}, listing{err: broken}}, nil, broken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.chain.Iterate()
			if !errors.Is(err, tc.wantErr) || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Iterate = %v, %v; want %v, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
	var _ UserdbIterator = UserdbChain{}
}
