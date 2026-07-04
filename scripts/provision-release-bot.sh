#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
#
# provision-release-bot.sh — GitHub App bootstrap + zero-click repo attach
#
# Usage:
#   provision-release-bot.sh --bootstrap [--name <app-name>] [--port <port>]
#   provision-release-bot.sh --attach <owner/repo> [--app-id <id>] [--key-file <path>]
#   provision-release-bot.sh --check [<owner/repo>]
#
# Requires: bash >= 3.2, gh (GitHub CLI, authenticated), python3
# Idempotent: safe to re-run; checks existing state before mutating.
set -euo pipefail

# ── constants ────────────────────────────────────────────────────────────────
DEFAULT_APP_NAME="koryph-release-bot"
DEFAULT_PORT=3737
CRED_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/koryph/release-bot"
APP_ID_FILE="${CRED_DIR}/app-id"
KEY_FILE="${CRED_DIR}/private-key.pem"
TIMEOUT=120   # seconds to wait for the browser redirect

# ── helpers ──────────────────────────────────────────────────────────────────
die()  { echo "error: $*" >&2; exit 1; }
info() { echo "▶ $*"; }
ok()   { echo "✓ $*"; }
warn() { echo "! $*" >&2; }

require() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || die "'${cmd}' not found on PATH; install it and try again"
}

# Check for python3 (required for the manifest flow HTTP server and JSON parsing)
require_python3() {
  require python3
}

# Open a URL in the default browser (macOS, Linux, WSL)
open_browser() {
  local url="$1"
  if command -v open >/dev/null 2>&1; then
    open "$url"
  elif command -v xdg-open >/dev/null 2>&1; then
    xdg-open "$url"
  elif command -v wslview >/dev/null 2>&1; then
    wslview "$url"
  else
    echo "  → Please open this URL in your browser:"
    echo "    ${url}"
  fi
}

# Parse owner/repo into separate vars (sets OWNER and REPO globals)
parse_repo() {
  local arg="$1"
  OWNER="${arg%%/*}"
  REPO="${arg##*/}"
  [[ -n "${OWNER}" && -n "${REPO}" && "${OWNER}" != "${REPO}" ]] \
    || die "expected owner/repo, got '${arg}'"
}

# ── --bootstrap ──────────────────────────────────────────────────────────────
cmd_bootstrap() {
  local app_name="${DEFAULT_APP_NAME}"
  local port="${DEFAULT_PORT}"

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --name) app_name="$2"; shift 2 ;;
      --port) port="$2";     shift 2 ;;
      *)      die "unknown bootstrap option: $1" ;;
    esac
  done

  require_python3
  require gh

  # Idempotent: skip if credentials already exist
  if [[ -f "${APP_ID_FILE}" && -f "${KEY_FILE}" ]]; then
    local existing_id
    existing_id=$(cat "${APP_ID_FILE}")
    info "App already bootstrapped (App ID ${existing_id})."
    echo "  Credentials: ${CRED_DIR}"
    echo "  Re-run with a different --name to create a second app,"
    echo "  or delete ${CRED_DIR} to force re-bootstrap."
    return 0
  fi

  mkdir -p "${CRED_DIR}"
  chmod 700 "${CRED_DIR}"

  local redirect_url="http://localhost:${port}/callback"
  local code_file
  code_file=$(mktemp)
  # shellcheck disable=SC2064
  trap "rm -f '${code_file}'" EXIT

  # Build the manifest JSON
  local manifest
  manifest=$(python3 - <<EOF
import json, sys
m = {
  "name": "${app_name}",
  "description": "Koryph release bot — opens Release PRs so checks trigger without the PAT self-approval trap.",
  "url": "https://github.com/koryph/koryph",
  "redirect_url": "${redirect_url}",
  "public": False,
  "default_permissions": {
    "contents": "write",
    "pull_requests": "write"
  },
  "default_events": []
}
print(json.dumps(m))
EOF
)

  # Start a one-shot Python HTTP server that:
  #   GET /         → serves the auto-submit form that POSTs to GitHub
  #   GET /callback → captures the code, writes it to code_file, responds 200
  # Write the server script to a temp file
  local server_script
  server_script=$(mktemp)

  cat > "${server_script}" <<PYSERVER
