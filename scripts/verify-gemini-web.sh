#!/bin/sh
# verify-gemini-web.sh - end-to-end acceptance checks for Gemini Web
# (Google Connected Apps) access to an AMG deployment.
#
# The script only reads public endpoints plus /mcp with the owner token. It
# never mutates server state, never prints the token, and removes its temporary
# files on exit. POSIX sh; requires curl and python3.
set -eu
umask 077

BASE_URL="https://agent.cosymart.top"
if [ -n "${HOME:-}" ]; then
	TOKEN_FILE="$HOME/.config/agent-mcp-go/mcp-token"
else
	TOKEN_FILE=""
fi
ORIGIN="https://gemini.google.com"
DENIED_ORIGIN="https://evil.example"

usage() {
	cat <<'EOF'
Usage: scripts/verify-gemini-web.sh [options]

Run nine end-to-end acceptance checks for Gemini Web (Google Connected Apps)
access to an AMG deployment. Each check prints PASS or FAIL; any failure exits
with status 1. The script never prints the token.

Options:
  --base-url URL         deployment origin, without /mcp
                         (default https://agent.cosymart.top)
  --token-file PATH      owner MCP token file
                         (default $HOME/.config/agent-mcp-go/mcp-token)
  --origin ORIGIN        browser origin that must be whitelisted
                         (default https://gemini.google.com)
  --denied-origin ORIGIN origin expected to be rejected
                         (default https://evil.example)
  -h, --help             show this help

Examples:
  scripts/verify-gemini-web.sh
  scripts/verify-gemini-web.sh --base-url http://127.0.0.1:3201 \
      --token-file /tmp/amg-token --origin https://gemini.google.com
EOF
}

die() {
	printf 'error: %s\n' "$*" >&2
	exit 2
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--base-url)
			[ "$#" -ge 2 ] || die "--base-url requires a value"
			BASE_URL=$2
			shift 2
			;;
		--base-url=*)
			BASE_URL=${1#*=}
			shift
			;;
		--token-file)
			[ "$#" -ge 2 ] || die "--token-file requires a value"
			TOKEN_FILE=$2
			shift 2
			;;
		--token-file=*)
			TOKEN_FILE=${1#*=}
			shift
			;;
		--origin)
			[ "$#" -ge 2 ] || die "--origin requires a value"
			ORIGIN=$2
			shift 2
			;;
		--origin=*)
			ORIGIN=${1#*=}
			shift
			;;
		--denied-origin)
			[ "$#" -ge 2 ] || die "--denied-origin requires a value"
			DENIED_ORIGIN=$2
			shift 2
			;;
		--denied-origin=*)
			DENIED_ORIGIN=${1#*=}
			shift
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			die "unknown argument: $1 (use --help)"
			;;
	esac
done

BASE_URL=${BASE_URL%/}
case "$BASE_URL" in
	http://?*|https://?*) ;;
	*) die "--base-url must start with http:// or https://" ;;
esac
case "$BASE_URL" in
	*/mcp|*/mcp/) die "--base-url must not include the /mcp path" ;;
esac
[ -n "$TOKEN_FILE" ] || die "HOME is unset; pass --token-file explicitly"
[ -r "$TOKEN_FILE" ] || die "token file is not readable: $TOKEN_FILE"
TOKEN=$(tr -d '[:space:]' < "$TOKEN_FILE")
[ "${#TOKEN}" -ge 32 ] || die "token file has fewer than 32 non-whitespace characters: $TOKEN_FILE"

