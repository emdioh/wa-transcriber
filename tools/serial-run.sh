#!/bin/sh
# Run one command on a board over its USB serial console and print the board's
# answer plus the exit status. Written for the BeagleBone Black's ttyACM0
# (root, passwordless); also works on any getty that gives a root shell.
#
#   ./serial-run.sh 'ip -4 addr show eth0'
#   ./serial-run.sh -t 30 'make -C /tmp work'       # longer timeout
#   ./serial-run.sh -b 9600 -d /dev/ttyUSB0 'uname -a'
#
# How it works and why: a CDC-ACM frame written by the board while no process
# has the port open is *dropped by the kernel*, so "start a writer, then open
# the reader" loses the whole answer -- that was the old bug. This script keeps
# a single read-write fd open for the whole session (fd 3): it drains until the
# line is silent, logs in as root if a login prompt is waiting (empty password:
# a bare \r), then sends
#   echo TAG-BEGIN; <command>; echo TAG-END-$?
# and prints only the lines between the markers -- a line that *is* the marker,
# because the tty also echoes the command line back. Exit status is the board
# command's; 2 means no END marker arrived (timeout, shell exited, console
# lost) and the session stays usable for the next call.
#
# The console is shared: kernel messages can land in the middle of the output,
# a late answer to the previous run can arrive first (hence the drain), and a
# booting board is noisy. The markers make the answer easy to cut out.
#
# Gotchas: baud must match getty (115200 everywhere here); on the BBB the
# USB serial console is the gadget's /dev/ttyGS0 on the board side, which the
# host enumerates as ttyACM0 (S02console creates it via configfs, alongside
# getty on ttyS0 / tty1). Replies come back with "\r\r\n" line endings, which
# are normalised; a bare \n does not wake getty, \r does; after `exit` the
# shell is gone and getty needs a moment to reprint its prompt, which the
# login loop waits for. Only one console user at a time: two runs in parallel
# fight over the same tty and both fail.
set -eu

# The gadget re-enumerates on every reboot and the device NUMBER changes
# (ttyACM0 <-> ttyACM1), so default to the most recently created node.
default_dev() {
	ls -1t /dev/ttyACM* /dev/ttyUSB* 2>/dev/null | head -1 || true
}

dev=${SERIAL_DEV:-$(default_dev)}
wait_s=15
baud=115200
while [ $# -gt 0 ]; do
	case $1 in
	-d) dev=$2; shift 2 ;;
	-b) baud=$2; shift 2 ;;
	-t) wait_s=$2; shift 2 ;;
	--) shift; break ;;
	-*) echo "serial-run.sh: unknown option $1" >&2; exit 64 ;;
	*) break ;;
	esac
done
cmd=${1:?usage: serial-run.sh [-d dev] [-b baud] [-t seconds] 'command'}

tag="WAT-$$"

# One fd for the whole session: configure, then talk. stty(1) reopens the port
# (dropping queued input), so it must run before the shell is touched; acting
# on fd 3 keeps it from racing the reads below.
exec 3<>"$dev"
stty -F "$dev" "$baud" raw -echo -echoe -echok -echoctl -echoke -ixon -ixoff \
	-crtscts -hupcl cread clocal min 0 time 1 2>/dev/null || true

# Read what arrives over the next $1 seconds into stdout. A single dd is not
# enough: with VMIN=0,VTIME=1 the tty returns EOF as soon as nothing is
# pending, so dd would exit at once and the caller would never wait for a slow
# command (the whole run once finished in 0.3 s). Poll until the deadline,
# with a quiet gap ending it early only when $2 is "quiet".
read_until() {
	end=$(( $(date +%s) + $1 ))
	while [ "$(date +%s)" -lt "$end" ]; do
		chunk=$(dd bs=4096 count=64 <&3 2>/dev/null | tr -d '\000' || true)
		[ -n "$chunk" ] || continue
		printf '%s' "$chunk"
	done
}

# Read until the line has been silent for a moment: used to clear the tail of
# an earlier run before this one starts.
read_quiet() {
	while :; do
		chunk=$(dd bs=4096 count=64 <&3 2>/dev/null | tr -d '\000' || true)
		[ -n "$chunk" ] || return 0
		printf '%s' "$chunk"
	done
}

