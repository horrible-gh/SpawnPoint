# 최초 관리자 설정 개선 — 검증 기록

## 기준

- 브랜치: `feature/access-control`
- 구현 커밋: `3c7bcbd01bc0b2ba297740852512960a2206981b`
- 최종 검증 커밋: `75f423e04655000d94d3bda6f3275a6921247554`
- 검증 일자: 2026-11-10 (JST)
- [GitHub Actions 42721156632](https://github.com/horrible-gh/SpawnPoint/actions/runs/42721156632)
- Windows job `129515149001`: **success**
- Linux job `129515149365`: **success**

이 문서를 추가한 커밋은 문서만 변경한다. `main` 병합이나 사용자 운영 서버 배포는 하지 않았다.

## 변경한 사용 흐름

최초 `/login`에서는 **OS 관리자 인증**을 기본으로 표시한다. 토큰 파일을 직접 찾거나 DB 위치를 추측할 필요가 없다.

Windows는 서버 Administrators 계정의 ID와 OS 비밀번호를 인증하고, Linux는 UID 0 또는 root 셸 실행을 허용하는 실제 sudo 정책을 인증한다. Linux의 sudo/wheel 그룹명만으로 권한을 부여하지 않으며 일부 명령만 sudo 허용된 일반 계정은 거절한다. root 셸을 실제 실행하지 않는다. OS 비밀번호는 저장하지 않는다.

SpawnPoint 자체 관리자 계정을 만들려면 **업데이트한 서버가 실행 중인 상태에서 서버 로컬 터미널**을 사용한다.

Windows 관리자 권한 PowerShell:

```powershell
.\spawnpoint.exe admin init
```

Linux sudo/root 터미널:

```sh
sudo ./spawnpoint admin init
```

ID, 표시 이름, 비밀번호와 확인을 입력하면 된다. 비밀번호는 화면에 표시하지 않는다.

상태와 실제 경로를 확인하려면 `setup status`, 토큰 원문이 특별히 필요하면 관리자 권한으로 `setup token`을 사용한다.

```powershell
.\spawnpoint.exe setup status
.\spawnpoint.exe setup token
```

```sh
./spawnpoint setup status
sudo ./spawnpoint setup token
```

포트가 기본 7527과 다르면 각 명령 뒤에 `--url http://127.0.0.1:실제포트`를 지정한다. 웹 화면에도 현재 리스너 포트에 맞춘 명령이 표시된다. `setup status`는 실행 중인 서버의 실제 DB 경로·토큰 경로·설정 상태·토큰 생성 오류를 조회한다. CLI를 다른 폴더에서 실행하거나 CLI 환경변수의 DB 경로가 달라도 새 DB를 만들지 않는다.

CLI는 실행 중인 서버에 연결하는 방식이다. 서버가 꺼져 있으면 먼저 서버를 시작해야 하며, 오프라인 DB 수정 또는 계정 복구 명령은 아니다. 이미 최초 계정이 있으면 모든 초기화 방법이 재생성을 거절한다.

## 검증 결과

| 항목 | Windows Server 2025 | Ubuntu |
|---|---|---|
| Go 전체 빌드·단위 회귀·기존 배포 스크립트 | 통과 | 통과 |
| 실제 일반 OS 로그인·OS 관리자 자격 검증 | 통과 | 통과 |
| 잘못된 비밀번호·관리자 아닌 계정의 최초 등록 거부 | 통과 | 통과 |
| 실제 서버의 OS 관리자 생성·자체 관리자 CLI 생성·재시작 후 로그인 | 통과 | 통과 |
| 실제 경로 조회·명시적 토큰 표시·중복 초기화 차단 | 통과 | 통과 |
| 기존 역할·그룹·세션·CSRF·토큰·감사·권한 API 회귀 | 통과 | 통과 |
| 설치된 서비스에서 OS 관리자 등록·CLI 경로/토큰 확인 | LocalService 통과 | systemd 통과 |
| 서비스의 자체/OS 로그인·지정 계정 실행·자동 시작·하위 프로세스 종료 | 통과 | 통과 |
| race 검사 | 별도 실행하지 않음 | 통과 |
| 실제 HTTP/쿠키 기반 Chromium 화면·반응형 검사 | 별도 실행하지 않음 | 통과 |

Windows 로그에는 기존 실제 서버 접근 제어 검사 158건, bootstrap/CLI/identity/transport/restart 검사 25건의 PASS와 설치된 서비스 시나리오 PASS가 기록된다. 테스트의 한국어 CLI 출력은 UTF-8로 해석한다. 운영 인증 조건이나 권한 검사를 완화해서 테스트를 통과시키지 않았다.

UI의 OS 관리자 인증/자체 계정 탭 및 상태 확인 안내 캡처는 위 실행의 `access-control-browser-evidence` 아티팩트에 포함된다. `setup-os-desktop.png`는 실제 Linux 서버 화면이다. 시안용 역할 전환이나 모의 API 화면이 아니다. 아티팩트 보존기간은 7일이다.

## 보안·적용 경계

- 이 기능은 ID/PW 기반 OS 관리자 인증이다. 비밀번호 없는 Windows 통합 SSO는 아니다.
- Linux OS 인증은 non-root SpawnPoint 서버에서 PAM/su를 사용한다. root로 서버 자체를 실행한 경우 자체 계정 CLI 초기화를 사용한다. root 계정으로 인증하는 것과 root로 웹 서버를 실행하는 것은 다르다.
- 원격 브라우저 로그인은 HTTPS, 로컬은 localhost를 사용한다. 원격 HTTP 비밀번호 전송을 허용하지 않는다.
- CLI의 원격 주소·프록시 환경변수·리다이렉트는 거절한다. 관리자 생성 요청은 보호된 로컬 초기 설정 정보와 내용에 바인딩된 HMAC으로 검증한다.
- `setup status`는 원문 토큰을 출력하지 않으며 원격 비로그인 응답에 경로/토큰을 공개하지 않는다.
- 기존 DB·등록 명령·자동 시작·권한·실행 계정은 삭제하지 않는다. 변경분 적용에는 해당 브랜치 업데이트, 재빌드, 실행파일 교체 및 서버 재시작이 필요하다.
- GitHub의 일회용 Windows/Ubuntu 환경 검증이며 사용자의 실제 장비, 모든 도메인 정책, NSS/LDAP 또는 사용자 정의 PAM/sudo 정책까지 검증한 것은 아니다.
