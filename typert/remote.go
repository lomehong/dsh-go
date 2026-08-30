package typert

import "fmt"

// Failure is one stable Remote failure envelope carried across the wire.
type Failure struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// LookupFailure is returned by a resolver to surface an existing RPC
// failure — policy rejections such as cold-resume fences keep their
// original code instead of being wrapped as lookup-failed.
type LookupFailure struct {
	Failure Failure
}

// Error names the category and message.
func (e *LookupFailure) Error() string {
	return fmt.Sprintf("typert lookup failure %q: %s", e.Failure.Code, e.Failure.Message)
}

// RemoteFailure is one business Remote failure envelope raised through a
// Remote method and forwarded to the client unchanged.
type RemoteFailure struct {
	Failure Failure
}

// Error names the category and message.
func (e *RemoteFailure) Error() string {
	return fmt.Sprintf("typert remote failure %q: %s", e.Failure.Code, e.Failure.Message)
}
