package api

import "reflect"

// Head gives the header of a journal line. Each event struct embeds
// EventHeader, so a pointer to any event has this method; the journal
// writer sets the header keys through it.
func (h *EventHeader) Head() *EventHeader { return h }

// Event is a pointer to one event struct of zz_events.go.
type Event interface {
	Head() *EventHeader
}

// eventTypes maps the struct type of each event to its type.
var eventTypes = func() map[reflect.Type]EventType {
	m := map[reflect.Type]EventType{}
	for t, mk := range EventStructs {
		m[reflect.TypeOf(mk()).Elem()] = t
	}
	return m
}()

// TypeOfEvent gives the event type of an event struct, for example
// node.state for *NodeStateEvent, or "" for a value that is no event.
func TypeOfEvent(ev any) EventType {
	t := reflect.TypeOf(ev)
	if t == nil {
		return ""
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return eventTypes[t]
}