TMP=$(mktemp -d "${TMPDIR:-/tmp}/verify-gemini-web.XXXXXX")
cleanup() {
	if [ -n "${TMP:-}" ]; then
		rm -rf "$TMP"
	fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

# Keep the credential out of argv: curl reads the Authorization header from a
# private file, and the shell variable is discarded afterwards.
printf 'Authorization: Bearer %s\n' "$TOKEN" > "$TMP/auth.hdr"
unset TOKEN

cat > "$TMP/hdr.py" <<'PY'
import sys

path, want = sys.argv[1], sys.argv[2].lower()
with open(path, 'r', encoding='latin-1') as handle:
    for line in handle:
        name, sep, value = line.partition(':')
        if sep and name.strip().lower() == want:
            print(value.strip())
            break
PY

cat > "$TMP/metadata.py" <<'PY'
import json
import sys

try:
    with open(sys.argv[1], 'r', encoding='utf-8') as handle:
        data = json.load(handle)
except (OSError, ValueError) as exc:
    sys.stderr.write('not valid JSON: %s\n' % exc)
    sys.exit(1)

problems = []
for key in ('authorization_endpoint', 'token_endpoint', 'registration_endpoint'):
    value = data.get(key)
    if not isinstance(value, str) or not value:
        problems.append('missing ' + key)
if 'code' not in (data.get('response_types_supported') or []):
    problems.append('response_types_supported lacks code')
if 'S256' not in (data.get('code_challenge_methods_supported') or []):
    problems.append('code_challenge_methods_supported lacks S256')
if problems:
    sys.stderr.write('; '.join(problems) + '\n')
    sys.exit(1)
print('code flow and PKCE S256 advertised')
PY

cat > "$TMP/rpc.py" <<'PY'
import json
import sys


def load_message(path):
    with open(path, 'rb') as handle:
        raw = handle.read().decode('utf-8', 'replace')
    candidates = []
    stripped = raw.strip()
    if stripped:
        candidates.append(stripped)
        for line in stripped.splitlines():
            line = line.strip()
            if line.startswith('data:'):
                candidates.append(line[len('data:'):].strip())
    for candidate in candidates:
        try:
            message = json.loads(candidate)
        except ValueError:
            continue
        if isinstance(message, dict) and ('result' in message or 'error' in message):
            return message
    return None


def fail(message):
    sys.stderr.write(message + '\n')
    sys.exit(1)


path, mode = sys.argv[1], sys.argv[2]
message = load_message(path)
if message is None:
    fail('response is neither MCP JSON nor SSE JSON-RPC data')
if 'error' in message:
    error = message.get('error') or {}
    fail('JSON-RPC error %s: %s' % (error.get('code'), error.get('message')))
result = message.get('result') or {}
if mode == 'init':
    version = result.get('protocolVersion') or ''
    if not version:
        fail('initialize result has no protocolVersion')
    print(version)
elif mode == 'tools':
    tools = result.get('tools') or []
    hints = ('readOnlyHint', 'destructiveHint', 'openWorldHint', 'idempotentHint')
    complete = 0
    missing_title = 0
    for tool in tools:
        annotations = tool.get('annotations')
        if isinstance(annotations, dict) and all(hint in annotations for hint in hints):
            complete += 1
        title = tool.get('title')
        if not isinstance(title, str) or not title.strip():
            missing_title += 1
    print('%d %d %d' % (len(tools), complete, missing_title))
else:
    fail('unknown parse mode: %s' % mode)
PY

TOTAL=9
CURRENT=0
FAILS=0

pass_check() {
	CURRENT=$((CURRENT + 1))
	printf 'PASS [%d/%d] %s\n' "$CURRENT" "$TOTAL" "$*"
}

fail_check() {
	CURRENT=$((CURRENT + 1))
	FAILS=$((FAILS + 1))
	printf 'FAIL [%d/%d] %s\n' "$CURRENT" "$TOTAL" "$*"
}

# request runs curl and leaves STATUS, CURL_RC, $TMP/body and $TMP/hdr behind.
request() {
	: > "$TMP/body"
	: > "$TMP/hdr"
	set +e
	STATUS=$(curl -sS --connect-timeout 5 --max-time 30 \
		-o "$TMP/body" -D "$TMP/hdr" -w '%{http_code}' "$@")
	CURL_RC=$?
	set -e
}

header_value() {
	python3 "$TMP/hdr.py" "$TMP/hdr" "$1"
}

printf 'Gemini Web acceptance checks: base_url=%s origin=%s token_file=%s\n' "$BASE_URL" "$ORIGIN" "$TOKEN_FILE"
printf 'token loaded; not printed in this output\n\n'

# 1. HTTP availability.
request "$BASE_URL/healthz"
if [ "$STATUS" = "200" ]; then
	pass_check "GET /healthz status=$STATUS"
else
	fail_check "GET /healthz status=$STATUS curl_rc=$CURL_RC (expected 200)"
fi

# 2. Authorization server metadata (RFC 8414) with the required OAuth fields.
request "$BASE_URL/.well-known/oauth-authorization-server"
if [ "$STATUS" = "200" ] && DETAIL=$(python3 "$TMP/metadata.py" "$TMP/body" 2>"$TMP/py.err"); then
	pass_check "GET /.well-known/oauth-authorization-server status=200 ($DETAIL)"
elif [ "$STATUS" != "200" ]; then
	fail_check "GET /.well-known/oauth-authorization-server status=$STATUS curl_rc=$CURL_RC (expected 200)"
else
	fail_check "GET /.well-known/oauth-authorization-server status=200 but metadata invalid: $(cat "$TMP/py.err")"
fi

# 3. The /mcp suffix metadata variant that hosted clients probe.
request "$BASE_URL/.well-known/oauth-authorization-server/mcp"
if [ "$STATUS" = "200" ]; then
	pass_check "GET /.well-known/oauth-authorization-server/mcp status=$STATUS"
else
	fail_check "GET /.well-known/oauth-authorization-server/mcp status=$STATUS curl_rc=$CURL_RC (expected 200)"
fi

# 4. Protected resource metadata (RFC 9728), plain and /mcp variants.
request "$BASE_URL/.well-known/oauth-protected-resource"
RESOURCE_STATUS=$STATUS
request "$BASE_URL/.well-known/oauth-protected-resource/mcp"
RESOURCE_MCP_STATUS=$STATUS
if [ "$RESOURCE_STATUS" = "200" ] && [ "$RESOURCE_MCP_STATUS" = "200" ]; then
	pass_check "GET /.well-known/oauth-protected-resource status=$RESOURCE_STATUS; /mcp status=$RESOURCE_MCP_STATUS"
else
	fail_check "GET /.well-known/oauth-protected-resource status=$RESOURCE_STATUS; /mcp status=$RESOURCE_MCP_STATUS (expected 200 and 200)"
fi

# 5. Anonymous /mcp must answer 401 with a discovery challenge.
request -X POST \
	-H 'Content-Type: application/json' \
	-H 'Accept: application/json, text/event-stream' \
	--data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"verify-gemini-web","version":"1"}}}' \
	"$BASE_URL/mcp"
