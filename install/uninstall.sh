#!/bin/sh
#
# loop-sessions uninstaller.
#
#   curl -fsSL https://sessions.example.com/dl/uninstall.sh | sh
#
# (replace https://sessions.example.com/dl with the base URL your release
# files are served from, the same LOOP_SESSIONS_BASE_URL the installer used.)
#
# This removes the agent and prints every path it deletes. Your captured data is
# left in place by default, and the command to remove that too is printed at the
# end.
#
# That default is deliberate. For anything that watches how you work, being easy
# and obvious to remove is what makes it acceptable to install in the first
# place: someone who suspects they cannot get rid of a tool will not accept it.
# Deleting someone's data without asking would undo that in one step.
#
#   --purge      also delete captured data and configuration
#   --dry-run    show what would be removed, change nothing
#   --force      remove the binary even if harness hooks still reference it
#   --help
#
# POSIX sh only.

set -eu

BIN_DIR="${LOOP_SESSIONS_BIN_DIR:-$HOME/.local/bin}"
BIN_NAME="loop-sessions"
DATA_DIR="$HOME/.loop/sessions"
AGENT_DIR="$HOME/Library/LaunchAgents"
HOOK_MARKER="#loop-sessions"

PURGE=0
DRY_RUN=0
FORCE=0
REMOVED=0

if [ -t 1 ]; then
	B="$(printf '\033[1m')"
	R="$(printf '\033[0m')"
else
	B=""
	R=""
fi

say() { printf '%s\n' "$*"; }
step() { printf '%s==>%s %s\n' "$B" "$R" "$*"; }

die() {
	printf '\n%serror:%s %s\n' "$B" "$R" "$1" >&2
	shift
	for line in "$@"; do
		printf '  %s\n' "$line" >&2
	done
	exit 1
}

usage() {
	sed -n '3,20p' "$0" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
	case "$1" in
	--purge) PURGE=1 ;;
	--dry-run) DRY_RUN=1 ;;
	--force) FORCE=1 ;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "Unknown option \"$1\"." "Run with --help to see what is available." ;;
	esac
	shift
done

# remove deletes a path and reports it, or reports what it would delete.
remove() {
	if [ "$DRY_RUN" = "1" ]; then
		say "    would remove: $1"
		REMOVED=$((REMOVED + 1))
		return 0
	fi
	if rm -rf "$1"; then
		say "    removed: $1"
		REMOVED=$((REMOVED + 1))
	else
		say "    COULD NOT REMOVE: $1"
	fi
}

# settings_file is where the harness keeps hook registrations.
settings_file() {
	if [ -n "${CLAUDE_CONFIG_DIR:-}" ]; then
		printf '%s\n' "${CLAUDE_CONFIG_DIR}/settings.json"
	else
		printf '%s\n' "$HOME/.claude/settings.json"
	fi
}

hooks_present() {
	sf="$(settings_file)"
	[ -f "$sf" ] && grep -q "$HOOK_MARKER" "$sf" 2>/dev/null
}

# ---------------------------------------------------------------- hooks
#
# Hooks come first, and the binary is not deleted while they still reference it.
#
# A hook entry is a command line the harness runs on every lifecycle event. If
# the binary is deleted while the entry remains, every event runs a command that
# does not exist, and the harness treats a failing hook as meaningful. Removing a
# telemetry agent must not degrade someone's editor, so an orphaned hook is
# treated as a blocking condition rather than a footnote.

unregister_hooks() {
	target="${BIN_DIR}/${BIN_NAME}"
	sf="$(settings_file)"

	if ! hooks_present; then
		return 0
	fi

	step "Removing harness hooks from ${sf}"
	if [ "$DRY_RUN" = "1" ]; then
		say "    would run: ${target} uninstall"
		return 0
	fi

	# The agent edits its own registration. Rewriting that JSON from shell is not
	# attempted: a settings file left unparseable can stop the harness starting,
	# which is a far worse outcome than a hook that lingers.
	if [ -x "$target" ]; then
		if "$target" uninstall >/dev/null 2>&1; then
			say "    removed via ${BIN_NAME} uninstall"
		fi
	fi

	if hooks_present; then
		say "    could not remove them automatically"
	fi
}

check_orphan_hooks() {
	sf="$(settings_file)"
	if ! hooks_present; then
		return 0
	fi
	if [ "$FORCE" = "1" ] || [ "$DRY_RUN" = "1" ]; then
		say ""
		say "${B}warning:${R} hook entries remain in ${sf}."
		say "  Remove every line containing ${HOOK_MARKER}, or the harness will try to"
		say "  run a command that no longer exists on each event."
		return 0
	fi
	die "Harness hooks still reference ${BIN_NAME}, so the binary was NOT removed." \
		"" \
		"Deleting it now would leave ${sf} pointing at a missing command," \
		"and the harness runs those hooks on every event." \
		"" \
		"Fix it one of these ways:" \
		"  ${BIN_DIR}/${BIN_NAME} uninstall     remove the hook entries, then re-run this" \
		"  edit ${sf} and delete entries containing ${HOOK_MARKER}" \
		"" \
		"Or re-run with --force to remove the binary anyway and clean up by hand."
}

