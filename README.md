# SpawnPoint

새 작업 인스턴스를 생성(spawn)하는 **진입점** 모듈. 외부의 "새 인스턴스를 하나
만들어 달라"는 요청을 받아, 처리 가능한지 검증하고, 통과한 요청에 한해 고유
식별자를 부여해 인스턴스를 등록한 뒤, 호출자에게 핸들(식별자 + 상태)을 돌려준다.

설계 문서 체인: D-0004(기본설계) → P-0005(프로토콜) → L-0006(로직) → DB-0007(DB).
본 구현은 그 네 문서에서 확정된 규칙을 코드로 옮긴 것이다.

## 구성

```
app/
  main.py            애플리케이션 진입점 (환경변수 설정 → 서버 기동, API+화면+실행기 통합)
runnerview/
  static/index.html  실행기 화면(실제 /processes* API에 연결된 동적 화면)
  page.py            정적 화면 로더
spawnpoint/
  params.py          파라미터 (L-0006 §1)
  clock.py           시각/타임존 유틸 (표시=KST, 저장/비교=UTC 고정폭 ISO)
  models.py          SpawnInstance 모델 (DB-0007 §2.1)
  storage.py         Registry — sqloader(SQLite 백엔드) 기반 저장소, insert/조회/순번
  sql/
    sqlite/            sqloader 쿼리 파일 (spawn_instance.json / spawn_daily_seq.json + .sql)
    migration/sqlite/  sqloader 마이그레이션 파일 (DB-0007 §2 DDL)
  validator.py       유효성 검증부 (L-0006 §2.2)
  allocator.py       식별자 발급부 (L-0006 §2.3)
  auth.py            Bearer 토큰 검증
  results.py         응답 구성부 (P-0005 응답 형태)
  service.py         최상위 처리 spawn() (L-0006 §2.1)
  runner.py          실행기(ProcessManager) — 실제 OS 서브프로세스 run/stop/restart/list/로그
  http_api.py        HTTP 어댑터 (POST /spawn + 화면 서빙 + /processes* 실행기 라우트)
tests/
  test_spawnpoint.py       프로토콜 4개 시나리오 + 경계 조건 (핵심 단위 테스트)
  test_http_integration.py 실제 소켓 기동 후 엔드투엔드 검증(+ healthz/405/413/404)
  test_runnerview.py       한 서버가 화면(/)과 API(/spawn)를 함께 서빙함을 검증
  test_runner.py           ProcessManager 단위 테스트 (start/stop/restart/list/로그)
  test_processes_http.py   /processes* 라우트 엔드투엔드 검증
```

API 서버와 화면은 **하나의 프로세스·하나의 포트**에서 함께 뜬다. `app/main.py`가
`runnerview`의 정적 화면 HTML을 `spawnpoint.http_api`에 주입해 `GET /`로 서빙한다.

컴포넌트 흐름(D-0004 §2): `Intake → Validator → Allocator → Registrar → Responder`.

## 엔드포인트

`POST /spawn` — `Content-Type: application/json`. 기본 로컬 실행에서는 인증 헤더가
필요 없다. `SPAWNPOINT_API_TOKENS`를 설정한 경우에만
`Authorization: Bearer <token>` 헤더가 필요하다.

요청:
```json
{
  "requester": "user-a1b2c3",
  "kind": "session",
  "request_key": "req-7f3a90",
  "options": { "label": "nightly-build", "ttl_seconds": 3600 }
}
```

성공 응답:
```json
{
  "ok": true,
  "instance": {
    "id": "spwn_20260725_0001a7c3f0",
    "status": "created",
    "kind": "session",
    "requester": "user-a1b2c3",
    "created_at": "2026-07-25T13:40:12+09:00"
  }
}
```

- 유효성 실패 → `{"ok": false, "error": {"code": "invalid_request", "field": "...", "message": "..."}}` (HTTP 400)
- 인증 실패 → `code: "unauthorized"` (HTTP 401)
- 중복 요청(같은 `request_key`가 `dedup_window`(300초) 내 재도착) → 기존 인스턴스를 `"deduplicated": true`와 함께 반환
- 본문 과대 → `code: "payload_too_large"` (HTTP 413, 상한 64KiB)
- 저장 실패 → `code: "storage_error"` (HTTP 500)

### 화면 / 운영 라우트

- `GET /`, `GET /index.html` → 실행기 화면(HTML). API와 같은 포트.
- `GET /healthz` → `{"ok": true, "status": "healthy"}` (라이브니스 점검, 인증 불요)
- 알 수 없는 경로 → HTTP 404 `not_found`
- 미지원 메서드(PUT/DELETE/PATCH) → HTTP 405 `method_not_allowed`

### 실행기(Process Runner) 라우트

