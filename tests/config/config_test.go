package config

import (
	"testing"

	"github.com/jrockway/ekglue/pkg/glue"
)

func TestConfig(t *testing.T) {
	// I copy my config from production into this file occasionally.  If you want to test yours
	// here, please send a PR to do so!
	if _, err := glue.LoadConfig("ekglue.yaml"); err != nil {
		t.Error(err)
	}
}