#!/usr/bin/env python3
import http.server
import urllib.parse
import json
import sys
import os

PORT    = int(sys.argv[1])
CODE_F  = sys.argv[2]
MANIFEST_JSON = sys.argv[3]

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass  # silence default access log

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        params = urllib.parse.parse_qs(parsed.query)

        if parsed.path == '/':
            # Serve the auto-submit form
            body = f"""<!DOCTYPE html>
<html>
<head><title>Creating GitHub App…</title></head>
<body>
<p>Opening GitHub — please confirm the one-click App creation…</p>
<form id="f" action="https://github.com/settings/apps/new" method="post">
  <input type="hidden" name="manifest" value="{MANIFEST_JSON.replace('"', '&quot;')}">
  <input type="submit" value="Create GitHub App">
</form>
<script>document.getElementById('f').submit();</script>
</body></html>"""
            self.send_response(200)
            self.send_header('Content-Type', 'text/html; charset=utf-8')
            self.end_headers()
            self.wfile.write(body.encode())

        elif parsed.path == '/callback':
            code = params.get('code', [''])[0]
            if code:
                with open(CODE_F, 'w') as fh:
                    fh.write(code)
                body = b"<html><body><h2>App created! You may close this tab.</h2></body></html>"
                self.send_response(200)
                self.send_header('Content-Type', 'text/html; charset=utf-8')
                self.end_headers()
                self.wfile.write(body)
                # Signal the server to exit
                self.server.shutdown_requested = True
            else:
                self.send_error(400, "missing code")
        else:
            self.send_error(404)

    def do_POST(self):
        self.send_error(405)

class Server(http.server.HTTPServer):
    shutdown_requested = False

    def serve_forever(self, timeout):
        import select, time
        deadline = time.time() + timeout
        while not self.shutdown_requested:
            remaining = deadline - time.time()
            if remaining <= 0:
                sys.exit(1)
            r, _, _ = select.select([self.socket], [], [], min(remaining, 1.0))
            if r:
                self._handle_request_noblock()

srv = Server(('127.0.0.1', PORT), Handler)
srv.serve_forever(int(sys.argv[4]))
PYSERVER

  # Launch server in background
  python3 "${server_script}" "${port}" "${code_file}" "${manifest}" "${TIMEOUT}" &
  local server_pid=$!
  # shellcheck disable=SC2064
  trap "kill '${server_pid}' 2>/dev/null || true; rm -f '${server_script}' '${code_file}'" EXIT

  # Brief pause to let server start
  sleep 0.5

  info "Starting GitHub App Manifest flow…"
  echo "  App name   : ${app_name}"
  echo "  Redirect   : ${redirect_url}"
  echo ""
  info "Opening browser — GitHub will ask you to click 'Create GitHub App' once."
  open_browser "http://localhost:${port}/"
  echo ""
  info "Waiting for redirect (up to ${TIMEOUT}s)…"

  # Poll for the code file to be populated
  local elapsed=0
  while [[ ! -s "${code_file}" ]]; do
    sleep 1
    elapsed=$((elapsed + 1))
    if [[ ${elapsed} -ge ${TIMEOUT} ]]; then
      kill "${server_pid}" 2>/dev/null || true
      die "Timed out waiting for the GitHub redirect. Did you click 'Create GitHub App' in the browser?"
    fi
  done

  local code
  code=$(cat "${code_file}")
  kill "${server_pid}" 2>/dev/null || true

  ok "Redirect received (code=${code:0:8}…)."
  info "Exchanging manifest code for App credentials…"

  local conversion
  conversion=$(gh api "/app-manifests/${code}/conversions" -X POST) \
    || die "Code exchange failed. Codes are single-use; re-run --bootstrap to get a new code."

  local app_id pem
  app_id=$(python3 -c "import sys,json; d=json.load(sys.stdin); print(d['id'])" <<< "${conversion}")
  pem=$(python3 -c "import sys,json; d=json.load(sys.stdin); print(d['pem'])" <<< "${conversion}")

  [[ -n "${app_id}" && -n "${pem}" ]] || die "Unexpected conversion response — missing id or pem."

  # Persist credentials
  echo "${app_id}" > "${APP_ID_FILE}"
  printf '%s\n' "${pem}" > "${KEY_FILE}"
  chmod 600 "${KEY_FILE}"

  ok "App created! App ID: ${app_id}"
  echo ""
  echo "  Credentials stored in: ${CRED_DIR}"
  echo "  App ID file          : ${APP_ID_FILE}"
  echo "  Private key          : ${KEY_FILE}"
  echo ""
  info "Next step — install the app on your GitHub account:"
  echo ""
  echo "  1. GitHub will open the installation page automatically."
  echo "     If it didn't, go to: https://github.com/settings/apps/${app_name}/installations"
  echo "  2. Click 'Install' and choose 'All repositories' or specific repos."
  echo "  3. Then run:  $(basename "$0") --attach <owner/repo>"
  echo ""
  open_browser "https://github.com/settings/apps/${app_name}/installations"
}

