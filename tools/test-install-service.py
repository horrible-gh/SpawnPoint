#!/usr/bin/env python3
"""Exercise fresh install, upgrade and rollback without touching host systemd.

Runs an isolated, path-rewritten copy of the shell installer against a mock
systemctl. No root privileges, real services or Go toolchain are required.
"""
import os
import pathlib
import subprocess
import tempfile

SOURCE = pathlib.Path(__file__).with_name('install-service.sh').read_text()


def check(cond, msg):
    if not cond:
        raise AssertionError(msg)


def write_executable(path, content):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content)
    path.chmod(0o755)


def run(cmd, env, ok=True):
    r = subprocess.run(cmd, text=True, capture_output=True, env=env)
    if ok and r.returncode != 0:
        raise AssertionError(f'Unexpected installer failure: {cmd}\n{r.stdout}\n{r.stderr}')
    if not ok and r.returncode == 0:
        raise AssertionError(f'Unexpected installer success: {cmd}')
    return r


with tempfile.TemporaryDirectory(prefix='sp-install-test-') as tmp:
    root = pathlib.Path(tmp)
    repo = root/'repo'
    tools = repo/'tools'
    tools.mkdir(parents=True)
    unit = root/'etc/systemd/system/spawnpoint.service'
    unit.parent.mkdir(parents=True)
    installed = root/'opt/spawnpoint/spawnpoint'
    fakebin = root/'fakebin'
    fakebin.mkdir()
    state = root/'mock-state'
    commands = root/'systemctl-calls'
    state.write_text('inactive')

    # Redirect every protected path into the temporary directory. Ownership
    # changes are excluded only from this sandbox copy so CI needn't run root.
    source = SOURCE
    for orig, dest in [
        ('/etc/systemd/system/spawnpoint.service', str(unit)),
        ('/opt/spawnpoint', str(installed.parent)),
        ('/var/lib/spawnpoint', str(root/'var/lib/spawnpoint')),
        ('/var/log/spawnpoint', str(root/'var/log/spawnpoint')),
    ]:
        source = source.replace(orig, dest)
    source = source.replace('install -d -o "$service_user" -g "$group" -m 0750', 'install -d -m 0750')
    write_executable(tools/'install-service.sh', source)
    write_executable(tools/'dist.sh', '''#!/bin/sh
set -eu
mkdir -p "$(dirname "$0")/../dist/linux64"
printf '#!/bin/sh\\necho %s\\n' "$MOCK_VERSION" > "$(dirname "$0")/../dist/linux64/spawnpoint"
chmod 755 "$(dirname "$0")/../dist/linux64/spawnpoint"
''')
    write_executable(fakebin/'id', '''#!/bin/sh
case "$1" in
  -u) echo 0 ;;
  -gn) echo testgroup ;;
  *) exit 0 ;;
esac
''')
    write_executable(fakebin/'sudo', '''#!/bin/sh
shift; shift; shift
"$@"
''')
    write_executable(fakebin/'systemctl', '''#!/bin/sh
set -eu
printf '%s\\n' "$*" >> "$MOCK_COMMANDS"
case "$1" in
  cat) test -f "$MOCK_UNIT" ;;
  show) echo "{ path=$MOCK_INSTALLED ; argv[]=$MOCK_INSTALLED ; ignore_errors=no ; }" ;;
  is-active) test "$(cat "$MOCK_STATE")" = active ;;
  stop) echo inactive > "$MOCK_STATE" ;;
  start)
    if [ -f "$MOCK_FAIL_ONCE" ]; then
      rm "$MOCK_FAIL_ONCE"
      exit 1
    fi
    echo active > "$MOCK_STATE" ;;
  daemon-reload|enable) : ;;
  *) exit 2 ;;
esac
''')
    env = {**os.environ,
           'PATH': str(fakebin)+':'+os.environ['PATH'],
           'MOCK_STATE': str(state),
           'MOCK_UNIT': str(unit),
           'MOCK_INSTALLED': str(installed),
           'MOCK_COMMANDS': str(commands),
           'MOCK_FAIL_ONCE': str(root/'fail-once'),
           'MOCK_VERSION': 'built-v2'}
    exe1 = root/'v1'
    exe2 = root/'v2'
    exe3 = root/'v3'
    for exe, label in [(exe1,'v1'),(exe2,'v2'),(exe3,'v3')]:
        write_executable(exe, '#!/bin/sh\necho '+label+'\n')
    installer = ['sh',str(tools/'install-service.sh')]

    # Initial registration is still non-disruptive and preserves the unit.
    run(installer+['--user','testuser','--source',str(exe1)],env)
    check(installed.read_bytes()==exe1.read_bytes(),'first install binary')
    check(state.read_text().strip()=='inactive','first install must not autostart')
    original_unit=unit.read_bytes()
    db = root/'var/lib/spawnpoint/spawnpoint.db'
    log = root/'var/log/spawnpoint/proc_test.log'
    db.write_text('persistent database');log.write_text('persistent log')

    # An upgrade while stopped keeps the service stopped.
    run(installer+['--source',str(exe2)],env)
    check(installed.read_bytes()==exe2.read_bytes(),'stopped upgrade binary')
    check(state.read_text().strip()=='inactive','stopped upgrade remains stopped')
    check(unit.read_bytes()==original_unit,'upgrade may not rewrite unit')

    # --start on same binary starts the installed service without replacing it.
    commands.write_text('')
    run(installer+['--source',str(exe2),'--start'],env)
    check(state.read_text().strip()=='active','--start activates service')
    check('stop' not in commands.read_text(),'unchanged binary must not stop service')
    commands.write_text('')
    run(installer+['--source',str(exe2)],env)
    check('stop' not in commands.read_text() and 'start' not in commands.read_text(),'same binary no restart')

    # Hot upgrade changes binary and restarts the previously active service.
    commands.write_text('')
    run(installer+['--source',str(exe3)],env)
    check(installed.read_bytes()==exe3.read_bytes(),'running upgrade binary')
    check(state.read_text().strip()=='active','running upgrade remains active')
    check('stop' in commands.read_text() and 'start' in commands.read_text(),'running upgrade restarts')

    # Failed service restart rolls the previous executable back and recovers.
    (root/'fail-once').write_text('fail')
    fail=run(installer+['--source',str(exe1)],env,ok=False)
    check('restoring previous' in fail.stderr,'rollback explained')
    check(installed.read_bytes()==exe3.read_bytes(),'rollback restores old binary')
    check(state.read_text().strip()=='active','rollback restarts original service')

    # Default invocation builds current checkout automatically and updates it.
    env['MOCK_VERSION']='built-new'
    run(installer,env)
    check(b'built-new' in installed.read_bytes(),'default invocation compiled and installed')
    check(state.read_text().strip()=='active','default invocation restarted service')
    check(unit.read_bytes()==original_unit,'unit preserved through every update')
    check(db.read_text()=='persistent database' and log.read_text()=='persistent log','persistent data untouched')

    # Refuse to replace services with a custom ExecStart.
    unit.write_text(original_unit.decode().replace('ExecStart='+str(installed), 'ExecStart=/bin/other'))
    fail=run(installer+['--source',str(exe1)],env,ok=False)
    check('nonstandard registration' in fail.stderr,'custom ExecStart refused')
    check(b'built-new' in installed.read_bytes(),'nonstandard service binary untouched')

print('PASS: 8 isolated Linux installer flows: first install, stopped/running upgrade, unchanged, --start, rollback, rebuild, custom unit refusal')
