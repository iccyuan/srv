# livegen.sh -- deterministic, SELF-LIMITING data generator for the
# srv MCP live test suite (go/internal/mcp/mcp_live_test.go).
#
# It is NOT run standalone. The test concatenates this file with a
# single `livegen <args>` call and ships the whole thing as the body
# of a detached job (sshx RunDetached -> `setsid nohup bash -c ...`),
# so every generated process lives in its OWN process group.
#
# SAFETY CONTRACT (this file caused a prior incident; keep it true):
#   * There is NO unbounded loop anywhere. Every loop has an explicit
#     integer ceiling AND is additionally bounded by the watchdog.
#   * A watchdog hard-kills THIS job's process group after MAXLIFE
#     seconds no matter what -- a crashed/killed test can never leave
#     a runaway behind. `kill 0` is contained to the setsid group, so
#     it can never touch the test runner or any sibling job.
#   * Every numeric argument is re-clamped here (defense in depth):
#     even a buggy caller cannot produce a runaway or a huge file.
#
# Usage: livegen MODE TOKEN PATH WARMUP COUNT INTERVAL MAXLIFE
#   MODE     lines | idle
#   TOKEN    unique marker prefixed on every emitted line
#   PATH     file to append to (lines mode)
#   WARMUP   seconds to wait before the first line (lines mode)
#   COUNT    number of lines to emit (lines mode)        [clamp 0..1000]
#   INTERVAL seconds between lines (lines mode)          [clamp 0..10]
#   MAXLIFE  hard upper bound on total runtime, all modes [clamp 1..300]
livegen() {
  set -u
  mode=${1:?mode}; token=${2:?token}; path=${3:?path}
  warmup=${4:-0}; count=${5:-1}; interval=${6:-1}; maxlife=${7:-60}

  _clamp() { # value lo hi -> clamped value (non-numeric -> lo)
    v=$1; lo=$2; hi=$3
    case $v in '' | *[!0-9]*) v=$lo ;; esac
    [ "$v" -lt "$lo" ] && v=$lo
    [ "$v" -gt "$hi" ] && v=$hi
    printf '%s' "$v"
  }
  warmup=$(_clamp "$warmup" 0 60)
  count=$(_clamp "$count" 0 1000)
  interval=$(_clamp "$interval" 0 10)
  maxlife=$(_clamp "$maxlife" 1 300)

  # Hard watchdog. Runs under setsid (the detach wrapper), so process
  # group 0 == THIS job's group only: signalling it cannot reach the
  # test runner or any other job. Fires only as a backstop -- the
  # loops below already terminate on their own well before MAXLIFE.
  ( sleep "$maxlife"; kill -TERM 0 2>/dev/null; sleep 2; kill -KILL 0 2>/dev/null ) &
  _wd=$!
  _stop_wd() { kill "$_wd" 2>/dev/null; }

  case $mode in
  lines)
    [ "$warmup" -gt 0 ] && sleep "$warmup"
    i=1
    while [ "$i" -le "$count" ]; do
      printf '%s %d %s\n' "$token" "$i" "$(date +%s)" >>"$path"
      i=$((i + 1))
      if [ "$i" -le "$count" ] && [ "$interval" -gt 0 ]; then
        sleep "$interval"
      fi
    done
    ;;
  idle)
    # Bystander / victim job for the kill tests. Sleeps in <=5s
    # slices (bounded by maxlife) so a TERM/KILL lands promptly and
    # the loop can never outlive the watchdog.
    slept=0
    while [ "$slept" -lt "$maxlife" ]; do
      sleep 5
      slept=$((slept + 5))
    done
    ;;
  *)
    echo "livegen: unknown mode '$mode'" >&2
    _stop_wd
    return 2
    ;;
  esac

  _stop_wd
  return 0
}
