package runas

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

const childFlag = "--spawnpoint-run-as-child"
const readyPrefix = "SPAWNPOINT_RUN_AS_READY:"

type Request struct {
	Command string            `json:"command"`
	Cwd     string            `json:"cwd"`
	Env     map[string]string `json:"env"`
	Nonce   string            `json:"nonce"`
}

// Child remains suspended (Windows) or blocked on its request pipe (Linux)
// until Ready, allowing the caller to establish containment before user code.
type Child struct {
	PID         int
	Handle      uintptr
	Output      *os.File
	request     *os.File
	payload     []byte
	nonce       string
	account     string
	resume      func() error
	terminate   func()
	wait        func() int
	waitOnce    sync.Once
	releaseOnce sync.Once
	code        int
}

func (c *Child) Wait() int { c.waitOnce.Do(func() { c.code = c.wait() }); return c.code }
func (c *Child) Abort() {
	c.finishAttempt(true)
	c.request.Close()
	c.terminate()
	c.Output.Close()
	c.Wait()
}
func (c *Child) finishAttempt(failed bool) { c.releaseOnce.Do(func() { release(c.account, failed) }) }

var attempts = struct {
	sync.Mutex
	active map[string]bool
	failed map[string]time.Time
}{active: map[string]bool{}, failed: map[string]time.Time{}}

func acquire(account string) error {
	attempts.Lock()
	defer attempts.Unlock()
	if attempts.active[account] || time.Since(attempts.failed[account]) < 10*time.Second {
		return errors.New("account authentication is busy or temporarily throttled; retry after 10 seconds")
	}
	attempts.active[account] = true
	return nil
}
func release(account string, failed bool) {
	attempts.Lock()
	defer attempts.Unlock()
	delete(attempts.active, account)
	if failed {
		attempts.failed[account] = time.Now()
	} else {
		delete(attempts.failed, account)
	}
	for a, t := range attempts.failed {
		if time.Since(t) > time.Minute {
			delete(attempts.failed, a)
		}
	}
}
func Launch(dir string, spec *Spec, command, cwd string, env map[string]string) (*Child, error) {
	if spec == nil {
		return nil, errors.New("missing execution identity")
	}
	account := spec.Domain + "\\" + spec.Username
	if runtime.GOOS == "windows" {
		account = strings.ToLower(account)
	}
	if err := acquire(account); err != nil {
		return nil, err
	}
	password, err := spec.password(dir)
	if err != nil {
		release(account, false)
		return nil, err
	}
	defer wipe(password)
	nonceBytes := make([]byte, 16)
	if _, err = rand.Read(nonceBytes); err != nil {
		release(account, false)
		return nil, err
	}
	nonce := hex.EncodeToString(nonceBytes)
	payload, err := json.Marshal(Request{command, cwd, env, nonce})
	if err != nil {
		release(account, false)
		return nil, err
	}
	if len(payload) > 65536 {
		release(account, false)
		return nil, errors.New("execution request is too large")
	}
	c, err := launch(spec, password)
	if err != nil {
		release(account, true)
		return nil, err
	}
	c.payload = payload
	c.nonce = nonce
	c.account = account
	return c, nil
}

// Ready consumes only the handshake, byte by byte; application output stays in
// the original pipe for the existing log collector, including non-UTF8 bytes.
func (c *Child) Ready() error {
	ok := false
	defer func() { c.finishAttempt(!ok) }()
	if err := c.resume(); err != nil {
		return err
	}
	go func() { defer c.request.Close(); c.request.Write(c.payload); c.payload = nil }()
	done := make(chan error, 1)
	go func() {
		target := []byte(readyPrefix + c.nonce + "\n")
		seen := make([]byte, 0, len(target))
		one := []byte{0}
		for n := 0; n < 8192; n++ {
			if _, err := io.ReadFull(c.Output, one); err != nil {
				done <- errors.New("account authentication or command startup failed")
				return
			}
			seen = append(seen, one[0])
			if len(seen) > len(target) {
				seen = seen[1:]
			}
			if string(seen) == string(target) {
				done <- nil
				return
			}
		}
		done <- errors.New("invalid execution handshake")
	}()
	select {
	case err := <-done:
		ok = err == nil
		return err
	case <-time.After(30 * time.Second):
		return errors.New("account authentication timed out")
	}
}

// HandleChild is only entered by the private process-launch protocol. It does
// not grant privileges; it already runs as the account authenticated by the OS.
func HandleChild() bool {
	if len(os.Args) != 2 || os.Args[1] != childFlag {
		return false
	}
	os.Exit(childMain())
	return true
}
func childMain() int {
	input, err := childInput()
	if err != nil {
		return 125
	}
	defer input.Close()
	var r Request
	d := json.NewDecoder(io.LimitReader(input, 65537))
	d.DisallowUnknownFields()
	if err = d.Decode(&r); err != nil || r.Command == "" || len(r.Nonce) != 32 {
		return 125
	}
	if _, err = hex.DecodeString(r.Nonce); err != nil {
		return 125
	}
	if d.Decode(new(any)) != io.EOF {
		return 125
	}
	input.Close()
	return executeChild(r)
}
func ready(nonce string) error { _, err := fmt.Fprintln(os.Stdout, readyPrefix+nonce); return err }
func copyOutput(r io.Reader)   { io.Copy(os.Stdout, r) }
