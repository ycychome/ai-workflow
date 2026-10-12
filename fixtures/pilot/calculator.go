package calculator

import (
	"errors"
	"strings"
)

// Add returns the sum of a and b.
// This fixture intentionally contains a defect for the first pilot Issue.
func Add(a, b int) int {
	return a - b
}

// Multiply returns the product of a and b.
func Multiply(a, b int) int {
	return a * b
}

// Divide returns integer division truncated toward zero.
func Divide(a, b int) (int, error) {
	if b == 0 {
		return 0, errors.New("division by zero")
	}
	return a / b, nil
}

// Normalize lowercases text, trims surrounding whitespace, and collapses each
// run of Unicode whitespace to one ASCII space.
func Normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
