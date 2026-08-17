#!/bin/sh
# rancher CLI installer. POSIX sh. Canonical invocation: README "Install".
set -eu

REPO="rancher/rancher-cli"
TAG_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.(0|[1-9][0-9]*))?$'

if [ -n "${RANCHER_CLI_INSTALL_BASE_URL:-}" ]; then
  BASE_URL="$RANCHER_CLI_INSTALL_BASE_URL"
  API_URL="$RANCHER_CLI_INSTALL_BASE_URL"
else
  BASE_URL="https://github.com"
  API_URL="https://api.github.com"
fi

QUIET=""
DRY_RUN=""
WANT_PRE=""
REQ_VERSION="${RANCHER_CLI_VERSION:-}"
BIN_DIR="${RANCHER_CLI_INSTALL_BIN_DIR:-}"
REQUIRE_ATTESTATION=""
TOKEN="${GH_TOKEN:-${GITHUB_TOKEN:-}}"

# cleanup() reads these before they are assigned by any phase; initialize
# them here so the trap fires safely even on very early exits.
WORK=""
LOCK=""
STAGING_DIR=""

# Asset ids resolved by fetch_asset_ids() and consumed by download().
# Empty until fetch_asset_ids runs; download() uses them only when TOKEN is set.
CLI_ASSET_ID=""
HELPER_ASSET_ID=""
SHA256SUMS_ASSET_ID=""

# METADATA_FAIL_CODE: HTTP code from a failed fetch_asset_ids() metadata GET.
# Set when TOKEN is non-empty but the tags endpoint returns non-200; consulted
# by download()'s browser-URL error path to produce an enriched message.
METADATA_FAIL_CODE=""

# REL_JSON: winning release JSON cached by resolve_pre_tag() so
# fetch_asset_ids() can skip a redundant GET on the --pre path.
REL_JSON=""

err() { printf 'error: %s\n' "$*" >&2; exit 1; }

# physical_dir <dir>: prints the physically-resolved directory (., .., and
# symlinked segments resolved) or nothing when the directory is unreachable.
# Callers treat empty output as "cannot resolve", never as an error.
physical_dir() {
  cd "$1" 2>/dev/null && pwd -P
}
warn() { printf 'warning: %s\n' "$*" >&2; }
note() { [ -n "$QUIET" ] || printf '%s\n' "$*"; }

usage() {
  cat <<'EOF'
usage: install.sh [--version <tag>] [--pre] [--bin-dir <dir>]
                  [--verify-attestation] [--dry-run] [-q]
EOF
  exit 1
}

# curl_authed <extra-header|""> <curl-args...>: single owner of the
# token→curl channel. The token travels via --config on stdin — never in
# argv. Every tokened request in this script calls this function.
# Untokened requests (anonymous fetch branches, CDN leg of fetch_asset)
# call curl directly.
curl_authed() {
  _extra_hdr="$1"; shift
  {
    printf 'header = "Authorization: Bearer %s"\n' "$TOKEN"
    if [ -n "$_extra_hdr" ]; then printf 'header = "%s"\n' "$_extra_hdr"; fi
  } | curl --config - --proto '=https' --tlsv1.2 "$@"
}

# fetch <url> [output]: curl with the token (if any) passed via --config on
# stdin so it never appears in argv or logs.
fetch() {
  _url="$1"; _out="${2:-}"
  if [ -n "$TOKEN" ]; then
    if [ -n "$_out" ]; then
      curl_authed "" -fsSL -o "$_out" "$_url"
    else
      curl_authed "" -fsSL "$_url"
    fi
  else
    if [ -n "$_out" ]; then
      curl --proto '=https' --tlsv1.2 -fsSL -o "$_out" "$_url"
    else
      curl --proto '=https' --tlsv1.2 -fsSL "$_url"
    fi
  fi
}

# fetch_asset <id> <output> <name>: downloads a release asset via the GitHub
# API asset endpoint. TOKEN must be non-empty.
#
# Single tokened request without -L writes the body to "$_out" and captures
# the HTTP code and redirect Location via -w. On 200 the file already holds
# the asset — done, one request. On 302/301 (real GitHub signed CDN redirect)
# the small redirect body in "$_out" is overwritten by a plain untokened fetch
# of the Location URL; the signed URL is self-authenticating and the token
# must never leave $API_URL. Security property: identical to the previous
# two-step probe; direct-200 case (stub, internal mirrors) now makes exactly
# one request instead of two.
fetch_asset() {
  _id="$1"; _out="$2"; _name="$3"
  _probe_rc=0
  _probe="$(curl_authed "Accept: application/octet-stream" -sS \
    -o "$_out" -w '%{http_code} %{redirect_url}' \
    "$API_URL/repos/$REPO/releases/assets/$_id")" || _probe_rc=$?
  [ "$_probe_rc" -eq 0 ] || err "download failed: $_name (asset id $_id)"
  _code="${_probe%% *}"
  _location="${_probe#* }"
  case "$_code" in
    200)
      # Body already written to $_out by the tokened request above.
      ;;
    30[12])
      # Redirect to signed CDN URL: overwrite $_out without token.
      [ -n "$_location" ] || err "download failed: $_name (asset id $_id, redirect with no Location)"
      curl --proto '=https' --tlsv1.2 -fsS \
        -o "$_out" "$_location" || \
        err "download failed: $_name (asset id $_id, CDN fetch)"
      ;;
    *)
      err "download failed: $_name (asset id $_id, HTTP $_code)"
      ;;
  esac
}

