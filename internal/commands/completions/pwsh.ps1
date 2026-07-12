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
    param($commandName, $wordToComplete, $cursorPosition)
    $other = "$wordToComplete --generate-shell-completion"
    Invoke-Expression $other | ForEach-Object {
        $parts = $_.Split(':', 2)
        if ($parts.Count -eq 2) {
            $completion = $parts[0].Trim()
            $description = $parts[1].Trim()
            [System.Management.Automation.CompletionResult]::new($completion, $completion, 'ParameterValue', $description)
        } else {
            [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
        }
    }
}
