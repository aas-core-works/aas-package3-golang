//go:build debug
// +build debug

package aasx

import (
	"strings"
	"testing"
)

func TestRequireDoesNotPanicIfOk(t *testing.T) {
	// Should not panic
	Require(true, "something")
}

func TestRequirePanicsIfConditionFails(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("Expected panic but got none")
		} else {
			// Verify it's a precondition violation
			msg, ok := r.(string)
			if !ok || !strings.Contains(msg, "precondition violation") {
				t.Errorf("Expected precondition violation panic, got: %v", r)
			}
		}
	}()

	Require(false, "something")
}

func TestEnsureDoesNotPanicIfOk(t *testing.T) {
	// Should not panic
	Ensure(true, "something")
}

func TestEnsurePanicsIfConditionFails(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("Expected panic but got none")
		} else {
			// Verify it's a postcondition violation
			msg, ok := r.(string)
			if !ok || !strings.Contains(msg, "postcondition violation") {
				t.Errorf("Expected postcondition violation panic, got: %v", r)
			}
		}
	}()

	Ensure(false, "something")
}
