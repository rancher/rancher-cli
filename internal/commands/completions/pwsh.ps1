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
    if ($commandAst -is [System.Management.Automation.Language.CommandAst] -and $commandAst.CommandElements.Count -gt 0) {
        $tokens = @(foreach ($element in $commandAst.CommandElements) {
            if ($null -ne $element.Value) { $element.Value.ToString() } else { $element.Extent.Text }
        })
    } else {
        $tokens = @('rancher', $wordToComplete)
    }

    # Mirror bash.sh/zsh.sh/fish.sh: the in-progress word is dropped from the
    # request (so `rancher get po` asks for the "get" kind list, not "po"
    # itself) unless it looks like a flag, where it's kept so flag names
    # complete. Results are then filtered by prefix below, since (unlike
    # cmdlet parameter completion) native completers get no automatic
    # prefix filtering from PowerShell.
    $base = $tokens
    if ($tokens.Count -gt 0 -and $wordToComplete -ne '' -and $tokens[$tokens.Count - 1] -eq $wordToComplete) {
        $base = @($tokens | Select-Object -First ($tokens.Count - 1))
    }
    $requestTokens = @(if ($wordToComplete -like '-*') { @($base) + @($wordToComplete) } else { $base })

    $exe = $requestTokens[0]
    $rest = @($requestTokens | Select-Object -Skip 1)

    & $exe @rest --generate-shell-completion 2>$null | ForEach-Object {
        $parts = $_.Split(':', 2)
        if ($parts.Count -eq 2) {
            $completion = $parts[0].Trim()
            $description = $parts[1].Trim()
        } else {
            $completion = $_.Trim()
            $description = $_.Trim()
        }
        if ($completion -like "$wordToComplete*") {
            [System.Management.Automation.CompletionResult]::new($completion, $completion, 'ParameterValue', $description)
        }
    }
}
