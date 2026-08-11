# rancher shell completion (bash)
#
# Adapted from urfave/cli's embedded bash_autocomplete script
# (https://github.com/urfave/cli), MIT License,
# Copyright (c) 2023 urfave/cli maintainers.
#
# Source this to enable completion:
#   source <(rancher completion bash)
#
# Minimum supported shell: bash 3.2 (macOS stock) with the bash-completion
# package (1.2+) providing _get_comp_words_by_ref; when bash-completion 2.12+
# is present its _comp_initialize is used instead. Without bash-completion
# the completer silently declines. On bash 3 the double-TAB description
# column is absent (COMP_TYPE is bash 4.0+); completion itself is unaffected.

# macOS still ships Bash 3, where bash-completion may not provide
# _init_completion. This is a minimal compatible fallback.
__rancher_init_completion() {
  COMPREPLY=()
  if declare -F _comp_initialize >/dev/null 2>&1; then
    _comp_initialize "$@"
  else
    _get_comp_words_by_ref "$@" cur prev words cword
  fi
}

__rancher_build_completion_request() {
  # Build the request from the reconstructed completion variables produced by
  # __rancher_init_completion -n "=:" (words/cword/cur) so that equals-form
  # flags like --by=n are not split: bash's default word breaks turn --by=n
  # into three tokens (--by, =, n), but _get_comp_words_by_ref -n "=:" rejoins
  # them so cur="--by=n" and words/cword reflect the collapsed form.
  # --rancher-completion-cur=${cur} is one token even when cur is "".
  __rancher_completion_request=(
    "${words[0]}"
    "--rancher-completion-proto=2"
    "${words[@]:1:$((cword - 1))}"
    "--rancher-completion-cur=${cur}"
    "--generate-shell-completion"
  )
}

# Keep Bash 3 compatibility: associative arrays require Bash 4+, so
# descriptions are looked up via parallel indexed arrays.
__rancher_find_description_for() {
  local candidate="$1"
  local i

  for i in "${!__rancher_completion_tokens[@]}"; do
    if [[ "${__rancher_completion_tokens[i]}" == "${candidate}" ]]; then
      printf '%s' "${__rancher_completion_descriptions[i]}"
      return 0
    fi
  done

  return 1
}