# ── --attach ─────────────────────────────────────────────────────────────────
cmd_attach() {
  local target_repo="${1:-}"
  shift || true
  local app_id_override=""
  local key_file_override=""

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --app-id)   app_id_override="$2";   shift 2 ;;
      --key-file) key_file_override="$2"; shift 2 ;;
      *)          die "unknown attach option: $1" ;;
    esac
  done

  [[ -n "${target_repo}" ]] || die "--attach requires <owner/repo>"
  require_python3
  require gh

  parse_repo "${target_repo}"
  local owner="${OWNER}"
  local repo="${REPO}"

  # Resolve credentials
  local app_id key_path
  if [[ -n "${app_id_override}" ]]; then
    app_id="${app_id_override}"
  elif [[ -f "${APP_ID_FILE}" ]]; then
    app_id=$(cat "${APP_ID_FILE}")
  else
    die "No App ID found. Run --bootstrap first, or pass --app-id."
  fi

  if [[ -n "${key_file_override}" ]]; then
    key_path="${key_file_override}"
  elif [[ -f "${KEY_FILE}" ]]; then
    key_path="${KEY_FILE}"
  else
    die "No private key found. Run --bootstrap first, or pass --key-file."
  fi

  [[ -f "${key_path}" ]] || die "Private key file not found: ${key_path}"

  info "Attaching ${owner}/${repo} to App ID ${app_id}…"

  # ── 1. Resolve installation ID ───────────────────────────────────────────
  info "Resolving installation ID…"
  local iid
  iid=$(gh api "/apps/$(gh api /app --jq '.slug' 2>/dev/null || true)/installations" \
         --jq ".[] | select(.account.login == \"${owner}\") | .id" 2>/dev/null || true)

  # If the slug approach fails (unauthenticated as app), fall back to user installations
  if [[ -z "${iid}" ]]; then
    iid=$(gh api "/user/installations" \
           --jq ".installations[] | select(.account.login == \"${owner}\") | .id" 2>/dev/null || true)
  fi

  if [[ -z "${iid}" ]]; then
    # Try listing installations for the app directly
    iid=$(gh api "/app/installations" \
           --jq ".[] | select(.account.login == \"${owner}\") | .id" 2>/dev/null || true)
  fi

  [[ -n "${iid}" ]] || die "Could not find an installation for '${owner}'. Install the app on that account first:\n  https://github.com/settings/apps"

  ok "Installation ID: ${iid}"

  # ── 2. Resolve repository ID ─────────────────────────────────────────────
  info "Resolving repository ID…"
  local rid
  rid=$(gh api "/repos/${owner}/${repo}" --jq '.id') \
    || die "Repository ${owner}/${repo} not found or not accessible."

  ok "Repository ID: ${rid}"

  # ── 3. Add repository to installation (idempotent) ───────────────────────
  info "Adding ${owner}/${repo} to installation ${iid}…"
  local already
  already=$(gh api "/user/installations/${iid}/repositories" \
             --jq ".repositories[] | select(.id == ${rid}) | .id" 2>/dev/null || true)

  if [[ "${already}" == "${rid}" ]]; then
    ok "Repository already in installation — skipping."
  else
    gh api -X PUT "/user/installations/${iid}/repositories/${rid}" \
      || die "Failed to add ${owner}/${repo} to installation."
    ok "Repository added to installation."
  fi

  # ── 4. Set repository secrets ─────────────────────────────────────────────
  info "Setting RELEASE_BOT_APP_ID secret on ${owner}/${repo}…"
  local existing_app_id_secret
  existing_app_id_secret=$(gh secret list --repo "${owner}/${repo}" \
    --jq '.[].name' 2>/dev/null | grep -x "RELEASE_BOT_APP_ID" || true)

  if [[ "${existing_app_id_secret}" == "RELEASE_BOT_APP_ID" ]]; then
    ok "RELEASE_BOT_APP_ID already set."
  else
    printf '%s' "${app_id}" | gh secret set RELEASE_BOT_APP_ID --repo "${owner}/${repo}" \
      || die "Failed to set RELEASE_BOT_APP_ID secret."
    ok "RELEASE_BOT_APP_ID set."
  fi

  info "Setting RELEASE_BOT_PRIVATE_KEY secret on ${owner}/${repo}…"
  local existing_key_secret
  existing_key_secret=$(gh secret list --repo "${owner}/${repo}" \
    --jq '.[].name' 2>/dev/null | grep -x "RELEASE_BOT_PRIVATE_KEY" || true)

  if [[ "${existing_key_secret}" == "RELEASE_BOT_PRIVATE_KEY" ]]; then
    ok "RELEASE_BOT_PRIVATE_KEY already set."
  else
    gh secret set RELEASE_BOT_PRIVATE_KEY --repo "${owner}/${repo}" < "${key_path}" \
      || die "Failed to set RELEASE_BOT_PRIVATE_KEY secret."
    ok "RELEASE_BOT_PRIVATE_KEY set."
  fi

  # ── 5. Enable Actions PR-approval toggle (IaC capture) ───────────────────
  info "Enabling Actions can_approve_pull_request_reviews on ${owner}/${repo}…"
  local current_setting
  current_setting=$(gh api "/repos/${owner}/${repo}/actions/permissions/workflow" \
    --jq '.can_approve_pull_request_reviews' 2>/dev/null || echo "null")

  if [[ "${current_setting}" == "true" ]]; then
    ok "Actions PR-approval already enabled."
  else
    gh api -X PUT "/repos/${owner}/${repo}/actions/permissions/workflow" \
      -F can_approve_pull_request_reviews=true \
      || die "Failed to enable Actions PR approval. Check that the repo allows Actions write permissions."
    ok "Actions PR-approval enabled."
  fi

  echo ""
  ok "Done! ${owner}/${repo} is ready to use the release bot."
  echo ""
  echo "  The workflow will use 'actions/create-github-app-token' with:"
  echo "    app-id: \${{ secrets.RELEASE_BOT_APP_ID }}"
  echo "    private-key: \${{ secrets.RELEASE_BOT_PRIVATE_KEY }}"
  echo ""
  echo "  Run '$(basename "$0") --check ${owner}/${repo}' to verify the configuration."
}

