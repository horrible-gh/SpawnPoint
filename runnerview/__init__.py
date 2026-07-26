"""실행기(Process Runner) 화면 — 시안(dx0qx6f7) 정적 뷰 애셋.

메인 서버(app/main.py, spawnpoint.http_api)가 같은 포트에서 이 화면을 함께
서빙한다 — 별도 프로세스가 아니다. 이 패키지는 화면 HTML 애셋과 로더만
소유한다([[page]]).

run/stop/restart/list와 SSE 로그 tail을 처리하는 실제 백엔드는 아직 없다.
지금은 시안과 동일한 정적 화면(더미 데이터)만 제공하며, 실 프로세스
실행·종료·로그 캡처는 전용 설계 체인(D/P/L/DB) 수립 후 후속 작업으로 분리한다.
"""
