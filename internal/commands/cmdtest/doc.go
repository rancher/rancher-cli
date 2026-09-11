// Package cmdtest holds the test fakes and fixtures the local command
// packages share: a runtime.Input built from maps, a canned-stdin
// prompter, a runtime.ConfigView over fixed names, the fake credential
// helper, and the config-lock ordering harness. Test-only: no production
// package imports it.
package cmdtest
