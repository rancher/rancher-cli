//go:build windows

package token

import "golang.org/x/sys/windows"

// osOpenURL opens openURL through the Windows ShellExecute API. ShellExecute
// takes the target as a string argument to the shell API: it builds no command
// line and spawns no cmd.exe, so a server-supplied authorize URL (which
// legitimately contains `&` between query parameters) cannot inject shell
// metacharacters. openBrowser has already validated the scheme.
func osOpenURL(openURL string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(openURL)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, target, nil, nil, windows.SW_SHOWNORMAL)
}
