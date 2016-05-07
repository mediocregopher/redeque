// Package peel contains the actual application specific logic for each
// command which can be performed. It's designed to be able to be used as an
// actual client for bananaq if desired.
package peel

import (
	"time"

	"github.com/mediocregopher/bananaq/core"
)

// Peel contains all the information needed to actually implement the
// application logic of bananaq. it is intended to be used both as the server
// component and as a client for external applications which want to be able to
// interact with the database directly. It can be initialized manually, or using
// the New method. All methods on Peel are thread-safe.
type Peel struct {
	c core.Core
}

// New initializes a Peel struct with a Core, using the given redis address and
// pool size. The redis address can be a standalone node or a node in a cluster.
func New(redisAddr string, poolSize int) (Peel, error) {
	c, err := core.New(redisAddr, poolSize)
	return Peel{c}, err
}

// Client describes the information attached to any given client of bananaq.
// For most commands this isn't actually necessary except for logging, but for
// some it's actually used (these will be documented as such)
type Client struct {
	ID string
}

// QAddCommand describes the parameters which can be passed into the QAdd
// command
type QAddCommand struct {
	Client
	Queue    string    // Required
	Expire   time.Time // Required
	Contents string    // Required
}

// QAdd adds an event to a queue. The Expire will be used to generate an ID.
// IDs are monotonically increasing and unique across the cluster, so the ID may
// correspond to a very slightly greater time than the given Expire. That
// (potentially slightly greater) time will also be used as the point in time
// after which the event is no longer valid.
func (p Peel) QAdd(c QAddCommand) (core.ID, error) {
	e, err := p.c.NewEvent(c.Expire, c.Contents)
	if err != nil {
		return 0, err
	}

	// We always store the event data itself with an extra 30 seconds until it
	// expires, just in case a consumer gets it just as its expire time hits
	if err := p.c.SetEvent(e, 30*time.Second); err != nil {
		return 0, err
	}

	es := queueAvailable(c.Queue)

	qa := core.QueryActions{
		EventSetBase: es.Base,
		QueryActions: []core.QueryAction{
			{
				QuerySelector: &core.QuerySelector{
					EventSet: es,
					Events:   []core.Event{e},
				},
			},
			{
				QueryAddTo: &core.QueryAddTo{
					EventSets: []core.EventSet{es},
				},
			},
		},
	}
	if _, err := p.c.Query(qa); err != nil {
		return 0, err
	}

	return e.ID, nil
}

// QGetCommand describes the parameters which can be passed into the QGet
// command
type QGetCommand struct {
	Client
	Queue         string // Required
	ConsumerGroup string // Required
	AckDeadline   time.Time
}

