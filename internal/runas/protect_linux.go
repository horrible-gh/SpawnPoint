//go:build linux

package runas

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var keyMu sync.Mutex

// key files are separate from SQLite. Copying the database alone does not copy
// a usable password. Root/the service owner can still access their own secrets.
func key(dir string, create bool) ([]byte, error) {
	keyMu.Lock()
	defer keyMu.Unlock()
	if dir == "" {
		return nil, errors.New("credential directory is not configured")
	}
	if create {
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, errors.New("credential directory must be owned by the service account with mode 0700")
	}
	path := filepath.Join(dir, "master.key")
	if create {
		if _, err = os.Lstat(path); os.IsNotExist(err) {
			b := make([]byte, 32)
			if _, err = rand.Read(b); err != nil {
				return nil, err
			}
			defer wipe(b)
			f, e := os.CreateTemp(dir, ".key-")
			if e != nil {
				return nil, e
			}
			temp := f.Name()
			defer os.Remove(temp)
			_, e = f.Write(b)
			if e == nil {
				e = f.Sync()
			}
			ce := f.Close()
			if e == nil {
				e = ce
			}
			if e != nil {
				return nil, e
			}
			// Link publishes a complete file without replacing an existing key.
			if e = os.Link(temp, path); e != nil && !os.IsExist(e) {
				return nil, e
			}
		}
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() != 32 || st.Mode().Perm()&0077 != 0 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, errors.New("invalid credential key permissions or size")
	}
	b := make([]byte, 32)
	_, err = io.ReadFull(f, b)
	return b, err
}
func gcm(dir string, create bool) (cipher.AEAD, error) {
	k, err := key(dir, create)
	if err != nil {
		return nil, err
	}
	defer wipe(k)
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func seal(dir string, plain, binding []byte) (string, error) {
	g, err := gcm(dir, true)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	data := g.Seal(nonce, nonce, plain, binding)
	return base64.StdEncoding.EncodeToString(data), nil
}
func unseal(dir, text string, binding []byte) ([]byte, error) {
	g, err := gcm(dir, false)
	if err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(b) < g.NonceSize() {
		return nil, errors.New("invalid credential")
	}
	return g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], binding)
}