# fetch_asset_ids: when TOKEN is set, resolves the numeric GitHub asset ids
# for the three assets download() needs.
#
# Source: uses REL_JSON (cached by resolve_pre_tag on the --pre path) when
# available; otherwise fetches via $API_URL/repos/$REPO/releases/tags/$TAG and
# captures the HTTP code so failures can be diagnosed in the download() error.
#
# On metadata fetch failure: stores the code in METADATA_FAIL_CODE, warns to
# stderr, and returns 0 so download() falls back to browser URLs.  Only a
# metadata-success + missing-asset is a broken release.
#
# Extraction is parser-first (jq → python3, one invocation each, three output
# lines) with a single-pass bounded-awk fallback.  The awk stops at the first
# "body": line: GitHub places the body field after assets (verified live), so
# free-text in release notes is never parsed as asset metadata.
fetch_asset_ids() {
  [ -n "$TOKEN" ] || return 0
  if [ -n "$REL_JSON" ]; then
    # --pre path: resolve_pre_tag cached the winning release JSON from the
    # listing page; skip the redundant tags-endpoint round trip.
    _rel_json="$REL_JSON"
  else
    # Capture body and HTTP code in one variable — no temp file.
    # curl appends '\n<code>' after the body via -w; split on the last newline:
    #   _meta_code = everything after the final newline (the 3-digit status)
    #   _meta_body = everything before it (the JSON response body)
    # Command substitution strips trailing newlines, but the code (pure digits)
    # never ends with one, so the separator newline is always preserved.
    _meta_rc=0
    _meta_resp="$(curl_authed "" -sS -L \
      -w '\n%{http_code}' \
      "$API_URL/repos/$REPO/releases/tags/$TAG")" || _meta_rc=$?
    _meta_code="${_meta_resp##*
}"
    _meta_body="${_meta_resp%
*}"
    if [ "$_meta_rc" -ne 0 ] || [ "$_meta_code" != "200" ]; then
      METADATA_FAIL_CODE="${_meta_code:-error}"
      warn "release metadata not readable with this token (HTTP ${_meta_code:-error}) — falling back to direct download"
      return 0
    fi
    _rel_json="$_meta_body"
  fi
  _used_awk=""
  if command -v jq >/dev/null 2>&1; then
    # One jq invocation emitting three lines: cli id, helper id, sums id.
    { IFS= read -r CLI_ASSET_ID || :; IFS= read -r HELPER_ASSET_ID || :; IFS= read -r SHA256SUMS_ASSET_ID || :; } <<EOF
$(printf '%s' "$_rel_json" | jq -r \
  --arg c "$CLI_ASSET" --arg h "$HELPER_ASSET" \
  '(.assets//[]) | ((map(select(.name==$c))[0].id//""),(map(select(.name==$h))[0].id//""),(map(select(.name=="SHA256SUMS"))[0].id//""))')
EOF
  elif python3 -c 'pass' >/dev/null 2>&1; then
    # One python3 invocation emitting three lines: cli id, helper id, sums id.
    { IFS= read -r CLI_ASSET_ID || :; IFS= read -r HELPER_ASSET_ID || :; IFS= read -r SHA256SUMS_ASSET_ID || :; } <<EOF
$(printf '%s' "$_rel_json" | python3 -c \
  'import json,sys; a=json.load(sys.stdin).get("assets",[]); ns=[sys.argv[1],sys.argv[2],"SHA256SUMS"]; [print(next((str(x["id"]) for x in a if x["name"]==n),"")) for n in ns]' \
  "$CLI_ASSET" "$HELPER_ASSET")
