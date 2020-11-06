package main

import (
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestRun(t *testing.T) {
	os.Setenv("BEARER_TOKEN", "") // disable bearer in tests
	err := run([]string{"git-ci"})
	require.Equal(t, err, flag.ErrHelp)
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
