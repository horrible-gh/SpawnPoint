package main

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"spawnpoint/internal/access"
	"spawnpoint/internal/config"
)

func setupCommand(args []string) (int, bool) {
	if len(args) == 0 || (args[0] != "setup" && args[0] != "admin") {
		return 0, false
	}
	if e := runSetupCommand(args, os.Stdin, os.Stdout, os.Stderr); e != nil {
		fmt.Fprintln(os.Stderr, "SpawnPoint:", e)
		return 1, true
	}
	return 0, true
}
func localSetupURL(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("--url은 서버 로컬 HTTP/HTTPS 주소여야 합니다")
	}
	h := u.Hostname()
	ip := net.ParseIP(h)
	if !strings.EqualFold(h, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("CLI 초기 설정은 localhost/127.0.0.1/[::1]에서만 허용됩니다")
	}
	// Resolve localhost to a literal loopback; do not trust host-file/DNS overrides.
	if strings.EqualFold(h, "localhost") {
		port := u.Port()
		u.Host = "127.0.0.1"
		if port != "" {
			u.Host = net.JoinHostPort("127.0.0.1", port)
		}
	}
	u.Path = ""
	return u, nil
}
func runSetupCommand(args []string, in *os.File, out, errOut io.Writer) error {
	if len(args) < 2 {
		return errors.New("사용법: setup status | setup token | admin init [--url http://127.0.0.1:7527]")
	}
	command := args[0] + " " + args[1]
	if command != "setup status" && command != "setup token" && command != "admin init" {
		return errors.New("지원 명령: setup status, setup token, admin init")
	}
	port := config.DefaultPort
	if p, e := strconv.Atoi(os.Getenv(config.EnvPort)); e == nil && p > 0 && p < 65536 {
		port = p
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(errOut)
	target := flags.String("url", fmt.Sprintf("http://127.0.0.1:%d", port), "실행 중인 SpawnPoint의 로컬 주소")
	login := flags.String("login", "", "새 SpawnPoint 관리자 ID")
	name := flags.String("name", "", "표시 이름")
	piped := flags.Bool("password-stdin", false, "자동화 전용: 비밀번호 한 줄을 표준입력에서 읽음")
	if e := flags.Parse(args[2:]); e != nil {
		return e
	}
	if flags.NArg() != 0 {
		return errors.New("알 수 없는 인수가 있습니다")
	}
	u, e := localSetupURL(*target)
	if e != nil {
		return e
	}
	if command != "setup status" {
		if e = requireSetupAdmin(); e != nil {
			return e
		}
	}
	b := make([]byte, 32)
	if _, e = rand.Read(b); e != nil {
		return e
	}
	nonce := hex.EncodeToString(b)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("초기 설정 요청은 리다이렉트를 따르지 않습니다")
	}}
	resp, e := client.Get(u.String() + "/auth/setup-info?nonce=" + nonce)
	if e != nil {
		return fmt.Errorf("실행 중인 서버에 연결하지 못했습니다 (%s). 서버를 시작하거나 --url로 실제 포트를 지정하세요: %w", u, e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("서버가 초기 설정 상태 조회를 거절했습니다 (HTTP %d). 최신 실행파일인지 확인하세요", resp.StatusCode)
	}
	var info access.SetupInfo
	if e = json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&info); e != nil {
		return errors.New("초기 설정 응답을 읽지 못했습니다")
	}
	if command == "setup status" {
		state := "완료"
		if info.Required {
			state = "최초 관리자 생성 필요"
		}
		fmt.Fprintf(out, "Server: %s\nState: %s\nDatabase: %s\nSetup token: %s\nToken source: %s\n", u, state, info.DBPath, info.TokenPath, info.TokenSource)
		if info.TokenError != "" {
			fmt.Fprintf(out, "Token error: %s\n", info.TokenError)
		}
		return nil
	}
	if !info.Required {
		return errors.New("이미 최초 관리자 설정이 완료되었습니다. 기존 계정은 변경하지 않았습니다")
	}
	var secret string
	if info.TokenSource == "environment" {
		secret = strings.TrimSpace(os.Getenv("SPAWNPOINT_SETUP_TOKEN"))
		if secret == "" {
			return errors.New("서버는 SPAWNPOINT_SETUP_TOKEN을 사용 중입니다. 같은 값을 이 터미널에도 설정하거나 웹에서 OS 관리자 인증을 이용하세요")
		}
	} else {
		if !strings.HasSuffix(info.TokenPath, ".setup-token") {
			return errors.New("잘못된 초기 설정 토큰 경로")
		}
		f, e := openLocalSetupToken(info.TokenPath)
		if e != nil {
			return fmt.Errorf("초기 설정 토큰 읽기 실패 (%s): %w; 웹의 OS 관리자 인증도 사용할 수 있습니다", info.TokenPath, e)
		}
		raw, e := io.ReadAll(io.LimitReader(f, 258))
		f.Close()
		if e != nil {
			return e
		}
		secret = strings.TrimSpace(string(raw))
		clear(raw)
	}
	if len(secret) < 32 || len(secret) > 256 {
		return errors.New("유효한 초기 설정 인증정보가 없습니다")
	}
	if !hmac.Equal([]byte(info.Proof), []byte(access.SetupServerProof(secret, nonce))) {
		return errors.New("서버와 로컬 초기 설정 파일이 일치하지 않습니다. 계정 정보를 전송하지 않았습니다")
	}
	if command == "setup token" {
		fmt.Fprintln(out, secret)
		return nil
	}
	reader := bufio.NewReader(in)
	line := func(prompt string) (string, error) {
		fmt.Fprint(out, prompt)
		s, e := reader.ReadString('\n')
		if e != nil {
			return "", e
		}
		return strings.TrimSpace(s), nil
	}
	if *login == "" {
		*login, e = line("관리자 ID: ")
		if e != nil {
			return e
		}
	}
	if *name == "" {
		if *piped {
			*name = *login
		} else {
			*name, e = line("표시 이름 (Enter=ID): ")
			if e != nil {
				return e
			}
			if *name == "" {
				*name = *login
			}
		}
	}
	var password string
	if *piped {
		password, e = reader.ReadString('\n')
		password = strings.TrimSuffix(strings.TrimSuffix(password, "\n"), "\r")
		if e == io.EOF && password != "" {
			e = nil
		}
	} else {
		password, e = readSetupPassword(in, reader, out, "비밀번호 (12자 이상): ")
		if e == nil {
			var repeat string
			repeat, e = readSetupPassword(in, reader, out, "비밀번호 확인: ")
			if e == nil && password != repeat {
				e = errors.New("비밀번호 확인이 일치하지 않습니다")
			}
		}
	}
	if e != nil {
		return e
	}
	account := access.BootstrapAccount{Login: strings.TrimSpace(*login), Name: *name, Password: password, Nonce: nonce}
	account.Proof = access.SetupAccountProof(secret, account)
	raw, e := json.Marshal(account)
	if e != nil {
		return e
	}
	defer clear(raw)
	response, e := client.Post(u.String()+"/auth/setup-cli", "application/json", bytes.NewReader(raw))
	if e != nil {
		return e
	}
	defer response.Body.Close()
	var result struct {
		OK    bool `json:"ok"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if e = json.NewDecoder(io.LimitReader(response.Body, 16384)).Decode(&result); e != nil {
		return errors.New("초기 설정 결과를 읽지 못했습니다. setup status로 확인하세요")
	}
	if response.StatusCode != 200 || !result.OK {
		return fmt.Errorf("초기 관리자 생성 거부: %s", result.Error.Message)
	}
	fmt.Fprintf(out, "관리자 %s 생성 완료. %s/login 에서 SpawnPoint 계정으로 로그인하세요.\n", account.Login, u)
	return nil
}
