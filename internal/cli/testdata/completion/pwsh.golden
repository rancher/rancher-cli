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
#
# Minimum supported shell: Windows PowerShell 5.1 or pwsh 7+
# (Register-ArgumentCompleter -Native and CompletionResult are the newest
# APIs used).

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
        $lastInRangeText = $inRange[$inRange.Count - 1].Extent.Text
    } else {
        $tokens = @('rancher', $wordToComplete)
        $lastInRangeText = $null
    }

    # Protocol v2: self-prefixed current-word field.
    # $tokens[0] = binary; $tokens[1..Count-2] = words before cursor (middle);
    # $wordToComplete = current word (may be empty string).
    $exe = $tokens[0]
    # PowerShell's CommandAst.CommandElements does NOT append an empty element for
    # trailing whitespace, so $wordToComplete is '' when cursor is after a space.
    # When empty, every AST token after the binary is a completed word; when non-empty,
    # the final AST token is the current (partial) word and must be excluded.
    # The whole if-expression is wrapped in @(): PowerShell unrolls a
    # single-element array result of an if-expression to a bare String, and
    # splatting a String enumerates its CHARACTERS - "get" would reach the
    # binary as three arguments g, e, t. The outer @() re-wraps every branch.
    $middle = @(if ([string]::IsNullOrEmpty($wordToComplete)) {
        if ($tokens.Count -gt 1) { $tokens[1..($tokens.Count - 1)] }
    } else {
        if ($tokens.Count -gt 2) { $tokens[1..($tokens.Count - 2)] }
    })
    $cur = if ($null -ne $wordToComplete) { $wordToComplete } else { '' }
    # A -Native completer receives the current word's RAW extent text, so a
    # quoted word arrives WITH its quotes ('a b c'). The parser's .Value -
    # already collected into $tokens - is the unquoted form the binary must
    # see. Applied only when the last in-range element is the current word
    # (extent text matches), so a mid-word cursor or the no-AST fallback
    # keeps the raw text.
    if (-not [string]::IsNullOrEmpty($cur) -and $lastInRangeText -eq $wordToComplete) {
        $cur = $tokens[$tokens.Count - 1]
    }
    # "--rancher-completion-cur=$cur" is one token even when $cur is "".
    $currentWordToken = "--rancher-completion-cur=$cur"

    # Match against the unquoted current word: the raw $wordToComplete keeps
    # its quotes, and a quoted word can never prefix-match a candidate.
    $escapedWord = [System.Management.Automation.WildcardPattern]::Escape($cur)

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
