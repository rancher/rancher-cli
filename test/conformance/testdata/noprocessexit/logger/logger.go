// Package logger holds one function per way a *log.Logger method that ends
// the process is reached, for the scan that refuses them.
package logger

import (
	"log"
	"log/slog"
	"os"
)

type embedded struct{ *log.Logger }

func callFatal()   { log.Default().Fatal("x") }
func callFatalf()  { log.Default().Fatalf("x") }
func callFatalln() { log.Default().Fatalln("x") }
func callPanic()   { log.Default().Panic("x") }
func callPanicf()  { log.Default().Panicf("x") }
func callPanicln() { log.Default().Panicln("x") }

func newLogger() { log.New(os.Stderr, "", 0).Fatalf("x") }

func variable() {
	l := log.New(os.Stderr, "", 0)
	l.Panicln("x")
}

func methodValue() {
	f := log.Default().Fatal
	f("x")
}

func methodExpression() { (*log.Logger).Fatalf(log.Default(), "x") }

func embeddedLogger(e embedded) { e.Panic("x") }

func slogLogger() {
	slog.NewLogLogger(slog.NewTextHandler(os.Stderr, nil), slog.LevelError).Fatal("x")
}