CHALLENGE=$(header_value 'www-authenticate')
case "$CHALLENGE" in
	*resource_metadata=*) HAS_CHALLENGE=1 ;;
	*) HAS_CHALLENGE=0 ;;
esac
if [ "$STATUS" = "401" ] && [ "$HAS_CHALLENGE" = "1" ]; then
	pass_check "anonymous POST /mcp status=401 WWW-Authenticate includes resource_metadata="
else
	fail_check "anonymous POST /mcp status=$STATUS curl_rc=$CURL_RC WWW-Authenticate=${CHALLENGE:-<absent>} (expected 401 with resource_metadata=)"
fi

# 6. Authenticated initialize: negotiate a protocol version and a session.
request -X POST \
	-H 'Content-Type: application/json' \
	-H 'Accept: application/json, text/event-stream' \
	-H @"$TMP/auth.hdr" \
	--data '{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"verify-gemini-web","version":"1"}}}' \
	"$BASE_URL/mcp"
PROTO=""
INIT_OK=0
if [ "$STATUS" = "200" ]; then
	if PROTO=$(python3 "$TMP/rpc.py" "$TMP/body" init 2>"$TMP/py.err"); then
		INIT_OK=1
	fi
fi
SESSION=$(header_value 'mcp-session-id')
if [ "$INIT_OK" = "1" ] && [ -n "$SESSION" ]; then
	pass_check "initialize status=200 protocolVersion=$PROTO Mcp-Session-Id=present"
elif [ "$INIT_OK" = "1" ]; then
	fail_check "initialize protocolVersion=$PROTO but Mcp-Session-Id is missing (expected a session)"
else
	fail_check "POST /mcp initialize status=$STATUS curl_rc=$CURL_RC: $(cat "$TMP/py.err")"
fi
[ -n "$PROTO" ] || PROTO=2025-11-25

