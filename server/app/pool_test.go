package app

import (
	"reflect"
	"testing"
)

// TestPoolInstallsTheStatementTimeoutHook pins which hook poolConfig installs,
// not merely that one is installed. The config test checks AfterConnect for
// nil; a future hook that did something else would keep that green while every
// pooled connection ran with no statement ceiling, and the only other test that
// would notice needs a database. Function values do not compare in Go, so the
// code pointers are compared instead.
func TestPoolInstallsTheStatementTimeoutHook(t *testing.T) {
	pc, err := bootConfig(t).poolConfig()
	if err != nil {
		t.Fatalf("poolConfig: %v", err)
	}
	if pc.AfterConnect == nil {
		t.Fatal("no AfterConnect hook; pooled connections carry no statement_timeout")
	}
	got := reflect.ValueOf(pc.AfterConnect).Pointer()
	want := reflect.ValueOf(setStatementTimeout).Pointer()
	if got != want {
		t.Error("AfterConnect is not setStatementTimeout; nothing sets the pool's statement_timeout")
	}
}
