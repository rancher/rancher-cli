# rancher shell completion (bash)
#
# Adapted from urfave/cli's embedded bash_autocomplete script
# (https://github.com/urfave/cli), MIT License,
# Copyright (c) 2023 urfave/cli maintainers.
#
# Source this to enable completion:
#   source <(rancher completion bash)

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

# Builds the completion request as an argv ARRAY in the global
# __rancher_completion_request, never a string: COMP_WORDS[0] is the real
# rancher binary, so the array is invoked directly and a pasted $(...) in
# any word is passed as an inert argument rather than evaluated.
__rancher_build_completion_request() {
  __rancher_completion_request=("${COMP_WORDS[@]:0:${COMP_CWORD}}")
  local current_word="${COMP_WORDS[COMP_CWORD]}"

  if [[ "${current_word}" == "-"* ]]; then
    __rancher_completion_request+=("${current_word}")
  fi

  __rancher_completion_request+=(--generate-shell-completion)
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

    local matches=( $(compgen -W "${__rancher_completion_tokens[*]}" -- "${cur}") )

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
    else
      COMPREPLY=("${matches[@]}")
    fi

    return 0
  fi
}

complete -o bashdefault -o default -F __rancher_bash_autocomplete rancher
