package rpc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

type LineConn struct {
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
}

func Dial(socketPath string, timeout time.Duration) (*LineConn, error) {
	connection, err := net.DialTimeout("unix", socketPath, timeout)
	if err != nil {
		return nil, err
	}
	return &LineConn{
		conn:   connection,
		reader: bufio.NewReader(connection),
		writer: bufio.NewWriter(connection),
	}, nil
}

func Call(socketPath string, request RequestEnvelope, timeout time.Duration) (ResponseEnvelope, error) {
	connection, err := Dial(socketPath, timeout)
	if err != nil {
		return ResponseEnvelope{}, err
	}
	defer connection.Close()

	if err := connection.SetDeadline(time.Now().Add(timeout)); err != nil {
		return ResponseEnvelope{}, err
	}
	if err := connection.WriteJSON(request); err != nil {
		return ResponseEnvelope{}, err
	}

	for {
		raw, err := connection.ReadRawLine()
		if err != nil {
			return ResponseEnvelope{}, err
		}
		messageType, err := PeekMessageType(raw)
		if err != nil {
			return ResponseEnvelope{}, err
		}
		if messageType == "event" {
			continue
		}

		response, err := ParseResponse(raw)
		if err != nil {
			return ResponseEnvelope{}, err
		}
		if response.ID != request.ID {
			continue
		}
		return response, nil
	}
}

func OpenWatch(socketPath string, request RequestEnvelope, ackTimeout time.Duration) (*LineConn, ResponseEnvelope, error) {
	connection, err := Dial(socketPath, ackTimeout)
	if err != nil {
		return nil, ResponseEnvelope{}, err
	}

	if err := connection.SetReadDeadline(time.Now().Add(ackTimeout)); err != nil {
		connection.Close()
		return nil, ResponseEnvelope{}, err
	}
	if err := connection.WriteJSON(request); err != nil {
		connection.Close()
		return nil, ResponseEnvelope{}, err
	}

	for {
		raw, err := connection.ReadRawLine()
		if err != nil {
			connection.Close()
			return nil, ResponseEnvelope{}, err
		}
		messageType, err := PeekMessageType(raw)
		if err != nil {
			connection.Close()
			return nil, ResponseEnvelope{}, err
		}
		if messageType == "event" {
			continue
		}

		response, err := ParseResponse(raw)
		if err != nil {
			connection.Close()
			return nil, ResponseEnvelope{}, err
		}
		if response.ID != request.ID {
			continue
		}
		if err := connection.ClearReadDeadline(); err != nil {
			connection.Close()
			return nil, ResponseEnvelope{}, err
		}
		return connection, response, nil
	}
}

func (connection *LineConn) SetDeadline(deadline time.Time) error {
	return connection.conn.SetDeadline(deadline)
}

func (connection *LineConn) SetReadDeadline(deadline time.Time) error {
	return connection.conn.SetReadDeadline(deadline)
}

func (connection *LineConn) ClearReadDeadline() error {
	return connection.conn.SetReadDeadline(time.Time{})
}

func (connection *LineConn) WriteJSON(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return connection.WriteRawLine(payload)
}

func (connection *LineConn) WriteRawLine(payload []byte) error {
	if _, err := connection.writer.Write(payload); err != nil {
		return err
	}
	if err := connection.writer.WriteByte('\n'); err != nil {
		return err
	}
	return connection.writer.Flush()
}

func (connection *LineConn) ReadRawLine() ([]byte, error) {
	line, err := connection.reader.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return trimLine(line), nil
}

func (connection *LineConn) Close() error {
	return connection.conn.Close()
}

func PeekMessageType(raw []byte) (string, error) {
	var candidate struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &candidate); err != nil {
		return "", err
	}
	if candidate.Type == "" {
		return "", errors.New("rpc payload missing type")
	}
	return candidate.Type, nil
}

func ParseResponse(raw []byte) (ResponseEnvelope, error) {
	var response ResponseEnvelope
	if err := json.Unmarshal(raw, &response); err != nil {
		return ResponseEnvelope{}, err
	}
	if response.Type != "response" {
		return ResponseEnvelope{}, fmt.Errorf("expected response envelope, received %s", response.Type)
	}
	return response, nil
}

func ParseEvent(raw []byte) (EventEnvelope, error) {
	var event EventEnvelope
	if err := json.Unmarshal(raw, &event); err != nil {
		return EventEnvelope{}, err
	}
	if event.Type != "event" {
		return EventEnvelope{}, fmt.Errorf("expected event envelope, received %s", event.Type)
	}
	return event, nil
}

func trimLine(raw []byte) []byte {
	for len(raw) > 0 {
		last := raw[len(raw)-1]
		if last != '\n' && last != '\r' {
			break
		}
		raw = raw[:len(raw)-1]
	}
	return raw
}
