package main

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestRun(t *testing.T) {
	err := run([]string{"git-ci"})
	require.Equal(t, err, flag.ErrHelp)
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
