# Browser reconnection verification

The access UI must recover from a temporary outage without reloading the page or replaying a process-changing request. A successful identity or log read alone is not proof of process-state recovery.

## Regressions addressed

- State polling used to stop while an input or dialog had focus. If logs were paused, there was then no request left to detect an outage. Editing now defers rendering, not connection probes. Healthy-editor probes do not replace the editor's database snapshot or revision.
- Background rendering used to retain the entire old log panel. This restored a stale action-status badge even after the new state snapshot cleared it. Only the log body/viewport is retained now; the header is freshly rendered.
- GET requests have a deadline covering both response headers and body reading. Mutating requests are not automatically retried.

## Integration coverage

`tools/test-reconnect-browser.cjs` builds on the actual executable and Chromium, with a disposable database and a local fault-injection proxy. It verifies lost-command recovery without replay, a focused editor with paused logs, stalled headers, a stalled JSON body, three actual server stop/start cycles in the same page, browser offline/online recovery, and preservation of healthy-editor input and revision. The executable's served HTML must exactly match the checked-out source. Reports include the executable SHA-256 and uncaught browser errors.

The `Verify browser reconnection` workflow runs this on Windows and Linux, plus the Node UI regression tests. It retains reports, exact source/served HTML and screenshots for seven days. It does not access any production server or database.

## Deployments versus already-open pages

The HTML and JavaScript are embedded in the executable. Rebuilding/restarting the server does not replace JavaScript already loaded in an existing browser tab. After installing a UI fix, load the updated page once; subsequent restarts are what the no-reload recovery test covers. A missing UI control in a screenshot distinguishes the loaded UI from the source, but does not by itself establish which executable the server is running.
