package main

import "go.uber.org/zap"

// nopLogger returns a no-op zap.Logger for tests so we don't pollute
// stdout. Defined here rather than in main_test.go to keep the boot
// test file free of plumbing.
func nopLogger() *zap.Logger { return zap.NewNop() }
