package nolibrary

import "fmt"

func show(err error) string { return err.Error() }

func wrap(err error) error { return fmt.Errorf("failed: %w", err) }
