package aasx

import (
	"errors"
)

// ErrPreconditionViolation is returned when a precondition check fails.
var ErrPreconditionViolation = errors.New("precondition violation")

// ErrPostconditionViolation is returned when a postcondition check fails.
var ErrPostconditionViolation = errors.New("postcondition violation")

// Require checks a precondition and panics with the given message if it fails.
// Use this for validating function arguments and entry conditions.
func Require(condition bool, message string) {
	if !condition {
		panic(ErrPreconditionViolation.Error() + ": " + message)
	}
}

// Ensure checks a postcondition and panics with the given message if it fails.
// Use this for validating function results and exit conditions.
func Ensure(condition bool, message string) {
	if !condition {
		panic(ErrPostconditionViolation.Error() + ": " + message)
	}
}
