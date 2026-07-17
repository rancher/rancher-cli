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

_rancher() {
	local -a opts
	local current
	current=${words[-1]}
	# Protocol v2: self-prefixed current-word field (round-5 collapse-proof encoding).
	# "--rancher-completion-cur=${current}" is quoted so it is one token even when current is "".
	opts=("${(@f)$(${words[1]} --rancher-completion-proto=2 ${words[2,-2]} "--rancher-completion-cur=${current}" --generate-shell-completion)}")

	if [[ "${opts[1]}" != "" ]]; then
		_describe 'values' opts
	else
		_files
	fi
}

# Don't run the completion function when being source-ed or eval-ed.
# See https://github.com/urfave/cli/issues/1874 for discussion.
if [ "$funcstack[1]" = "_rancher" ]; then
	_rancher
fi
