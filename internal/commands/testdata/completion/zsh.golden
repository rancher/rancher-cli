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
	local -a opts # Declare a local array
	local current
	current=${words[-1]} # -1 means "the last element"
	if [[ "$current" == "-"* ]]; then
		# Current word starts with a hyphen, so complete flags/options
		opts=("${(@f)$(${words[@]:0:#words[@]-1} ${current} --generate-shell-completion)}")
	else
		# Current word does not start with a hyphen, so complete subcommands
		opts=("${(@f)$(${words[@]:0:#words[@]-1} --generate-shell-completion)}")
	fi

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