# 7. tools/list inside the initialize session: 40 tools, full annotations,
# full titles.
if [ -z "$SESSION" ]; then
	fail_check "tools/list not run: initialize returned no Mcp-Session-Id"
else
	request -X POST \
		-H 'Content-Type: application/json' \
		-H 'Accept: application/json, text/event-stream' \
		-H @"$TMP/auth.hdr" \
		-H "Mcp-Session-Id: $SESSION" \
		-H "MCP-Protocol-Version: $PROTO" \
		--data '{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}' \
		"$BASE_URL/mcp"
	if [ "$STATUS" != "200" ]; then
		fail_check "tools/list status=$STATUS curl_rc=$CURL_RC (expected 200)"
	elif COUNTS=$(python3 "$TMP/rpc.py" "$TMP/body" tools 2>"$TMP/py.err"); then
		set -- $COUNTS
		TOOL_COUNT=$1
		ANNOTATION_COMPLETE=$2
		MISSING_TITLE=$3
		if [ "$TOOL_COUNT" = "40" ] && [ "$ANNOTATION_COMPLETE" = "40" ] && [ "$MISSING_TITLE" = "0" ]; then
			pass_check "tools/list tools=$TOOL_COUNT annotation_complete=$ANNOTATION_COMPLETE missing_title=$MISSING_TITLE"
		else
			fail_check "tools/list tools=$TOOL_COUNT annotation_complete=$ANNOTATION_COMPLETE missing_title=$MISSING_TITLE (expected 40/40/0)"
		fi
	else
		fail_check "tools/list status=$STATUS but response invalid: $(cat "$TMP/py.err")"
	fi
fi

# 8. Stateless endpoint must not offer a standalone GET/SSE stream.
request -H @"$TMP/auth.hdr" \
	-H 'Accept: application/json, text/event-stream' \
	"$BASE_URL/mcp/stateless"
if [ "$STATUS" = "405" ]; then
	pass_check "GET /mcp/stateless status=405 (standalone GET/SSE unsupported)"
else
	fail_check "GET /mcp/stateless status=$STATUS curl_rc=$CURL_RC (expected 405)"
fi

# 9. Browser CORS: whitelisted preflight succeeds, unknown origin is denied.
request -X OPTIONS \
	-H "Origin: $ORIGIN" \
	-H 'Access-Control-Request-Method: POST' \
	-H 'Access-Control-Request-Headers: authorization, content-type' \
	"$BASE_URL/mcp"
PREFLIGHT_STATUS=$STATUS
PREFLIGHT_ACAO=$(header_value 'access-control-allow-origin')
request -X OPTIONS \
	-H "Origin: $DENIED_ORIGIN" \
	-H 'Access-Control-Request-Method: POST' \
	"$BASE_URL/mcp"
DENIED_STATUS=$STATUS
DENIED_ACAO=$(header_value 'access-control-allow-origin')
if [ "$PREFLIGHT_STATUS" = "204" ] && { [ "$PREFLIGHT_ACAO" = "$ORIGIN" ] || [ "$PREFLIGHT_ACAO" = "*" ]; } && [ "$DENIED_STATUS" = "403" ] && [ -z "$DENIED_ACAO" ]; then
	pass_check "OPTIONS /mcp origin=$ORIGIN status=204 ACAO=$PREFLIGHT_ACAO; origin=$DENIED_ORIGIN status=403 ACAO=<none>"
else
	fail_check "OPTIONS /mcp origin=$ORIGIN status=$PREFLIGHT_STATUS ACAO=${PREFLIGHT_ACAO:-<none>}; origin=$DENIED_ORIGIN status=$DENIED_STATUS ACAO=${DENIED_ACAO:-<none>} (expected 204 with ACAO=$ORIGIN and 403 without ACAO)"
fi

printf '\n'
if [ "$FAILS" -eq 0 ]; then
	printf 'ALL PASS: %d/%d checks passed (base_url=%s)\n' "$CURRENT" "$TOTAL" "$BASE_URL"
	exit 0
fi
printf 'FAILED: %d/%d checks failed (base_url=%s)\n' "$FAILS" "$TOTAL" "$BASE_URL"
exit 1
