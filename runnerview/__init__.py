"""Process Runner UI — static view assets.

The main server (app/main.py, spawnpoint.http_api) serves this UI on the same port
alongside the API — it is not a separate process. This package owns the UI HTML
assets and loader ([[page]]).

The actual backend handles run/stop/restart/list operations and log polling via
ProcessManager in runner.py.
"""
