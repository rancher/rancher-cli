package stdout

import (
	"fmt"
	"io"
	"os"
)

func variable() { _, _ = os.Stdout.Write(nil) }

func call() { fmt.Println("text") }

func printf() { fmt.Printf("text") }

func value() func(...any) (int, error) { return fmt.Print }

func toStdout() { _, _ = fmt.Fprintln(os.Stdout, "text") }

func formatted() string { return fmt.Sprintf("text") }

func toWriter(w io.Writer) { _, _ = fmt.Fprintln(w, "text") }

func standardError() { _, _ = os.Stderr.Write(nil) }