EOF
  else
    _used_awk=1
    # Single-pass awk bounded by the "body" field: stops before free-text
    # release notes so adversarial content there cannot spoof an asset id.
    # The END block executes even on an early exit, printing all three results.
    _ids="$(printf '%s' "$_rel_json" | awk \
      -v c="$CLI_ASSET" -v h="$HELPER_ASSET" -v s="SHA256SUMS" '
        /"body":/ { exit }
        /"url":/ && /\/releases\/assets\// {
          u=$0; sub(/.*\/assets\//,"",u); sub(/".*/,"",u)
        }
        /"name":/ {
          n=$0; sub(/.*"name": *"/,"",n); sub(/".*/,"",n)
          if (n==c) ci=u
          if (n==h) hi=u
          if (n==s) si=u
        }
        END { print ci " " hi " " si }')"
    CLI_ASSET_ID="${_ids%% *}"
    _ids_rest="${_ids#* }"
    HELPER_ASSET_ID="${_ids_rest%% *}"
    SHA256SUMS_ASSET_ID="${_ids_rest##* }"
  fi
  _awk_note=""
  [ -z "${_used_awk:-}" ] || _awk_note=" — note: id extraction without jq or python3 is best-effort; install one of them if the release assets look correct"
  [ -n "$CLI_ASSET_ID" ] || \
    err "asset not found in release $TAG: $CLI_ASSET — verify the release assets exist${_awk_note}"
  [ -n "$HELPER_ASSET_ID" ] || \
    err "asset not found in release $TAG: $HELPER_ASSET — verify the release assets exist${_awk_note}"
  [ -n "$SHA256SUMS_ASSET_ID" ] || \
    err "asset not found in release $TAG: SHA256SUMS — verify the release assets exist${_awk_note}"
}

validate_tag() {
  printf '%s' "$1" | grep -Eq "$TAG_RE" || \
    err "tag '$1' violates the release tag grammar (vX.Y.Z or vX.Y.Z-{alpha|beta|rc}.N)"
}

# tag_rank <tag>: prints "MAJ MIN PAT PRERANK PREN" for a grammar-valid tag.
# Prerelease classes order alpha < beta < rc < stable (stable rank 3, N 0).
tag_rank() {
  _t="${1#v}"
  _core="${_t%%-*}"
  _maj="${_core%%.*}"; _rest="${_core#*.}"
  _min="${_rest%%.*}"; _pat="${_rest#*.}"
  if [ "$_t" = "$_core" ]; then
    _pr=3; _pn=0
  else
    _pre="${_t#*-}"; _pn="${_pre#*.}"
    case "$_pre" in
      alpha.*) _pr=0 ;;
      beta.*)  _pr=1 ;;
      rc.*)    _pr=2 ;;
    esac
  fi
  printf '%s %s %s %s %s' "$_maj" "$_min" "$_pat" "$_pr" "$_pn"
}

# tag_older_than <a> <b>: true when a orders strictly before b. Both tags
# must already satisfy the tag grammar (callers validate).
tag_older_than() {
  # shellcheck disable=SC2046 # tag_rank output is exactly five integers
  set -- $(tag_rank "$1") $(tag_rank "$2")
  _i=1
  while [ "$_i" -le 5 ]; do
    _a="$1"; _b="$6"
    if [ "$_a" -lt "$_b" ]; then return 0; fi
    if [ "$_a" -gt "$_b" ]; then return 1; fi
    shift
    _i=$((_i + 1))
  done
  return 1
}

resolve_tag() {
  if [ -n "$REQ_VERSION" ]; then
    validate_tag "$REQ_VERSION"
    TAG="$REQ_VERSION"
    return
  fi
  if [ -n "$WANT_PRE" ]; then
    resolve_pre_tag
    return
  fi
  # Stable latest: one request, tag from the redirect Location. No JSON.
  # Transport failures are DISTINCT from absence: a curl error must never
  # read as "no stable release".
  _rc=0
  if [ -n "$TOKEN" ]; then
    _probe="$(curl_authed "" -sI -o /dev/null \
      -w '%{http_code} %{redirect_url}' "$BASE_URL/$REPO/releases/latest")" || _rc=$?
  else
    _probe="$(curl --proto '=https' --tlsv1.2 -sI -o /dev/null \
      -w '%{http_code} %{redirect_url}' "$BASE_URL/$REPO/releases/latest")" || _rc=$?
  fi
  [ "$_rc" -eq 0 ] || err "release resolution request failed (curl exit $_rc) — network, TLS, or proxy problem"
  _code="${_probe%% *}"
  _redirect="${_probe#* }"
  # Only GitHub's first-release 404 means "no stable release"; 401/403/429/
  # 5xx are request failures, not absence.
  case "$_code" in
    30?) : ;;
    404)
      if [ -z "$TOKEN" ]; then
        err "no stable release found — an internal repository needs auth (gh auth login or GH_TOKEN); on a public repository this means: use --pre or --version <tag>"
      fi
      err "no stable release yet — use --pre or --version <tag>; with a token set this can also mean the token lacks access to an internal repository (scope/SSO)"
      ;;
    *) err "release resolution failed (HTTP $_code) — check token/rate limits and retry" ;;
  esac
  case "$_redirect" in
    */tag/*) TAG="${_redirect##*/tag/}" ;;
    *) err "release resolution returned no tag redirect (HTTP $_code)" ;;
  esac
  validate_tag "$TAG"
}

