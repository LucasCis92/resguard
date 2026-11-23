// Package ipc defines the protocol between the daemon and the CLI.
//
// They talk over a Unix socket exchanging JSON messages, one per request and
// one per response, over a persistent connection.
package ipc

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/LucasCis92/resguard/internal/collector"
	"github.com/LucasCis92/resguard/internal/events"
	"github.com/LucasCis92/resguard/internal/metrics"
)

// DefaultSocketPath is the daemon's default socket path. systemd creates
// /run/resguard thanks to RuntimeDirectory= in the unit.
const DefaultSocketPath = "/run/resguard/resguard.sock"

// Request is a command sent by the CLI to the daemon. Lang is the language
// ("en", "es") the caller wants text answers in; empty means English.
type Request struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Lang    string   `json:"lang,omitempty"`
}

// Response is the daemon's answer. Output is text ready to print; Data, when
// present, is structured data for the CLI to present its own way (e.g. the
// "watch" view).
type Response struct {
	OK     bool            `json:"ok"`
	Output string          `json:"output,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Decode unmarshals Data into v.
func (r Response) Decode(v any) error {
	if len(r.Data) == 0 {
		return fmt.Errorf("the response carries no data")
	}
	return json.Unmarshal(r.Data, v)
}

// CommandInfo describes a daemon command (the data answer of "help").
type CommandInfo struct {
	Name        string `json:"name"`
	Usage       string `json:"usage,omitempty"`
	Description string `json:"description,omitempty"`
}

// ProcessList holds the processes of one or more units ("processes" command).
// The CLI computes each process's CPU usage by comparing CPUTicks between two
// answers.
type ProcessList struct {
	Time      time.Time         `json:"time"`
	NumCPU    int               `json:"num_cpu"`
	Units     []string          `json:"units"` // units that were found
	Processes []metrics.Process `json:"processes"`
}

// Snapshot is the full state used by the "watch" view ("snapshot" command).
type Snapshot struct {
	Sample   collector.Sample          `json:"sample"`
	Services []collector.ServiceSample `json:"services"`
	Events   []events.Event            `json:"events"`
	Interval time.Duration             `json:"interval"`
}

// Client keeps a connection open to the daemon.
type Client struct {
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder

	// Lang is sent with every request so the daemon answers in it.
	Lang string
}

// Dial opens a connection to the daemon at socketPath.
func Dial(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("connecting to the daemon at %s: %w", socketPath, err)
	}
	return &Client{conn: conn, enc: json.NewEncoder(conn), dec: json.NewDecoder(conn)}, nil
}

// Call sends a command and waits for its answer.
func (c *Client) Call(command string, args ...string) (Response, error) {
	var resp Response
	if err := c.enc.Encode(Request{Command: command, Args: args, Lang: c.Lang}); err != nil {
		return resp, fmt.Errorf("sending request: %w", err)
	}
	if err := c.dec.Decode(&resp); err != nil {
		return resp, fmt.Errorf("reading response: %w", err)
	}
	return resp, nil
}

// Close closes the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}