# ── --check ──────────────────────────────────────────────────────────────────
cmd_check() {
  local target_repo="${1:-}"
  require_python3
  require gh

  local drift=0

  if [[ -n "${target_repo}" ]]; then
    parse_repo "${target_repo}"
    local owner="${OWNER}"
    local repo="${REPO}"

    echo "=== Checking ${owner}/${repo} ==="
    echo ""

    # Check App ID secret
    local app_id_secret
    app_id_secret=$(gh secret list --repo "${owner}/${repo}" \
      --jq '.[].name' 2>/dev/null | grep -x "RELEASE_BOT_APP_ID" || true)
    if [[ "${app_id_secret}" == "RELEASE_BOT_APP_ID" ]]; then
      ok "RELEASE_BOT_APP_ID secret: present"
    else
      warn "RELEASE_BOT_APP_ID secret: MISSING"
      drift=1
    fi

    # Check private key secret
    local key_secret
    key_secret=$(gh secret list --repo "${owner}/${repo}" \
      --jq '.[].name' 2>/dev/null | grep -x "RELEASE_BOT_PRIVATE_KEY" || true)
    if [[ "${key_secret}" == "RELEASE_BOT_PRIVATE_KEY" ]]; then
      ok "RELEASE_BOT_PRIVATE_KEY secret: present"
    else
      warn "RELEASE_BOT_PRIVATE_KEY secret: MISSING"
      drift=1
    fi

    # Check Actions PR-approval toggle
    local pr_approval
    pr_approval=$(gh api "/repos/${owner}/${repo}/actions/permissions/workflow" \
      --jq '.can_approve_pull_request_reviews' 2>/dev/null || echo "false")
    if [[ "${pr_approval}" == "true" ]]; then
      ok "Actions can_approve_pull_request_reviews: enabled"
    else
      warn "Actions can_approve_pull_request_reviews: DISABLED"
      drift=1
    fi

    echo ""
    if [[ ${drift} -eq 0 ]]; then
      ok "No drift detected. ${owner}/${repo} is fully configured."
    else
      warn "Drift detected. Run: $(basename "$0") --attach ${owner}/${repo}"
      return 1
    fi

  else

    # No repo specified — check local bootstrap credentials only
    echo "=== Checking local bootstrap credentials ==="
    echo ""

    if [[ -f "${APP_ID_FILE}" ]]; then
      local app_id
      app_id=$(cat "${APP_ID_FILE}")
      ok "App ID file: present (${app_id})"
    else
      warn "App ID file: MISSING (${APP_ID_FILE})"
      drift=1
    fi

    if [[ -f "${KEY_FILE}" ]]; then
      ok "Private key file: present (${KEY_FILE})"
    else
      warn "Private key file: MISSING (${KEY_FILE})"
      drift=1
    fi

    echo ""
    if [[ ${drift} -eq 0 ]]; then
      ok "Bootstrap credentials present."
      echo "  Run '$(basename "$0") --check <owner/repo>' to check a specific repository."
    else
      warn "Bootstrap credentials incomplete. Run: $(basename "$0") --bootstrap"
      return 1
    fi
  fi
}

