package store

import (
	"encoding/json"
	"fmt"

	"censi/harness/internal/session"
)

// eventRecord is one line of a session log.
//
// The payload is kept as raw JSON rather than a decoded value so that writing an
// event never depends on knowing what it contains. Reading does, and that is
// where the type registry below comes in.
type eventRecord struct {
	Seq     int                `json:"seq"`
	Type    string             `json:"type"`
	Time    int64              `json:"time"`
	Payload json.RawMessage    `json:"payload,omitempty"`
	Surface *session.SurfaceOp `json:"surface,omitempty"`
	Sources []int              `json:"sources,omitempty"`
}

// encodeEvent converts a live event into a record.
func encodeEvent(event session.Event) (eventRecord, error) {
	record := eventRecord{
		Seq:     int(event.Seq),
		Type:    event.Type,
		Time:    event.Time,
		Surface: event.Surface,
	}

	for _, source := range event.SourceEventSeqs {
		record.Sources = append(record.Sources, int(source))
	}

	if event.Payload != nil {
		data, err := json.Marshal(event.Payload)
		if err != nil {
			return eventRecord{}, fmt.Errorf("store: encoding the payload of %s: %w", event.Type, err)
		}

		record.Payload = data
	}

	return record, nil
}

// messageTypes are the events whose payload must decode back into a
// session.Message.
//
// Getting this list wrong is not a cosmetic failure: the projection switches on
// the payload's type, so an event that decodes into the wrong shape is either
// dropped from the derived history or panics - and the derived history is the
// request prefix the provider cached.
var messageTypes = map[string]bool{
	session.EventSystemMessage:    true,
	session.EventDeveloperMessage: true,
	session.EventUserMessage:      true,
	session.EventAssistantMessage: true,
	session.EventToolResult:       true,
}

// decodePayload reconstructs a payload for replay.
func decodePayload(eventType string, raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	if messageTypes[eventType] {
		var message session.Message
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, fmt.Errorf("store: decoding the payload of %s: %w", eventType, err)
		}

		return message, nil
	}

	// Anything else is carried as a generic object. It is preserved faithfully
	// without this package needing to know the shape - which matters because the
	// alternative is a registry that silently falls behind every new event type.
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("store: decoding the payload of %s: %w", eventType, err)
	}

	return generic, nil
}