log() { printf '%s\n' "$*" >&2; }

# --- bring the console to a shell prompt ------------------------------------
# A dead session and a live shell look the same until something is sent: the
# port is quiet in both cases. Before that, drain until it falls silent: a
# late answer to the *previous* run's command can otherwise land in this one
# and be mistaken for its prompt or its output. Silence (one empty read) is
# the only reliable "nothing is pending" signal on a shared console.
# Late answers to the previous run's command must not be mistaken for this
# one's prompt or output, so wait for silence first.
drained=
i=0
while [ "$i" -lt 6 ]; do
	i=$((i + 1))
	chunk=$(read_until 1)
	[ -n "$chunk" ] || break
	drained="$drained$chunk"
	i=0
done
buf=$drained

# Log in if a login prompt is waiting (or the console is stuck at one).
# getty takes 'root' + \r and, with an empty password, goes straight to a
# shell; if it does ask, the bare \r answers it. This must be check-then-act:
# sending 'root' to a live shell runs it as a command (`-sh: root: not found`).
# The loop retries because a shell that just exited only makes getty reprint
# its prompt a moment later, and a fresh 15 s window would otherwise be wasted.
i=0
while [ "$i" -lt 3 ]; do
	i=$((i + 1))
	# Look only at the tail of the buffer: an earlier prompt from this same
	# session would otherwise satisfy the check forever.
	tail=$(printf '%s' "$buf" | tail -c 120)
	case $tail in
	*'# ') break ;;
	esac
	if ! printf '%s' "$tail" | grep -q 'ogin: *$'; then
		# No prompt yet: a command from an earlier run may still be running.
		# Ctrl-C aborts it (harmless at a prompt, which just reprints), then \r
		# asks for the prompt; \n alone does not wake getty or sh.
		printf '\003' >&3
		printf '\r' >&3
		buf="$buf$(read_until 3)"
		tail=$(printf '%s' "$buf" | tail -c 120)
		case $tail in
		*'# ') break ;;
		esac
		printf '%s' "$tail" | grep -q 'ogin: *$' || continue
	fi
	log "serial-run: logging in as root on $dev"
	printf 'root\r' >&3
	buf=$(read_until 3)
	case $buf in
	*'assword:'*) printf '\r' >&3; buf="$buf$(read_until 3)" ;;
	esac
	buf="$buf$(read_until 1)"
done

case $(printf '%s' "$buf" | tail -c 120) in
*'# '*) : ;;
*) log "serial-run: no shell prompt on $dev (last bytes: $(printf '%s' "$buf" | tail -c 200 | tr -cd '[:print:] '))"; exit 2 ;;
esac

# --- run the command between markers ---------------------------------------
# The board's tty echoes the line we send, so the marker appears twice: once
# in the echo (with the command after it) and once as the shell's own output
# on a line of its own. Only a line that *is* the marker counts, which is why
# the awk below anchors at both ends.
printf 'echo %s-BEGIN; %s; echo %s-END-$?\r' "$tag" "$cmd" "$tag" >&3

# Never collect the answer with $(...): command substitution strips trailing
# newlines and the whole answer collapses into one line, so the markers would
# never line up. Write it to a temp file, where the \r\r\n line endings become
# plain newlines on the way in.
tmp=${TMPDIR:-/tmp}/serial-run.$$
trap 'rm -f "$tmp"' EXIT
read_until "$wait_s" >"$tmp"
tr '\r' '\n' <"$tmp" | awk -v tag="$tag" '
	$0 == tag "-BEGIN" { on = 1; next }
	on && ($0 ~ ("^" tag "-END-[0-9]+$") || index($0, tag "-END-")) {
		rest = $0
		sub(/^.*-END-/, "", rest)
		sub(/[^0-9].*$/, "", rest)
		rc = rest
		on = 0; done = 1; exit
	}
	on && $0 != "" { print }
	END {
		if (done) { print "[exit " rc "]"; exit rc }
		print "[no END marker: command still running, shell exited, or console lost]" > "/dev/stderr"
		exit 2
	}
'
