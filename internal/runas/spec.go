// Package runas authenticates an OS account without elevating the web service.
// Passwords never enter command lines, environment blocks or public metadata.
package runas

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
)

// Input is write-only. An omitted/empty password keeps an existing credential
// only when the normalized account is unchanged.
type Input struct {
	Mode     string `json:"mode"`
	Username string `json:"username,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Password string `json:"password,omitempty"`
}

// Spec is private storage data, not an HTTP response.
type Spec struct {
	Username string `json:"username"`
	Domain   string `json:"domain,omitempty"`
	Sealed   string `json:"sealed"`
}

type Summary struct {
	Mode        string `json:"mode"`
	Username    string `json:"username"`
	Domain      string `json:"domain,omitempty"`
	HasPassword bool   `json:"has_password"`
}

func (s *Spec) Summary() *Summary {
	if s == nil {
		return nil
	}
	return &Summary{Mode: "user", Username: s.Username, Domain: s.Domain, HasPassword: s.Sealed != ""}
}
func Clone(s *Spec) *Spec {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}
func (s *Spec) JSON() string {
	if s == nil {
		return ""
	}
	b, _ := json.Marshal(s)
	return string(b)
}
func Parse(raw string) (*Spec, error) {
	if raw == "" {
		return nil, nil
	}
	var s Spec
	if err := json.Unmarshal([]byte(raw), &s); err != nil || s.Username == "" || s.Sealed == "" {
		return nil, errors.New("invalid saved execution identity")
	}
	return &s, nil
}
func Normalize(i Input) (Input, error) {
	i.Username = strings.TrimSpace(i.Username)
	i.Domain = strings.TrimSpace(i.Domain)
	if i.Mode == "service" {
		if i.Username != "" || i.Domain != "" || i.Password != "" {
			return i, errors.New("service identity must not include credentials")
		}
		return i, nil
	}
	if i.Mode != "user" {
		return i, errors.New("run_as.mode must be service or user")
	}
	if i.Username == "" || len(i.Username) > 256 || len(i.Domain) > 256 {
		return i, errors.New("enter a valid username")
	}
	if strings.ContainsAny(i.Username+i.Domain, "\x00\r\n") {
		return i, errors.New("invalid account name")
	}
	if len(i.Password) > 4096 || strings.ContainsRune(i.Password, 0) {
		return i, errors.New("invalid password length or character")
	}
	if runtime.GOOS == "windows" {
		if n := strings.IndexByte(i.Username, '\\'); n >= 0 {
			if i.Domain != "" {
				return i, errors.New("specify a domain only once")
			}
			i.Domain, i.Username = i.Username[:n], i.Username[n+1:]
		}
		if strings.Contains(i.Username, "@") {
			if i.Domain != "" {
				return i, errors.New("a UPN username must not include a separate domain")
			}
		} else if i.Domain == "" {
			i.Domain = "."
		}
		if i.Username == "" {
			return i, errors.New("enter a username")
		}
	} else {
		if i.Domain != "" {
			return i, errors.New("Linux execution accounts do not use a domain field")
		}
		if strings.HasPrefix(i.Username, "-") || strings.ContainsAny(i.Username, "/\\: \t") {
			return i, errors.New("invalid Linux account name")
		}
		if strings.ContainsAny(i.Password, "\r\n") {
			return i, errors.New("Linux password must not contain a line break")
		}
	}
	return i, nil
}
func accountEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
func Apply(dir string, input Input, current *Spec) (*Spec, error) {
	i, err := Normalize(input)
	if err != nil {
		return nil, err
	}
	if i.Mode == "service" {
		return nil, nil
	}
	if i.Password == "" {
		if current != nil && accountEqual(current.Username, i.Username) && accountEqual(current.Domain, i.Domain) && current.Sealed != "" {
			return Clone(current), nil
		}
		return nil, errors.New("enter a password for this account")
	}
	s := &Spec{Username: i.Username, Domain: i.Domain}
	s.Sealed, err = seal(dir, []byte(i.Password), s.binding())
	if err != nil {
		return nil, fmt.Errorf("cannot protect execution credential: %w", err)
	}
	return s, nil
}
func (s *Spec) binding() []byte {
	return []byte("spawnpoint/run-as/v1\x00" + s.Username + "\x00" + s.Domain)
}
func (s *Spec) password(dir string) ([]byte, error) {
	if s == nil {
		return nil, errors.New("no execution identity")
	}
	b, err := unseal(dir, s.Sealed, s.binding())
	if err != nil {
		return nil, errors.New("saved credential cannot be decrypted; re-enter the password")
	}
	return b, nil
}
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