# --pre: page the releases list, newest published (draft:false) entry wins.
# Requires a real JSON parser: drafts are visible to push-access tokens and
# regex field-pairing cannot be trusted against arbitrary release bodies.
resolve_pre_tag() {
  if command -v jq >/dev/null 2>&1; then
    _parse()   { jq -r 'map(select(.draft | not)) | (.[0].tag_name // "")'; }
    _count()   { jq -r 'length'; }
    _extract() { jq --arg t "$1" '.[] | select(.tag_name==$t)'; }
  elif command -v python3 >/dev/null 2>&1; then
    _parse()   { python3 -c 'import json,sys; rs=[r for r in json.load(sys.stdin) if not r.get("draft")]; print(rs[0]["tag_name"] if rs else "")'; }
    _count()   { python3 -c 'import json,sys; print(len(json.load(sys.stdin)))'; }
    _extract() { python3 -c 'import json,sys; rs=[r for r in json.load(sys.stdin) if r.get("tag_name")==sys.argv[1]]; print(json.dumps(rs[0]) if rs else "")' "$1"; }
  else
    err "--pre needs jq or python3 to parse the release listing"
  fi
  _page=1
  while :; do
    # Page cap: runaway-loop backstop against non-API responses (captive portal,
    # JSON object); not a release search window limit.
    [ "$_page" -gt 100 ] && err "release listing: page cap reached (100 pages) — non-API response or runaway loop"
    if ! _body="$(fetch "$API_URL/repos/$REPO/releases?per_page=100&page=$_page")"; then
      if [ -z "$TOKEN" ]; then
        err "release listing request failed (page $_page) — an internal repository needs auth: authenticate gh (gh auth login) or set GH_TOKEN"
      fi
      err "release listing request failed (page $_page)"
    fi
    _n="$(printf '%s' "$_body" | _count)"
    case "$_n" in
      ''|*[!0-9]*) err "release listing parse failed (page $_page) — unexpected response (captive portal?)" ;;
    esac
    [ "$_n" -eq 0 ] && err "no published release found — the listing is exhausted"
    TAG="$(printf '%s' "$_body" | _parse)"
    if [ -n "$TAG" ]; then
      validate_tag "$TAG"
      # Cache the winning release JSON so fetch_asset_ids can extract asset ids
      # without a redundant GET to the releases/tags endpoint.
      REL_JSON="$(printf '%s' "$_body" | _extract "$TAG")"
      return
    fi
    _page=$((_page + 1))
  done
}

verify_checksums() {
  # Each asset independently: exactly ONE entry, then its digest — a
  # combined count would accept a duplicated CLI entry masking a missing
  # helper entry, leaving one binary entirely unchecked.
  for _a in "$CLI_ASSET" "$HELPER_ASSET"; do
    _n="$(grep -cE "  $_a\$" "$WORK/SHA256SUMS" || true)"
    [ "$_n" -eq 1 ] || err "SHA256SUMS must contain exactly one entry for $_a (found $_n)"
    ( cd "$WORK" && grep -E "  $_a\$" SHA256SUMS | $SHA_CMD -c - >/dev/null 2>&1 ) || \
      err "checksum verification failed for $_a"
  done
}

# Attestation: capability probe, then verify. Anything short of a capable,
# authenticated gh takes the unavailable path (notice; fatal only with
# --verify-attestation). A capable client's verification failure is fatal.
#
# Attestation verification talks to github.com regardless of BASE_URL —
# that is where release attestations live. The auth check here is
# deliberately NOT scoped to the download host.
gh_capable() {
  command -v gh >/dev/null 2>&1 || return 1
  gh attestation verify --help 2>/dev/null | grep -q 'signer-workflow' || return 1
  gh auth status >/dev/null 2>&1 || return 1
  return 0
}

resolve_token() {
  [ -z "$TOKEN" ] || return 0
  command -v gh >/dev/null 2>&1 || return 0
  _host="${BASE_URL#*://}"
  _host="${_host%%/*}"
  _host="${_host##*@}"
  case "$_host" in
    \[*) _host="${_host#\[}"; _host="${_host%%\]*}" ;;
    *) _host="${_host%%:*}" ;;
  esac
  _tok="$(gh auth token --hostname "$_host" 2>/dev/null </dev/null || true)"
  [ -n "$_tok" ] || return 0
  TOKEN="$_tok"
  note "using gh auth token"
}

# hold_at: test-only SIGKILL seam — blocks while the named file exists.
hold_at() {
  [ -n "${1:-}" ] || return 0
  while [ -e "$1" ]; do sleep 0.1; done
}

# expected_sum <asset>: the sha256 hex the downloaded SHA256SUMS declares.
expected_sum() {
  awk -v a="$1" '$2 == a { print $1 }' "$WORK/SHA256SUMS"
}
# file_sum <path>: sha256 hex of an existing file.
file_sum() {
  $SHA_CMD "$1" | awk '{ print $1 }'
}