# ---------------------------------------------------------------- components

remove_launch_agent() {
	if [ ! -d "$AGENT_DIR" ]; then
		return 0
	fi

	# Matched on two patterns rather than one fixed label, so a plist named
	# differently by a future release is still found. find is used instead of two
	# globs because a name matching BOTH patterns would otherwise be processed
	# twice — harmless when deleting, but it made --dry-run over-report, and
	# --dry-run is the output someone reads to decide whether to trust this.
	#
	# The pattern stays specific. A broader one such as *loop*.plist would also
	# match unrelated agents, and this function deletes what it finds.
	plists="$(find "$AGENT_DIR" -maxdepth 1 -type f \
		\( -name '*loop-sessions*.plist' -o -name '*agent-sessions*.plist' \) 2>/dev/null || true)"
	[ -n "$plists" ] || return 0

	found=0
	# Split on newline only, and stay in the current shell: a `find | while read`
	# pipeline would run the loop in a subshell and lose the removal count.
	old_ifs="$IFS"
	IFS="$(printf '\n_')"
	IFS="${IFS%_}"
	for plist in $plists; do
		IFS="$old_ifs"
		[ -e "$plist" ] || continue
		if [ "$found" = "0" ]; then
			step "Background service"
			found=1
		fi

		label="$(basename "$plist" .plist)"
		if [ "$DRY_RUN" = "1" ]; then
			say "    would unload: ${label}"
		else
			# bootout is the modern form; unload is kept for older macOS. Both are
			# best-effort: a service that was not loaded is not an error here.
			launchctl bootout "gui/$(id -u)/${label}" >/dev/null 2>&1 ||
				launchctl unload "$plist" >/dev/null 2>&1 || true
			say "    unloaded: ${label}"
		fi
		remove "$plist"
		IFS="$(printf '\n_')"
		IFS="${IFS%_}"
	done
	IFS="$old_ifs"
}

remove_binary() {
	target="${BIN_DIR}/${BIN_NAME}"
	step "Agent binary"

	if [ -e "$target" ]; then
		remove "$target"
	else
		say "    not present: ${target}"
	fi

	# A copy somewhere else on PATH is reported, never deleted. Removing a file
	# this installer did not put there would be overreach.
	other="$(command -v "$BIN_NAME" 2>/dev/null || true)"
	if [ -n "$other" ] && [ "$other" != "$target" ]; then
		say ""
		say "    note: another ${BIN_NAME} is on your PATH at:"
		say "          ${other}"
		say "    It was not installed by this script and has been left alone."
	fi
}

data_summary() {
	if [ ! -d "$DATA_DIR" ]; then
		printf '%s\n' "none"
		return 0
	fi
	dsize="$(du -sh "$DATA_DIR" 2>/dev/null | awk '{print $1}')"
	dcount="$(find "$DATA_DIR" -type f 2>/dev/null | wc -l | tr -d ' ')"
	printf '%s\n' "${dsize:-unknown} across ${dcount} files"
}

handle_data() {
	if [ ! -d "$DATA_DIR" ]; then
		return 0
	fi

	if [ "$PURGE" = "1" ]; then
		step "Captured data and configuration (--purge)"
		say "    ${DATA_DIR} holds $(data_summary)"
		remove "$DATA_DIR"
		return 0
	fi

	step "Captured data and configuration"
	say "    kept: ${DATA_DIR}"
	say "    ($(data_summary), including anything not yet uploaded)"
}

farewell() {
	say ""
	if [ "$DRY_RUN" = "1" ]; then
		say "${B}Dry run.${R} Nothing was changed. ${REMOVED} path(s) would be removed."
		say ""
		return 0
	fi

	if [ "$REMOVED" = "0" ]; then
		say "${B}Nothing to remove.${R} loop-sessions does not appear to be installed."
		say ""
		return 0
	fi

	say "${B}Removed.${R} ${REMOVED} path(s) deleted, each listed above."
	say ""
	if [ "$PURGE" != "1" ] && [ -d "$DATA_DIR" ]; then
		say "Your captured sessions and configuration are still on this machine."
		say "To delete those too:"
		say ""
		say "  rm -rf ${DATA_DIR}"
		say ""
	fi
}

# ---------------------------------------------------------------- main

say ""
say "${B}loop-sessions uninstaller${R}"
if [ "$DRY_RUN" = "1" ]; then
	say "(dry run: nothing will be changed)"
fi
say ""

unregister_hooks
check_orphan_hooks
remove_launch_agent
remove_binary
handle_data
farewell
