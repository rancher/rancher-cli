# rancher shell completion (fish)
#
# Adapted from urfave/cli's embedded fish_autocomplete script
# (https://github.com/urfave/cli), MIT License,
# Copyright (c) 2023 urfave/cli maintainers.
#
# Enable completion:
#   rancher completion fish > ~/.config/fish/completions/rancher.fish
#
# Minimum supported shell: fish 3.0. Every construct here (string builtin,
# commandline -opc/-ct, negative slices) dates to fish 2.3, but 3.0 is the
# oldest release the suite is expected to run against.

function __rancher_perform_completion
    # Extract all args except the last one
    set -l args (commandline -opc)
    # Extract the last arg (partial input / current word)
    set -l lastArg (commandline -ct)

    # Protocol v2: self-prefixed current-word field (round-5 collapse-proof encoding).
    # "--rancher-completion-cur=$lastArg" is one token even when lastArg is "".
    set results ($args[1] --rancher-completion-proto=2 $args[2..-1] "--rancher-completion-cur=$lastArg" --generate-shell-completion 2> /dev/null)

    # Remove trailing empty lines
    for line in $results[-1..1]
        if test (string trim -- $line) = ""
            set results $results[1..-2]
        else
            break
        end
    end

    for line in $results
        set -l parts (string split -m 1 ":" -- "$line")
        if test (count $parts) -eq 2
            printf "%s\t%s\n" "$parts[1]" "$parts[2]"
        else
            printf "%s\n" "$line"
        end
    end
end

# Clear existing completions for rancher
complete -c rancher -e
# Register completion function.
#
# Continuation candidates ("amazonec2config/", "template=") need no special
# handling here: fish itself withholds the trailing space for completions
# ending in / = @ : . , - (complete -a entries carry AUTO_SPACE and fish's
# resolve_auto_space turns those suffixes into NO_SPACE), so acceptance keeps
# the cursor in the same token. bash and zsh need explicit handling — see
# bash.sh and zsh.sh.
complete -c rancher -f -a '(__rancher_perform_completion)'
