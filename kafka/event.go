/**
 * Copyright 2016 Confluent Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package kafka

import (
	"fmt"
	"os"
	"unsafe"
)

/*
#include <stdlib.h>
#include "select_rdkafka.h"
#include "glue_rdkafka.h"


void chdrs_to_tmphdrs (glue_msg_t *gMsg) {
    size_t i = 0;
    const char *name;
    const void *val;
    size_t size;
    rd_kafka_headers_t *chdrs;

    if (rd_kafka_message_headers(gMsg->msg, &chdrs)) {
        gMsg->tmphdrs = NULL;
        gMsg->tmphdrsCnt = 0;
        return;
    }

    gMsg->tmphdrsCnt = rd_kafka_header_cnt(chdrs);
    gMsg->tmphdrs = malloc(sizeof(*gMsg->tmphdrs) * gMsg->tmphdrsCnt);

    while (!rd_kafka_header_get_all(chdrs, i,
                                    &gMsg->tmphdrs[i].key,
                                    &gMsg->tmphdrs[i].val,
                                    (size_t *)&gMsg->tmphdrs[i].size))
        i++;
}

rd_kafka_event_t *_rk_queue_poll (rd_kafka_queue_t *rkq, int timeoutMs,
                                  rd_kafka_event_type_t *evtype,
                                  glue_msg_t *gMsg,
                                  rd_kafka_event_t *prev_rkev) {
    rd_kafka_event_t *rkev;

    if (prev_rkev)
      rd_kafka_event_destroy(prev_rkev);

    rkev = rd_kafka_queue_poll(rkq, timeoutMs);
    *evtype = rd_kafka_event_type(rkev);

    if (*evtype == RD_KAFKA_EVENT_FETCH) {
        gMsg->msg = (rd_kafka_message_t *)rd_kafka_event_message_next(rkev);
        gMsg->ts = rd_kafka_message_timestamp(gMsg->msg, &gMsg->tstype);

        if (gMsg->want_hdrs)
            chdrs_to_tmphdrs(gMsg);
    }

    return rkev;
}
*/
import "C"

func chdrsToTmphdrs(gMsg *C.glue_msg_t) {
	C.chdrs_to_tmphdrs(gMsg)
}

// Event generic interface
type Event interface {
	// String returns a human-readable representation of the event
	String() string
}

// Specific event types

// Stats statistics event
type Stats struct {
	statsJSON string
}

func (e Stats) String() string {
	return e.statsJSON
}

// AssignedPartitions consumer group rebalance event: assigned partition set
type AssignedPartitions struct {
	Partitions []TopicPartition
}

func (e AssignedPartitions) String() string {
	return fmt.Sprintf("AssignedPartitions: %v", e.Partitions)
}

// RevokedPartitions consumer group rebalance event: revoked partition set
type RevokedPartitions struct {
	Partitions []TopicPartition
}

func (e RevokedPartitions) String() string {
	return fmt.Sprintf("RevokedPartitions: %v", e.Partitions)
}

// PartitionEOF consumer reached end of partition
// Needs to be explicitly enabled by setting the `enable.partition.eof`
// configuration property to true.
type PartitionEOF TopicPartition

func (p PartitionEOF) String() string {
	return fmt.Sprintf("EOF at %s", TopicPartition(p))
}

// OffsetsCommitted reports committed offsets
type OffsetsCommitted struct {
	Error   error
	Offsets []TopicPartition
}

func (o OffsetsCommitted) String() string {
	return fmt.Sprintf("OffsetsCommitted (%v, %v)", o.Error, o.Offsets)
}

// OAuthBearerTokenRefresh indicates token refresh is required
type OAuthBearerTokenRefresh struct {
	// Config is the value of the sasl.oauthbearer.config property
	Config string
}

func (o OAuthBearerTokenRefresh) String() string {
	return "OAuthBearerTokenRefresh"
}

// eventPoll polls an event from the handler's C rd_kafka_queue_t,
// translates it into an Event type and then sends on `channel` if non-nil, else returns the Event.
// term_chan is an optional channel to monitor along with producing to channel
// to indicate that `channel` is being terminated.
// returns (event Event, terminate Bool) tuple, where Terminate indicates
// if termChan received a termination event.
func (h *handle) eventPoll(channel chan Event, timeoutMs int, maxEvents int, termChan chan bool) (Event, bool) {

	var prevRkev *C.rd_kafka_event_t
	term := false

	var retval Event

	if channel == nil {
		maxEvents = 1
	}
out:
	for evcnt := 0; evcnt < maxEvents; evcnt++ {
		var evtype C.rd_kafka_event_type_t
		var gMsg C.glue_msg_t
		gMsg.want_hdrs = C.int8_t(bool2cint(h.msgFields.Headers))
		rkev := C._rk_queue_poll(h.rkq, C.int(timeoutMs), &evtype, &gMsg, prevRkev)
		prevRkev = rkev
		timeoutMs = 0

		retval = nil

		switch evtype {
		case C.RD_KAFKA_EVENT_DR:
			// Producer Delivery Report event
			// Each such event contains delivery reports for all
			// messages in the produced batch.
			// Forward delivery reports to per-message's response channel
			// or to the global Producer.Events channel, or none.
			rkmessages := make([]*C.rd_kafka_message_t, int(C.rd_kafka_event_message_count(rkev)))

			cnt := int(C.rd_kafka_event_message_array(rkev, (**C.rd_kafka_message_t)(unsafe.Pointer(&rkmessages[0])), C.size_t(len(rkmessages))))

			for _, rkmessage := range rkmessages[:cnt] {
				msg := h.newMessageFromC(rkmessage)
				var ch *chan Event

				if rkmessage._private != nil {
					// Find cgoif by id
					cg, found := h.cgoGet((int)((uintptr)(rkmessage._private)))
					if found {
						cdr := cg.(cgoDr)

						if cdr.deliveryChan != nil {
							ch = &cdr.deliveryChan
						}
						msg.Opaque = cdr.opaque
					}
				}

				if ch == nil && h.fwdDr {
					ch = &channel
				}

				if ch != nil {
					select {
					case *ch <- msg:
					case <-termChan:
						retval = nil
						term = true
						break out
					}

				} else {
					retval = msg
					break out
				}
			}
		case C.RD_KAFKA_EVENT_NONE:
			// poll timed out: no events available
			break out

		default:
			if rkev != nil {
				fmt.Fprintf(os.Stderr, "Ignored event %s\n",
					C.GoString(C.rd_kafka_event_name(rkev)))
			}

		}

		if retval != nil {
			if channel != nil {
				select {
				case channel <- retval:
				case <-termChan:
					retval = nil
					term = true
					break out
				}
			} else {
				break out
			}
		}
	}

	if prevRkev != nil {
		C.rd_kafka_event_destroy(prevRkev)
	}

	return retval, term
}