# Idempotent same-tag path: an existing version dir is reused ONLY if both
# binaries match the freshly downloaded SHA256SUMS — an .ok marker or
# executable bit proves nothing about content.
version_dir_intact() {
  [ -f "$1/rancher" ] && [ -f "$1/rancher-credential-vault" ] || return 1
  [ "$(file_sum "$1/rancher")" = "$(expected_sum "$CLI_ASSET")" ] || return 1
  [ "$(file_sum "$1/rancher-credential-vault")" = "$(expected_sum "$HELPER_ASSET")" ] || return 1
}

# Public names (both preflighted above, before any layout mutation):
# create or take over, atomically, per migration policy.
install_link() {
  _name="$1"
  _dest="$BIN_DIR/$_name"
  _target="$DATA_DIR/current/$_name"
  if [ -e "$_dest" ] || [ -L "$_dest" ]; then
    case "$(readlink "$_dest" 2>/dev/null || true)" in
      "$DATA_DIR"/*) : ;;  # ours: silent atomic replace
      "") note "notice: replacing existing file $_dest" ;;
      *) note "notice: replacing existing symlink $_dest -> $(readlink "$_dest")" ;;
    esac
  fi
  _tmp="$BIN_DIR/.$_name.$$"
  ln -s "$_target" "$_tmp"
  $MV_REPLACE "$_tmp" "$_dest"
}

# Receipt: atomic, JSON-escaped, best-effort (warn, never fail the install).
# Input is guaranteed newline-free (has_raw_controls rejects \n up front:
# awk cannot preserve trailing newlines — it never sees whether input ended
# with its record separator — so newline-bearing paths skip the receipt
# rather than silently recording a wrong path).
json_escape() {
  printf '%s' "$1" | awk 'BEGIN{ORS=""} {gsub(/\\/,"\\\\"); gsub(/"/,"\\\""); gsub(/\t/,"\\t"); gsub(/\r/,"\\r"); gsub(/\010/,"\\b"); gsub(/\014/,"\\f"); print}'
}
# has_raw_controls: any byte the receipt cannot represent faithfully —
# every C0 byte except tab/CR/backspace/formfeed. Newline is deliberately
# in the reject set (see json_escape).
has_raw_controls() {
  # Command substitution strips trailing newlines, so embedded LF would survive
  # tr undetected. Check for it explicitly before the tr pass.
  _nl="$(printf '\nx')"; _nl="${_nl%x}"
  case "$1" in *"$_nl"*) return 0 ;; esac
  [ -n "$(printf '%s' "$1" | LC_ALL=C tr -d '\011\015\010\014\040-\377')" ]
}
write_receipt() {
  for _v in "$TAG" "$BIN_DIR" "$DATA_DIR"; do
    if has_raw_controls "$_v"; then
      warn "receipt skipped: value contains unsupported control bytes"
      return 0
    fi
  done
  mkdir -p "$CONFIG_DIR" || { warn "receipt skipped: cannot create $CONFIG_DIR"; return 0; }
  _tmp="$CONFIG_DIR/.receipt.$$"
  printf '{"version":"%s","tag":"%s","binDir":"%s","dataDir":"%s","layout":"versioned-dir","channel":"script","installedAt":"%s"}\n' \
    "$(json_escape "${TAG#v}")" "$(json_escape "$TAG")" "$(json_escape "$BIN_DIR")" \
    "$(json_escape "$DATA_DIR")" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$_tmp" || \
    { warn "receipt skipped: cannot write $_tmp"; rm -f "$_tmp"; return 0; }
  # No-follow replace, like current and the public links: plain mv would
  # follow a pre-existing receipt symlink-to-directory and drop the temp
  # file INSIDE it while reporting success.
  $MV_REPLACE "$_tmp" "$CONFIG_DIR/install-receipt.json" || \
    { warn "receipt skipped: cannot move into place"; rm -f "$_tmp"; return 0; }
}

cleanup() {
  rc=$?
  if [ -n "$STAGING_DIR" ]; then rm -rf "$STAGING_DIR" 2>/dev/null || :; fi
  [ -n "$LOCK" ] && rmdir "$LOCK" 2>/dev/null
  rm -rf "$WORK"
  exit "$rc"
}

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) [ $# -ge 2 ] || usage; REQ_VERSION="$2"; shift 2 ;;
      --pre) WANT_PRE=1; shift ;;
      --bin-dir) [ $# -ge 2 ] || usage; BIN_DIR="$2"; shift 2 ;;
      --verify-attestation) REQUIRE_ATTESTATION=1; shift ;;
      --dry-run) DRY_RUN=1; shift ;;
      -q) QUIET=1; shift ;;
      *) usage ;;
    esac
  done
  [ -n "$REQ_VERSION" ] && [ -n "$WANT_PRE" ] && err "--version and --pre are mutually exclusive"
  [ -n "${HOME:-}" ] || err "HOME is not set"
  [ -n "$BIN_DIR" ] || BIN_DIR="$HOME/.local/bin"
  DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/rancher-cli"
  CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/rancher-cli"
}

# grep/awk/tr are load-bearing (tag grammar, checksum selection, receipt
# guard — a missing tr would fail the control-byte guard OPEN), so they are
# preflighted like the rest.
preflight_tools() {
  for tool in curl uname mktemp chmod mv ln readlink grep awk tr; do
    command -v "$tool" >/dev/null 2>&1 || err "required tool not found: $tool"
  done
  if command -v sha256sum >/dev/null 2>&1; then
    SHA_CMD="sha256sum"
  elif command -v shasum >/dev/null 2>&1; then
    SHA_CMD="shasum -a 256"
  else
    err "required tool not found: sha256sum (or shasum)"
  fi
}

probe_platform() {
  OS="$(uname -s)"
  ARCH="$(uname -m)"
  case "$OS" in
    Linux) OS="linux"; MV_REPLACE="mv -T" ;;
    Darwin) OS="darwin"; MV_REPLACE="mv -h" ;;
    *) err "unsupported platform $OS/$ARCH — download manually: $BASE_URL/$REPO/releases" ;;
  esac
  case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    s390x) ARCH="s390x" ;;
    *) err "unsupported platform $OS/$ARCH — download manually: $BASE_URL/$REPO/releases" ;;
  esac
  if [ "$OS" = "darwin" ] && [ "$ARCH" = "s390x" ]; then
    err "unsupported platform darwin/s390x — download manually: $BASE_URL/$REPO/releases"
  fi
}

resolve() {
  resolve_tag
  # Asset names must match release.yaml's build loop — the two lists stay in lockstep.
  CLI_ASSET="rancher-$OS-$ARCH"
  HELPER_ASSET="rancher-credential-vault-$OS-$ARCH"
  DL_BASE="$BASE_URL/$REPO/releases/download/$TAG"

  if [ -n "$DRY_RUN" ]; then
    # Run id resolution now so the preview shows the true download source.
    # Contract: resolution GETs only — when TOKEN is set, one additional
    # metadata GET, but no filesystem writes, no lock, no attestation.
    fetch_asset_ids
    printf 'tag: %s\n' "$TAG"
    printf 'platform: %s/%s\n' "$OS" "$ARCH"
    if [ -n "$CLI_ASSET_ID" ]; then
      printf 'asset: %s\n' "$API_URL/repos/$REPO/releases/assets/$CLI_ASSET_ID"
      printf 'asset: %s\n' "$API_URL/repos/$REPO/releases/assets/$HELPER_ASSET_ID"
      printf 'asset: %s\n' "$API_URL/repos/$REPO/releases/assets/$SHA256SUMS_ASSET_ID"
    else
      printf 'asset: %s/%s\n' "$DL_BASE" "$CLI_ASSET"
      printf 'asset: %s/%s\n' "$DL_BASE" "$HELPER_ASSET"
      printf 'asset: %s/%s\n' "$DL_BASE" "SHA256SUMS"
    fi
    printf 'bin-dir: %s\n' "$BIN_DIR"
    printf 'data-dir: %s\n' "$DATA_DIR"
    exit 0
  fi
}

download() {
  # Probe MV_REPLACE: BusyBox mv lacks -T/-h; fail early rather than mid-install.
  # After the dry-run exit: dry-run never renames anything, and probing before it
  # would make platform reporting depend on the host's mv rather than on uname.
  _mv_probe="$(mktemp -d)"
  ln -s /dev/null "$_mv_probe/a"
  ln -s /dev/null "$_mv_probe/b"
  $MV_REPLACE "$_mv_probe/b" "$_mv_probe/a" 2>/dev/null || \
    err "mv does not support the required no-follow replace (-T/-h) — unsupported mv implementation (BusyBox?)"
  rm -rf "$_mv_probe"

  WORK="$(mktemp -d)"
  trap cleanup 0
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM

  note "downloading $TAG for $OS/$ARCH"
  if [ -n "$CLI_ASSET_ID" ]; then
    # Private/internal repos: browser download URLs return 404 regardless of
    # authentication. Use the GitHub API asset endpoint; fetch_asset_ids
    # resolved the numeric ids. If metadata was not reachable (scoped token on
    # a public repo) CLI_ASSET_ID is empty and we fall through to browser URLs.
    fetch_asset "$CLI_ASSET_ID" "$WORK/$CLI_ASSET" "$CLI_ASSET"
    fetch_asset "$HELPER_ASSET_ID" "$WORK/$HELPER_ASSET" "$HELPER_ASSET"
    fetch_asset "$SHA256SUMS_ASSET_ID" "$WORK/SHA256SUMS" "SHA256SUMS"
  else
    if ! fetch "$DL_BASE/$CLI_ASSET" "$WORK/$CLI_ASSET"; then
      _dlmsg="download failed: $DL_BASE/$CLI_ASSET (404 = tag '$TAG' has no such asset)"
      if [ -n "$TOKEN" ] && [ -n "$METADATA_FAIL_CODE" ]; then
        _dlmsg="$_dlmsg — or the token lacks access to an internal repository (scope/SSO); release metadata was also unreadable (HTTP $METADATA_FAIL_CODE)"
      fi
      err "$_dlmsg"
    fi
    fetch "$DL_BASE/$HELPER_ASSET" "$WORK/$HELPER_ASSET" || err "download failed: $DL_BASE/$HELPER_ASSET"
    fetch "$DL_BASE/SHA256SUMS" "$WORK/SHA256SUMS" || err "download failed: $DL_BASE/SHA256SUMS"
  fi
}

verify() {
  verify_checksums
  if gh_capable; then
    for _b in "$WORK/$CLI_ASSET" "$WORK/$HELPER_ASSET"; do
      gh attestation verify "$_b" --repo "$REPO" \
        --signer-workflow "$REPO/.github/workflows/release.yaml" >/dev/null 2>&1 || \
        err "build provenance attestation FAILED for $_b"
    done
    note "build provenance verified"
  else
    if [ -n "$REQUIRE_ATTESTATION" ]; then
      err "--verify-attestation: no capable authenticated gh CLI available"
    fi
    note "notice: attestation skipped (no capable authenticated gh CLI)"
  fi
}

stage() {
  mkdir -p "$BIN_DIR" || err "cannot create bin dir $BIN_DIR"
  [ -w "$BIN_DIR" ] || err "bin dir $BIN_DIR is not writable"
  mkdir -p "$DATA_DIR/versions" || err "cannot create data dir $DATA_DIR"

  _lock_path="$DATA_DIR/.install.lock"
  if ! mkdir "$_lock_path" 2>/dev/null; then
    err "another install is in progress (lock: $_lock_path); if none is, removing the lock directory is safe"
  fi
  LOCK="$_lock_path"

  # Preflight the current symlink: a real directory here would cause GC to
  # delete every version dir (CURRENT_TARGET="" matches nothing) and the
  # atomic switch would nest inside it instead of replacing it.
  if [ -e "$DATA_DIR/current" ] && [ ! -L "$DATA_DIR/current" ]; then
    err "$DATA_DIR/current exists but is not a symlink — the layout is corrupt; remove it and rerun"
  fi

  # Preflight BOTH public paths before ANY layout mutation (staging, switch,
  # GC): a refusal discovered later would leave managed links already serving
  # a new pair while the install reports failure.
  for _name in rancher rancher-credential-vault; do
    if [ -d "$BIN_DIR/$_name" ] && [ ! -L "$BIN_DIR/$_name" ]; then
      err "$BIN_DIR/$_name is a directory — refusing to replace it"
    fi
  done

  # GC under the lock: staging litter and version dirs current doesn't reference.
  CURRENT_TARGET=""
  [ -L "$DATA_DIR/current" ] && CURRENT_TARGET="$(readlink "$DATA_DIR/current")"
  for _d in "$DATA_DIR"/versions/*; do
    [ -e "$_d" ] || continue
    case "$_d" in
      *.staging.*) rm -rf "$_d" ;;
      *) [ "versions/${_d##*/}" = "$CURRENT_TARGET" ] || [ "$_d" = "$DATA_DIR/$CURRENT_TARGET" ] || \
           [ "${_d##*/}" = "${CURRENT_TARGET##*/}" ] || rm -rf "$_d" ;;
    esac
  done

  VDIR="$DATA_DIR/versions/$TAG"
  if [ -d "$VDIR" ] && version_dir_intact "$VDIR"; then
    note "version $TAG already present and intact — repairing links"
  else
    [ -d "$VDIR" ] && VDIR="$DATA_DIR/versions/$TAG.2"
    STAGING_DIR="$DATA_DIR/versions/$TAG.staging.$$"
    mkdir -p "$STAGING_DIR"
    cp "$WORK/$CLI_ASSET" "$STAGING_DIR/rancher"
    cp "$WORK/$HELPER_ASSET" "$STAGING_DIR/rancher-credential-vault"
    chmod 0755 "$STAGING_DIR/rancher" "$STAGING_DIR/rancher-credential-vault"
    hold_at "${RANCHER_CLI_INSTALL_HOLD_AFTER_STAGING:-}"
    mv "$STAGING_DIR" "$VDIR"
  fi
}