기본 로컬 실행에서는 인증 없이 쓸 수 있다. `SPAWNPOINT_API_TOKENS`를 설정했다면
모두 `Authorization: Bearer <token>`이 필요하다. `spawn()`(위 인스턴스 등록
프로토콜)과는 무관한 별개 기능 — 실제 OS 서브프로세스를
띄우고/죽이고/재시작하고/tail한다.

- `POST /processes` — 새 프로세스 실행. 본문 `{"cmd": "...", "label": "...", "cwd": "...", "env": {"K":"V"}}`
  (`cmd` 필수, 나머지 선택. `label` 미지정 시 `cmd`의 첫 토큰). `shell=True`로 실행하며
  stdout+stderr를 `logs/<id>.log`에 이어붙인다.
- `GET /processes` — 실행 중/종료된 프로세스 목록. 각 항목: `id, label, cmd, cwd, pid,
  status(running|exited|killed|stopped), exit_code, started_at, ended_at`.
- `POST /processes/<id>/stop` — 프로세스 트리 종료(POSIX: `killpg` SIGTERM→SIGKILL,
  Windows: `taskkill /PID <pid> /T /F`).
- `POST /processes/<id>/restart` — 실행 중이면 정지 후 같은 id로 재기동.
- `GET /processes/<id>/logs?offset=<바이트>` — `offset` 이후 새로 쓰인 로그 텍스트와
  다음 `offset`을 반환(폴링 tail). 화면은 1초 간격으로 폴링한다.
- 존재하지 않는 id → HTTP 404 `not_found`.

## 식별자 형식

`spwn_{YYYYMMDD}_{seq}{rand}` — 일자별 순번(원자적 증가, 4자리 0패딩) + 6자리 16진수
무작위 접미부. 유일성은 (날짜, 순번)으로 1차 보장하고 PK 제약으로 최종 보장하며,
충돌 시 재시도 경로로 흡수한다.

## 실행

```bash
# 서버 기동 (기본 로컬 실행: 인증 없음)
python -m app.main
#   화면(UI): http://127.0.0.1:8091/
#   API:      POST http://127.0.0.1:8091/spawn
# → 화면과 API가 한 프로세스·한 포트에서 함께 뜬다. 앱을 두 개 띄울 필요 없다.

# 배포 시 허용 토큰 지정
SPAWNPOINT_API_TOKENS="tok-a,tok-b" SPAWNPOINT_DB_PATH=/var/lib/spawnpoint.db python -m app.main
```

환경변수: `SPAWNPOINT_HOST`(127.0.0.1), `SPAWNPOINT_PORT`(8091),
`SPAWNPOINT_DB_PATH`(spawnpoint.db), `SPAWNPOINT_LOG_DIR`(logs, 실행기 로그 디렉토리),
`SPAWNPOINT_API_TOKENS`(미설정 시 인증 비활성화).

## 테스트

```bash
python -m unittest discover -s tests -v
```

저장소 계층은 sqloader(SQLite 백엔드, `requirements.txt`에 `sqloader==0.2.18` 고정)를
쓴다. 나머지 모듈은 Python 표준 라이브러리만 사용한다.
설치: `pip install -r requirements.txt`

## 범위 밖 (설계상 DEFERRED)

- `created` 이후 상태 전이(active/expired): 인스턴스 수명 관리 모듈 담당 (L-0006 §3)
- 물리 DB 엔진 선정, 만료 인스턴스 보관/삭제 정책 (DB-0007 [DEFERRED])
- 본 구현의 참조 저장소는 SQLite이며, 스키마·제약·인덱스는 DB-0007을 그대로 따른다.

## 실행기(Process Runner) 화면

`GET /`로 서빙되는 화면(`runnerview/static/index.html`)은 `spawnpoint/runner.py`의
`ProcessManager`가 제공하는 `/processes*` API에 실제로 연결되어 있다. 화면에서
명령을 입력해 실행하면 실제 OS 서브프로세스가 뜨고, 정지/재시작 버튼은 실제
프로세스 트리를 종료/재기동하며, 로그 패널은 `logs/<id>.log`를 1초 간격으로
폴링해 보여준다. 기본 로컬 실행은 화면을 열자마자 모든 기능을 사용할 수 있으며
별도의 토큰 입력란은 없다.

임의 OS 명령의 run/stop/restart/list, 로그 tail, POSIX killpg / Windows
taskkill /T 종료는 스폰 인스턴스 발급(위 본문)과 무관한 별개 도메인이며 상위
설계 문서(D/P/L/DB 체인)가 아직 없다 — `runner.py`는 그 체인을 따르지 않는
독립 모듈이다.

> 참고: 이전에는 이 화면이 별도 앱(`app/runner_main.py`, 포트 8092)으로 분리돼
> 있었으나, 두 프로세스를 띄울 이유가 없어 메인 서버(`app/main.py`)에 통합했다.
> 그 다음 단계로 화면이 광고하던 기능(실제 실행/정지/재시작/로그)을 실제로
> 연결했다 — 이전까지는 시안(더미 데이터) 뿐이었다.
