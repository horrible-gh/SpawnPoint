# 로컬 Caddy HTTPS 인식 수정 — 검증 기록

## 기준

- 브랜치: `feature/access-control`
- 제품 변경 커밋: `d5f13d975ab6edf38721f9b7ba4d6dcb39229f94`
- 최종 시험 커밋: `ed473e082a85c7be7977e1f50dc59d8aacf92055`
- 검증 일자: 2026-11-10 (JST)
- [GitHub Actions 42730105854](https://github.com/horrible-gh/SpawnPoint/actions/runs/42730105854)
- Windows Server 2025 job `129545654086`: success
- Ubuntu job `129545654447`: success

이 문서 추가는 제품 코드 변경이 아니다. `main` 병합과 사용자 운영 서버 배포는 하지 않았다.

## 수정

같은 서버에서 Caddy가 TLS를 종료하고 `reverse_proxy 127.0.0.1:7527`로 전달할 때,
기존에는 환경변수 opt-in이 없어 실제 HTTPS 접속을 원격 HTTP로 판정했다.
이제 Caddy 기본 전달 헤더를 루프백 소켓 연결에서 검증해 추가 설정 없이 인식한다.
로그인·최초 관리자 등록·세션 쿠키·Origin 검사·지정 OS 계정 실행이 공통 판정을 사용한다.
단순히 경고 문구를 감추거나 브라우저의 URL 값을 권한 판정에 사용한 수정이 아니다.

인증서 신뢰 설정, 사용자 PC, Caddy 서비스 설정, DB, 기존 프로세스는 변경하지 않았다.
간결한 로그인 화면과 별도 도움말 창도 그대로 유지했다.

## 시험

실제 공식 Caddy v2.10.2 실행파일을 SHA-512 릴리스 체크섬으로 검증한 뒤,
일회용 내부 CA·DB·포트로 TLS 프런트엔드와 HTTP 백엔드를 구성했다.
`SPAWNPOINT_TRUST_LOOPBACK_PROXY`를 제거한 환경에서 아래를 검사했다.

| 검사 | Windows | Linux |
|---|---|---|
| 전체 빌드·단위 회귀·배포 빌드 | 통과 | 통과 |
| 실제 Caddy HTTPS의 secure/https 인식 | 통과 | 통과 |
| 자체 관리자 초기화와 로그인 | 통과 | 통과 |
| 실제 OS 관리자 초기화와 OS 로그인 | 통과 | 통과 |
| Secure/HttpOnly 세션 유지와 로그아웃 | 통과 | 통과 |
| 지정 OS 사용자 실행과 whoami/id 출력 확인 | 통과 | 통과 |
| HTTP 프런트엔드·조작한 전달 헤더·잘못된 Origin/CSRF 거부 | 통과 | 통과 |
| 기존 접근 제어·초기 설정·설치된 서비스 회귀 | 통과 | 통과 |
| 데이터 경쟁 검사 | 별도 미실행 | 통과 |
| 실제 Chromium HTTPS 화면·관리자 등록·세션 검사 | 별도 미실행 | 통과 |

테스트 클라이언트는 일회용 CA를 사용하고 OS 인증서 저장소를 변경하지 않는다.
브라우저 시험의 인증서 예외는 일회용 Chromium 컨텍스트에만 적용한다.
`https-caddy-setup.png`, `https-caddy-session.png`는 위 실행의
`access-control-browser-evidence` 아티팩트에 들어 있다. 모의 API 화면이 아니다.

## 보안 경계 및 적용

자동 인식은 실제 루프백 TCP peer와 일관된 단일 스킴/Host 및 유효한 전달 IP 정보를
요구한다. 원격 소켓의 위조 헤더는 신뢰하지 않으며, 프록시를 거친 HTTP에는 직접
localhost 예외를 주지 않는다. 로그인·역할·CSRF 검사는 유지한다.
동일 호스트 OS 프로세스와 프록시를 신뢰하는 구성으로, 상호 불신 로컬 사용자에
대한 격리 수단은 아니다. 프록시는 외부 전달 헤더를 덮어써야 한다.
명시적 `SPAWNPOINT_TRUST_LOOPBACK_PROXY=0`은 자동 인식을 끄며 기존 `1` 옵션도 유지한다.

적용하려면 이 브랜치로 재빌드하고 실제 실행파일을 교체한 후 서버를 재시작한다.
일반적인 동일 호스트 Caddy 구성에는 추가 환경변수나 Caddy 변경이 필요 없다.
검증은 일회용 CI 환경에서 수행했으며 사용자의 실제 서버에는 배포하지 않았다.
