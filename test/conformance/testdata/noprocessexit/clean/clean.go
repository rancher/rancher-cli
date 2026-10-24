// Package clean holds methods named like the ones the scan refuses on
// *log.Logger, which it must not match, and a package-level log.Fatal,
// which the other rule covers.
package clean

import (
	"log"
	"testing"
)

type own struct{}

func (own) Fatal(...any) {}

func fromTB(tb testing.TB) {
	tb.Fatal("x")
	tb.Fatalf("x")
}

func ownMethod() { own{}.Fatal("x") }

func packageLevel() { log.Fatal("x") }
