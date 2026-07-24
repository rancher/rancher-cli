# rancher shell completion (PowerShell)
#
# Adapted from urfave/cli's embedded powershell_autocomplete.ps1 script
# (https://github.com/urfave/cli), MIT License,
# Copyright (c) 2023 urfave/cli maintainers.
#
# Unlike urfave's own script, which derives the command name from the .ps1
# filename it is saved as, this one hardcodes "rancher": it is emitted
# directly by `rancher completion pwsh`, never saved under a different name.
#
# Output the script to path/to/rancher.ps1 and run it to enable completion.

Register-ArgumentCompleter -Native -CommandName rancher -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)

    # For a -Native completer PowerShell binds (wordToComplete, commandAst,
    # cursorPosition) in that order: wordToComplete is only the current word,
    # and commandAst is the real, already-parsed CommandAst for the whole
    # line - not a string. Reading its CommandElements gives quote-aware
    # tokens without ever handing typed text to an interpreter.
    #
    # Only elements ending at or before the cursor are kept, mirroring
    # bash.sh's COMP_WORDS[0:COMP_CWORD] slice: editing an earlier word with
    # more already typed after it must not leak those later tokens into the
    # request. The command name (index 0) is always kept even if a filter
    # would otherwise drop it, so a single-token line never empties out.
    if ($commandAst -is [System.Management.Automation.Language.CommandAst] -and $commandAst.CommandElements.Count -gt 0) {
        $inRange = @($commandAst.CommandElements | Where-Object { $_.Extent.EndOffset -le $cursorPosition })
        if ($inRange.Count -eq 0) { $inRange = @($commandAst.CommandElements[0]) }
        $tokens = @(foreach ($element in $inRange) {
            if ($null -ne $element.Value) { $element.Value.ToString() } else { $element.Extent.Text }
        })
    } else {
        $tokens = @('rancher', $wordToComplete)
    }

    # Protocol v2: self-prefixed current-word field (round-5 collapse-proof encoding).
    # $tokens[0] = binary; $tokens[1..Count-2] = words before cursor (middle);
    # $wordToComplete = current word (may be empty string).
    $exe = $tokens[0]
    # PowerShell's CommandAst.CommandElements does NOT append an empty element for
    # trailing whitespace, so $wordToComplete is '' when cursor is after a space.
    # When empty, every AST token after the binary is a completed word; when non-empty,
    # the final AST token is the current (partial) word and must be excluded.
    $middle = if ([string]::IsNullOrEmpty($wordToComplete)) {
        if ($tokens.Count -gt 1) { @($tokens[1..($tokens.Count - 1)]) } else { @() }
    } else {
        if ($tokens.Count -gt 2) { @($tokens[1..($tokens.Count - 2)]) } else { @() }
    }
    $cur = if ($null -ne $wordToComplete) { $wordToComplete } else { '' }
    # "--rancher-completion-cur=$cur" is one token even when $cur is "".
    $currentWordToken = "--rancher-completion-cur=$cur"

    $escapedWord = [System.Management.Automation.WildcardPattern]::Escape($wordToComplete)

    & $exe '--rancher-completion-proto=2' @middle $currentWordToken '--generate-shell-completion' 2>$null | ForEach-Object {
        $parts = $_.Split(':', 2)
        if ($parts.Count -eq 2) {
            $completion = $parts[0].Trim()
            $description = $parts[1].Trim()
        } else {
            $completion = $_.Trim()
            $description = $_.Trim()
        }
        if ($completion -like "$escapedWord*") {
            # Continuation candidates ("amazonec2config/", "template=") need
            # no special handling here: PowerShell inserts CompletionText
            # verbatim and never appends a trailing space, so acceptance
            # keeps the cursor in the same word. bash and zsh need explicit
            # handling - see bash.sh and zsh.sh.
            [System.Management.Automation.CompletionResult]::new($completion, $completion, 'ParameterValue', $description)
        }
    }
}
