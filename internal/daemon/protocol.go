package daemon

import "encoding/json"

const (
	DaemonRequestTimeoutMS = 60_000
)

type RequestEnvelope struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	Type   string          `json:"type"`
}

type ResponseEnvelope struct {
	Error  string          `json:"error,omitempty"`
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Type   string          `json:"type"`
}

type EventEnvelope struct {
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
	Type    string          `json:"type"`
}

func MustMarshalJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
