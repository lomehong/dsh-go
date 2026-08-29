package schedule

import (
	"errors"
	"time"
)

// defaultNow is the production wall clock in epoch milliseconds.
func defaultNow() int64 { return time.Now().UnixMilli() }

// errRequired builds the loud missing-dependency error.
func errRequired(dependency string) error {
	return errors.New("schedule: a " + dependency + " is required")
}