// QGet retrieves an available event from the given queue for the given consumer
// group. If AckDeadline is given, then the consumer has until then to QAck the
// Event before it is placed back in the queue for this consumer group. If
// AckDeadline is not set, then the Event will never be placed back, and QAck
// isn't necessary..  An empty event is returned if there are no available
// events for the queue.
func (p Peel) QGet(c QGetCommand) (core.Event, error) {
	now := time.Now()
	esAvail := queueAvailable(c.Queue)
	esInProgID := queueInProgressByID(c.Queue, c.ConsumerGroup)
	esInProgAck := queueInProgressByAck(c.Queue, c.ConsumerGroup)
	esRedo := queueRedo(c.Queue, c.ConsumerGroup)
	esDone := queueDone(c.Queue, c.ConsumerGroup)

	// Depending on if Expire is set, we might add the event to the inProgs or
	// done
	var inProgOrDone []core.QueryAction
	if !c.AckDeadline.IsZero() {
		inProgOrDone = []core.QueryAction{
			{
				QueryAddTo: &core.QueryAddTo{
					EventSets: []core.EventSet{esInProgID},
				},
			},
			{
				QueryAddTo: &core.QueryAddTo{
					EventSets: []core.EventSet{esInProgAck},
					Score:     core.NewTS(c.AckDeadline),
				},
			},
		}
	} else {
		inProgOrDone = []core.QueryAction{
			{
				QueryAddTo: &core.QueryAddTo{
					EventSets: []core.EventSet{esDone},
				},
			},
		}
	}

	breakIfFound := core.QueryAction{
		Break: true,
		QueryConditional: core.QueryConditional{
			IfInput: true,
		},
	}

	mostRecentSelect := core.QueryEventRangeSelect{
		Min:     core.NewTS(now),
		MinExcl: true,
		Max:     0,
		Limit:   1,
		Reverse: true,
	}
	oldestSelect := mostRecentSelect
	oldestSelect.Reverse = false

	var qq []core.QueryAction
	// First, if there's any Events in redo, we grab the first one from there
	// and move it to inProg/done
	qq = append(qq,
		core.QueryAction{
			QuerySelector: &core.QuerySelector{
				EventSet:              esRedo,
				QueryEventRangeSelect: &oldestSelect,
			},
		},
		core.QueryAction{
			RemoveFrom: []core.EventSet{esRedo},
		},
	)
	qq = append(qq, inProgOrDone...)
	qq = append(qq, breakIfFound)

	// Otherwise, we grab the most recent Events from both inProgByID and done
	qq = append(qq,
		core.QueryAction{
			QuerySelector: &core.QuerySelector{
				EventSet:              esInProgID,
				QueryEventRangeSelect: &mostRecentSelect,
			},
		},
		core.QueryAction{
			QuerySelector: &core.QuerySelector{
				EventSet:              esDone,
				QueryEventRangeSelect: &mostRecentSelect,
				Union: true,
			},
		},
		core.QueryAction{
			QuerySelector: &core.QuerySelector{
				EventSet: esAvail,
				QueryEventRangeSelect: &core.QueryEventRangeSelect{
					MinFromInput: true,
					MinExcl:      true,
					Limit:        1,
				},
			},
		},
	)

	// If we got an event from before, add it to inProgs/done and return
	qq = append(qq, inProgOrDone...)
	qq = append(qq, breakIfFound)

	// The queue has no activity, simply get the first event in avail. Only
	// applies if both done and inProg are actually empty. If they're not and
	// we're here it means that the queue has simply been fully processed
	// thusfar
	qq = append(qq, core.QueryAction{
		QuerySelector: &core.QuerySelector{
			EventSet:       esAvail,
			PosRangeSelect: []int64{0, 0},
		},
		QueryConditional: core.QueryConditional{
			And: []core.QueryConditional{
				{
					IfEmpty: &esDone,
				},
				{
					IfEmpty: &esInProgID,
				},
			},
		},
	})

	qa := core.QueryActions{
		EventSetBase: esAvail.Base,
		QueryActions: qq,
		Now:          now,
	}

	ee, err := p.c.Query(qa)
	if err != nil {
		return core.Event{}, err
	} else if len(ee) == 0 {
		return core.Event{}, nil
	}

	return p.c.GetEvent(ee[0].ID)
}

// QAckCommand describes the parameters which can be passed into the QAck
// command
type QAckCommand struct {
	Client
	Queue         string     // Required
	ConsumerGroup string     // Required
	Event         core.Event // Required, Contents field optional
}

// QAck acknowledges that an event has been successfully processed and should
// not be re-processed. Only applicable for Events which were gotten through a
// QGet with an AckDeadline. Returns true if the Event was successfully
// acknowledged. false will be returned if the deadline was missed, and
// therefore some other consumer may re-process the Event later.
func (p Peel) QAck(c QAckCommand) (bool, error) {
	now := time.Now()

	esInProgID := queueInProgressByID(c.Queue, c.ConsumerGroup)
	esInProgAck := queueInProgressByAck(c.Queue, c.ConsumerGroup)
	esDone := queueDone(c.Queue, c.ConsumerGroup)

	qa := core.QueryActions{
		EventSetBase: esDone.Base,
		QueryActions: []core.QueryAction{
			{
				QuerySelector: &core.QuerySelector{
					EventSet: esInProgAck,
					QueryEventScoreSelect: &core.QueryEventScoreSelect{
						Event: c.Event,
						Min:   core.NewTS(now),
					},
				},
			},
			{
				Break: true,
				QueryConditional: core.QueryConditional{
					IfNoInput: true,
				},
			},
			{
				RemoveFrom: []core.EventSet{esInProgID, esInProgAck},
			},
			{
				QueryAddTo: &core.QueryAddTo{
					EventSets: []core.EventSet{esDone},
				},
			},
		},
		Now: now,
	}

	ee, err := p.c.Query(qa)
	if err != nil {
		return false, err
	}
	return len(ee) > 0, nil
}