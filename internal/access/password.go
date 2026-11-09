package access

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const iterations = 600000

func hashPassword(password string) (string, error) {
	if !utf8.ValidString(password) || utf8.RuneCountInString(password) < 12 || len(password) > 1024 || strings.ContainsRune(password, 0) {
		return "", errors.New("비밀번호는 12자 이상, 1024바이트 이하로 입력하세요")
	}
	salt := randomBytes(16)
	key, e := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if e != nil {
		return "", e
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iterations, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}
func checkPassword(encoded, password string) bool {
	p := strings.Split(encoded, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" || len(password) > 1024 {
		return false
	}
	n, e := strconv.Atoi(p[1])
	if e != nil || n < iterations || n > 2000000 {
		return false
	}
	salt, e := base64.RawStdEncoding.DecodeString(p[2])
	if e != nil || len(salt) != 16 {
		return false
	}
	want, e := base64.RawStdEncoding.DecodeString(p[3])
	if e != nil || len(want) != 32 {
		return false
	}
	got, e := pbkdf2.Key(sha256.New, password, salt, n, 32)
	return e == nil && subtle.ConstantTimeCompare(got, want) == 1
}
func dummyPassword(password string) {
	_, _ = pbkdf2.Key(sha256.New, password, []byte("unknown-account!"), iterations, 32)
}
func sameSecret(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
