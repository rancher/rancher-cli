package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMinRancherVersionFloor(t *testing.T) {
	assert.Equal(t, "2.14", MinRancherVersion)
}