__rancher_bash_autocomplete() {
  local words=("${COMP_WORDS[@]}")

  if [[ "${words[0]}" != "source" ]]; then
    local cur opts
    local cword="${COMP_CWORD}"
    local -a __rancher_completion_request

    COMPREPLY=()
    cur="${words[$cword]}"

    __rancher_init_completion -n "=:" || return

    __rancher_build_completion_request
    opts=$("${__rancher_completion_request[@]}" 2>/dev/null)

    # Completion output lines use "token:description" format; some of our
    # own commands print a bare token with no colon, handled below too.
    __rancher_completion_tokens=()
    __rancher_completion_descriptions=()

    local line
    local longest=0
    while IFS=$'\n' read -r line; do
      local token="${line}"
      local description=""

      if [[ "${line}" == *:* ]]; then
        token="${line%%:*}"
        description="${line#*:}"
      fi

      if [[ -z "${token}" ]]; then
        continue
      fi

      __rancher_completion_tokens+=("${token}")
      __rancher_completion_descriptions+=("${description}")
      (( ${#token} > longest )) && longest=${#token}
    done <<< "${opts}"

    # Match by literal prefix in bash rather than `compgen -W "${tokens[*]}"`:
    # compgen -W word-splits and command-substitutes its wordlist, so a
    # candidate containing $(...) — e.g. a config context/server name the
    # server-side completer echoes back — would execute on TAB. A quoted
    # comparison keeps every candidate a literal.
    local matches=()
    local __rancher_candidate
    for __rancher_candidate in "${__rancher_completion_tokens[@]}"; do
      if [[ "${__rancher_candidate}" == "${cur}"* ]]; then
        matches+=("${__rancher_candidate}")
      fi
    done

    # COMP_TYPE=63 means Bash is listing matches (usually on second TAB).
    if [[ "${COMP_TYPE:-}" == "63" && ${#matches[@]} -gt 0 ]]; then
      local listed=()
      local candidate
      for candidate in "${matches[@]}"; do
        local desc="$(__rancher_find_description_for "${candidate}")"

        if [[ -n "${desc}" ]]; then
          local padded="$(printf '%-*s' "${longest}" "${candidate}")"
          listed+=("${padded}  -- ${desc}")
        else
          listed+=("${candidate}")
        fi
      done
      COMPREPLY=("${listed[@]}")
    elif [[ ${#matches[@]} -gt 0 ]]; then
      # Registration uses -o nospace, so readline never appends the
      # argument-ending space itself and each candidate marks its own
      # boundary instead. A FINISHED candidate gets an explicit trailing
      # space appended, so accepting it still ends the argument; a candidate
      # ending in a continuation character is a prefix stage, not a finished
      # argument — "amazonec2config/" expects a name after the slash,
      # "template=" expects a Go template after the equals — and gets none,
      # so the cursor stays in the same word for the second-stage
      # completion. This is the git-completion pattern, chosen over a
      # per-invocation `compopt -o nospace` because compopt does not exist
      # on bash 3 (macOS stock): candidate-side spaces behave identically on
      # bash 3 and 4+. Multiple matches are unaffected either way — readline
      # inserts only the common prefix, which never reaches a trailing space
      # ("pod " and "pods " share only "pod"). fish and PowerShell need no
      # equivalent — see the notes in fish.sh and pwsh.ps1.
      COMPREPLY=()
      local __rancher_match
      for __rancher_match in "${matches[@]}"; do
        if [[ "${__rancher_match}" == */ || "${__rancher_match}" == *= ]]; then
          COMPREPLY+=("${__rancher_match}")
        else
          COMPREPLY+=("${__rancher_match} ")
        fi
      done
    else
      # No candidates from the binary: hand off to filename completion.
      #
      # bash 4+ (compopt exists): COMPREPLY stays empty so readline's own
      # -o default fallback runs, and its appended space — which the
      # registration-level -o nospace would strip after a completed file
      # name — is re-enabled for this invocation. Readline then owns
      # quoting, tilde handling, and boundaries natively.
      #
      # bash 3 (macOS stock, no compopt): the native fallback would insert
      # completed file names with NO argument delimiter at all, so generate
      # the candidates here instead, with the same per-candidate boundaries
      # the script's own candidates carry — "/" (no space) after
      # directories, a trailing space after everything else. Entries are
      # escaped with printf %q (a bash 3.2 builtin) because readline
      # inserts COMPREPLY entries verbatim (this compspec has no
      # -o filenames), so the entry itself must be shell-quoted for a name
      # with spaces to land as one word. Residual, the most pure bash 3 can
      # do: a typed prefix that is ALREADY quoted/escaped matches nothing
      # here and falls through to the native (spaceless) fallback, only
      # bare "~"/"~/..." prefixes are tilde-expanded (to the expanded
      # $HOME text, as readline's expand-tilde does; "~user" falls
      # through), and a file name with an embedded newline mis-splits.
      if type compopt &>/dev/null; then
        compopt +o nospace 2>/dev/null
      else
        local __rancher_fallback_cur="${cur}"
        if [[ "${__rancher_fallback_cur}" == "~" || "${__rancher_fallback_cur}" == "~/"* ]]; then
          __rancher_fallback_cur="${HOME}${__rancher_fallback_cur#\~}"
        fi
        local __rancher_name
        while IFS=$'\n' read -r __rancher_name; do
          if [[ -z "${__rancher_name}" ]]; then
            continue
          fi
          if [[ -d "${__rancher_name}" ]]; then
            COMPREPLY+=("$(printf '%q' "${__rancher_name}")/")
          else
            COMPREPLY+=("$(printf '%q' "${__rancher_name}") ")
          fi
        # compgen -f emits in platform/locale-dependent order (Linux bash
        # groups directories first under some locales); a fixed collation
        # keeps candidate order identical everywhere.
        done < <(compgen -f -- "${__rancher_fallback_cur}" | LC_ALL=C sort)
      fi
    fi

    return 0
  fi
}

complete -o bashdefault -o default -o nospace -F __rancher_bash_autocomplete rancher
