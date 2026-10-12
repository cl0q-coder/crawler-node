#!/bin/sh
# load-env.sh — fail-fast environment loader for the private overlay.
# Source this before starting node services:
#     . ./load-env.sh
# Refuses (non-zero exit, loud message) when a required variable is unset,
# empty, or still holds an obvious placeholder. A misconfigured env must
# never silently boot the node into an insecure or wrong-database state.

OVERLAY_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
ENV_FILE="${OVERLAY_DIR}/.env"

if [ ! -f "$ENV_FILE" ]; then
	echo "load-env.sh: missing $ENV_FILE — copy env.example to .env and fill in real values" >&2
	return 1 2>/dev/null || exit 1
fi

# shellcheck disable=SC1090
. "$ENV_FILE"

fail=0
require_unset_or_placeholder() {
	name="$1"
	eval "val=\${$name:-}"
	case "$val" in
		""|*CHANGEME*|*example.invalid*)
			echo "load-env.sh: refusing — $name is unset or still a placeholder" >&2
			fail=1
			;;
	esac
}

require_unset_or_placeholder CL0Q_DB_DSN
require_unset_or_placeholder CL0Q_OPERATOR_CONTACT

# Strict boolean: a typo like "ture" must not silently downgrade to http.
case "${CL0Q_NODE_ALLOW_HTTP:-0}" in
	0|1) ;;
	*)
		echo "load-env.sh: refusing — CL0Q_NODE_ALLOW_HTTP must be 0 or 1, got '${CL0Q_NODE_ALLOW_HTTP}'" >&2
		fail=1
		;;
esac

if [ "$fail" -ne 0 ]; then
	echo "load-env.sh: environment NOT loaded — fix the values above" >&2
	return 1 2>/dev/null || exit 1
fi

echo "load-env.sh: environment OK (CL0Q_DB_DSN and CL0Q_OPERATOR_CONTACT set, no placeholders)"
