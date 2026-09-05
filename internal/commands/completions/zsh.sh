#compdef rancher
compdef _rancher rancher

# rancher shell completion (zsh)
#
# Adapted from urfave/cli's embedded zsh_autocomplete script
# (https://github.com/urfave/cli), MIT License,
# Copyright (c) 2023 urfave/cli maintainers.
#
# Source this to enable completion:
#   source <(rancher completion zsh)
#
# Minimum supported shell: zsh 5.0, with the completion system initialized
# (compinit) - compdef, _describe, and _files come from it.

_rancher() {
	local -a opts
	local current
	current=${words[-1]}
	# Protocol v2: self-prefixed current-word field (round-5 collapse-proof encoding).
	# "--rancher-completion-cur=${current}" is quoted so it is one token even when current is "".
	opts=("${(@f)$(${words[1]} --rancher-completion-proto=2 ${words[2,-2]} "--rancher-completion-cur=${current}" --generate-shell-completion 2> /dev/null)}")

	if [[ "${opts[1]}" != "" ]]; then
		# A candidate whose token ends in a continuation character is a
		# prefix stage, not a finished argument - "amazonec2config/" expects
		# a name after the slash, "template=" expects a Go template after the
		# equals. compadd's default suffix (a space) would end the word on
		# acceptance, so those are added with -S '' (empty suffix) and the
		# next keystroke keeps extending the same word. fish and PowerShell
		# need no equivalent - see the notes in fish.sh and pwsh.ps1.
		local -a __rancher_continuing __rancher_finished
		local __rancher_candidate __rancher_token
		for __rancher_candidate in "${opts[@]}"; do
			__rancher_token="${__rancher_candidate%%:*}"
			if [[ "${__rancher_token}" == */ || "${__rancher_token}" == *= ]]; then
				__rancher_continuing+=("${__rancher_candidate}")
			else
				__rancher_finished+=("${__rancher_candidate}")
			fi
		done
		local ret=1
		(( ${#__rancher_continuing[@]} )) && _describe 'values' __rancher_continuing -S '' && ret=0
		(( ${#__rancher_finished[@]} )) && _describe 'values' __rancher_finished && ret=0
		return ret
	else
		_files
	fi
}

# Don't run the completion function when being source-ed or eval-ed.
# See https://github.com/urfave/cli/issues/1874 for discussion.
if [ "$funcstack[1]" = "_rancher" ]; then
	_rancher
fi