# ── usage ─────────────────────────────────────────────────────────────────────
usage() {
  cat <<EOF
provision-release-bot.sh — GitHub App bootstrap + zero-click repo attach

USAGE
  $(basename "$0") --bootstrap [--name <app-name>] [--port <port>]
  $(basename "$0") --attach <owner/repo> [--app-id <id>] [--key-file <path>]
  $(basename "$0") --check [<owner/repo>]

COMMANDS
  --bootstrap   Create the GitHub App (one click in browser) and store
                credentials locally. Safe to re-run — skips if already done.

  --attach      Add a repository to the app installation, set repository
                secrets, and enable Actions PR-approval toggle. Zero clicks.
                Safe to re-run — idempotent on all steps.

  --check       Report configuration drift without mutating anything.
                Without <owner/repo>, checks local bootstrap credentials only.

OPTIONS
  --name NAME   GitHub App name (default: ${DEFAULT_APP_NAME})
  --port PORT   Localhost port for redirect catcher (default: ${DEFAULT_PORT})
  --app-id ID   Override App ID instead of reading from credential store
  --key-file PATH Override private key path instead of reading from credential store

CREDENTIAL STORE
  App ID : ${APP_ID_FILE}
  Key PEM: ${KEY_FILE}

REQUIREMENTS
  bash >= 3.2, gh (authenticated), python3
EOF
}

# ── dispatch ──────────────────────────────────────────────────────────────────
[[ $# -ge 1 ]] || { usage; exit 0; }

cmd="$1"; shift

case "${cmd}" in
  --bootstrap) cmd_bootstrap "$@" ;;
  --attach)    cmd_attach "$@"    ;;
  --check)     cmd_check  "$@"    ;;
  --help|-h)   usage              ;;
  *)           die "unknown command: ${cmd}" ;;
esac