link() {
  # Name the version change before switching: an upgrade is a note, a
  # DOWNGRADE a warning (documented rollback flow, but never silent). A
  # previous target that is not a plain tag (collision-suffixed dir,
  # corrupt layout) gets the generic form without ordering.
  _prev="${CURRENT_TARGET##*/}"
  if [ -n "$_prev" ] && [ "$_prev" != "$TAG" ]; then
    if printf '%s' "$_prev" | grep -Eq "$TAG_RE"; then
      if tag_older_than "$TAG" "$_prev"; then
        warn "downgrading from $_prev to $TAG"
      else
        note "upgrading from $_prev to $TAG"
      fi
    else
      note "replacing $_prev with $TAG"
    fi
  fi

  # Atomic current switch: temp symlink + rename over the link itself.
  # Plain mv would move the temp link INTO the directory current points at.
  _tmp_link="$DATA_DIR/.current.$$"
  ln -s "versions/${VDIR##*/}" "$_tmp_link"
  $MV_REPLACE "$_tmp_link" "$DATA_DIR/current"
  [ "$(readlink "$DATA_DIR/current")" = "versions/${VDIR##*/}" ] || \
    err "current switch failed verification — rerun the installer"
  hold_at "${RANCHER_CLI_INSTALL_HOLD_AFTER_SWITCH:-}"

  # Post-switch GC: the pre-staging pass necessarily kept the then-current old
  # version; now that current points at the new dir, collect the superseded one.
  for _d in "$DATA_DIR"/versions/*; do
    [ -e "$_d" ] || continue
    [ "${_d##*/}" = "${VDIR##*/}" ] || rm -rf "$_d"
  done

  install_link "rancher"
  hold_at "${RANCHER_CLI_INSTALL_HOLD_AFTER_FIRST_LINK:-}"
  install_link "rancher-credential-vault"
}

