//go:build unix

package expand

import "golang.org/x/sys/unix"

func uname() (*unix.Utsname, error) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return nil, err
	}
	return &u, nil
}

func osName() (string, error) {
	u, err := uname()
	if err != nil {
		return "", err
	}
	return unix.ByteSliceToString(u.Sysname[:]), nil
}

func osRelease() (string, error) {
	u, err := uname()
	if err != nil {
		return "", err
	}
	return unix.ByteSliceToString(u.Release[:]), nil
}
