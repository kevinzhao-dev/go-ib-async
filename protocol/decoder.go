package protocol

import (
	"log"
	"strconv"
)

// Handler is a function that processes a decoded message.
type Handler func(msgID int, fields *FieldReader)

// Decoder parses inbound IB messages and dispatches to a Handler.
type Decoder struct {
	ServerVersion int
	handler       Handler
}

// NewDecoder creates a Decoder that dispatches all messages to the given handler.
func NewDecoder(serverVersion int, handler Handler) *Decoder {
	return &Decoder{
		ServerVersion: serverVersion,
		handler:       handler,
	}
}

// Interpret parses a raw field slice and dispatches to the handler.
func (d *Decoder) Interpret(fields []string) {
	if len(fields) == 0 {
		return
	}

	msgID, err := strconv.Atoi(fields[0])
	if err != nil {
		log.Printf("ibgo: invalid message ID %q: %v", fields[0], err)
		return
	}

	reader := NewFieldReader(fields)
	reader.Skip(1) // skip msgID field, handler knows it from msgID param

	defer func() {
		if r := recover(); r != nil {
			log.Printf("ibgo: panic in handler for msgID %d: %v", msgID, r)
		}
	}()

	d.handler(msgID, reader)
}