epilogue() {
  # Epilogue: verify the links resolve, prove the install, print next steps.
  if [ ! -x "$BIN_DIR/rancher" ] || [ ! -x "$BIN_DIR/rancher-credential-vault" ]; then
    err "installed links do not resolve — rerun the installer"
  fi
  note "installed: $("$BIN_DIR/rancher" --version)"
  case ":$PATH:" in
    *":$BIN_DIR:"*) : ;;
    *)
      _shell="${SHELL:-}"
      case "${_shell##*/}" in
        fish) note "add to PATH: fish_add_path $BIN_DIR" ;;
        *)    note "add to PATH: export PATH=\"$BIN_DIR:\$PATH\"" ;;
      esac ;;
  esac
  # Shadow scan: another rancher/rancher-credential-vault elsewhere on PATH
  # can shadow this install — either by PATH order or by a shell's command
  # hash cached before these links existed. The installer cannot fix a
  # parent shell's hash; it can name the hazard. warn (not note) so -q
  # cannot hide a shadowed install. set -f spans the whole loop: the
  # unquoted $PATH expands (and could glob) exactly once, at the `for`;
  # every expansion inside the body is quoted.
  # Loop invariants, physically resolved once (see physical_dir).
  _bin_dir_p="$(physical_dir "$BIN_DIR")"
  _cur_dir_p="$(physical_dir "$DATA_DIR/current")"
  _oldifs="$IFS"; IFS=:
  set -f
  for _d in $PATH; do
    [ -n "$_d" ] || _d=.
    [ "${_d%/}" != "${BIN_DIR%/}" ] || continue
    for _b in rancher rancher-credential-vault; do
      if [ ! -x "$_d/$_b" ] || [ -d "$_d/$_b" ]; then
        continue
      fi
      # A convenience symlink pointing back at this install runs the
      # identical binary — not a shadow, no warning. Follow the chain
      # (update-alternatives-style layouts use two hops), resolving
      # relative targets against each link's own directory, then compare
      # the final path canonicalized physically (cd + pwd -P resolves
      # ., .., and dir symlinks; readlink -f/realpath are not in the
      # preflight set). The hop cap breaks symlink cycles: a candidate
      # still symlinked at the cap simply warns like any other shadow.
      if [ -L "$_d/$_b" ]; then
        _p="$_d/$_b"
        _hops=0
        while [ -L "$_p" ] && [ "$_hops" -lt 8 ]; do
          _tgt="$(readlink "$_p")"
          case "$_tgt" in
            /*) _p="$_tgt" ;;
            *) _p="${_p%/*}/$_tgt" ;;
          esac
          _hops=$((_hops + 1))
        done
        _p_dir="$(physical_dir "${_p%/*}")"
        if [ -n "$_p_dir" ] && [ "${_p##*/}" = "$_b" ] && \
           { [ "$_p_dir" = "$_bin_dir_p" ] || [ "$_p_dir" = "$_cur_dir_p" ]; }; then
          continue
        fi
      fi
      warn "another $_b exists at $_d/$_b — it may shadow this install (check PATH order; run 'hash -r' or 'rehash' in open shells)"
    done
  done
  set +f
  IFS="$_oldifs"
  note "next steps:"
  note "  rancher completion <bash|zsh|fish|pwsh>   # shell completion"
  note "  rancher skill install                     # agent harnesses"
  note "  rancher login <server-url>                # authenticate"
}

main() {
  parse_args "$@"
  resolve_token
  preflight_tools
  probe_platform
  resolve
  fetch_asset_ids
  download
  verify
  stage
  link
  write_receipt
  epilogue
}

main "$@"
