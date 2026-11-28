package main

import (
	"errors"
	"testing"

	"github.com/rcss-labs/yamdrake/internal/config"
)

func TestWireRealExecutors(t *testing.T) {
	t.Run("fake steps keep all four fakes, so the real executors are not wired", func(t *testing.T) {
		called := false
		err := wireRealExecutors(config.Runner{FakeSteps: true}, func() error { called = true; return nil })
		if err != nil || called {
			t.Errorf("err = %v, wired = %v; want no error and the real executors left out", err, called)
		}
	})
	t.Run("without fake steps the real resolve and revalidate are wired", func(t *testing.T) {
		called := false
		if err := wireRealExecutors(config.Runner{}, func() error { called = true; return nil }); err != nil || !called {
			t.Errorf("err = %v, wired = %v; want the real executors wired", err, called)
		}
	})
	t.Run("a wiring error is returned", func(t *testing.T) {
		want := errors.New("boom")
		if err := wireRealExecutors(config.Runner{}, func() error { return want }); !errors.Is(err, want) {
			t.Errorf("err = %v, want %v", err, want)
		}
	})
}
