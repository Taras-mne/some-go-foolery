// Package tunnel defines the framing protocol used between the relay server
// and the daemon over a single WebSocket connection.  Every HTTP request that
// arrives at the relay for a given user is serialised into a Request frame,
// sent through the WebSocket, handled by the daemon's local WebDAV server,
// and the result is sent back as a Response frame.  Multiplexing is achieved
// via a unique ID per request.
package tunnel

// Request is sent from the relay to the daemon.
type Request struct {
	ID      string              `json:"id"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

// Response is sent from the daemon back to the relay.
type Response struct {
	ID      string              `json:"id"`
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}
