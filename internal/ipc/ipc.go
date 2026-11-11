// Package ipc define el protocolo entre el demonio y la CLI.
//
// La comunicación se hace por un socket Unix intercambiando mensajes JSON,
// uno por petición y uno por respuesta, sobre una conexión persistente.
package ipc

import (
	"encoding/json"
	"fmt"
	"net"
)

// DefaultSocketPath es la ruta por defecto del socket del demonio.
// systemd crea /run/resguard gracias a RuntimeDirectory= en la unidad.
const DefaultSocketPath = "/run/resguard/resguard.sock"

// Request es una orden enviada por la CLI al demonio.
type Request struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// Response es la contestación del demonio.
type Response struct {
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Client mantiene una conexión abierta con el demonio.
type Client struct {
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
}

// Dial abre una conexión con el demonio en socketPath.
func Dial(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("no se pudo conectar con el demonio en %s: %w", socketPath, err)
	}
	return &Client{conn: conn, enc: json.NewEncoder(conn), dec: json.NewDecoder(conn)}, nil
}

// Call envía una orden y espera su respuesta.
func (c *Client) Call(command string, args ...string) (Response, error) {
	var resp Response
	if err := c.enc.Encode(Request{Command: command, Args: args}); err != nil {
		return resp, fmt.Errorf("enviando petición: %w", err)
	}
	if err := c.dec.Decode(&resp); err != nil {
		return resp, fmt.Errorf("leyendo respuesta: %w", err)
	}
	return resp, nil
}

// Close cierra la conexión.
func (c *Client) Close() error {
	return c.conn.Close()
}
