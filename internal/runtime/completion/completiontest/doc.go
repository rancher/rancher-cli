// Package completiontest carries the test doubles the live completion
// sources' tests share: a server that answers fixed bodies by path and
// records what was asked of it, and the fixture documents more than one
// package asserts against, so a fixture edited for one consumer cannot
// drift away from another's. Test code only imports it; the completion
// seam itself never does.
package completiontest
