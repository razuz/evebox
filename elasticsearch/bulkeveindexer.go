/* Copyright (c) 2016 Jason Ish
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions
 * are met:
 *
 * 1. Redistributions of source code must retain the above copyright
 *    notice, this list of conditions and the following disclaimer.
 * 2. Redistributions in binary form must reproduce the above copyright
 *    notice, this list of conditions and the following disclaimer in the
 *    documentation and/or other materials provided with the distribution.
 *
 * THIS SOFTWARE IS PROVIDED ``AS IS'' AND ANY EXPRESS OR IMPLIED
 * WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
 * DISCLAIMED. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY DIRECT,
 * INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
 * (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
 * SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION)
 * HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT,
 * STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING
 * IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
 * POSSIBILITY OF SUCH DAMAGE.
 */

package elasticsearch

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/jasonish/evebox/eve"
	"github.com/jasonish/evebox/log"
	"github.com/satori/go.uuid"
)

const AtTimestampFormat = "2006-01-02T15:04:05.999Z"

type BulkEveIndexer struct {
	es         *ElasticSearch

	pipeReader *io.PipeReader
	pipeWriter *io.PipeWriter

	// Number of events queued in flight.
	queued uint

	wait sync.WaitGroup

	done bool

	channel chan interface{}
}

func NewIndexer(es *ElasticSearch) *BulkEveIndexer {

	indexer := BulkEveIndexer{
		es:               es,
		channel:          make(chan interface{}),
	}

	pipeReader, pipeWriter := io.Pipe()
	indexer.pipeReader = pipeReader
	indexer.pipeWriter = pipeWriter

	return &indexer
}

func (i *BulkEveIndexer) DecodeResponse(response *http.Response) (*BulkResponse, error) {

	var bulkResponse BulkResponse

	if response.StatusCode == 200 {
		decoder := json.NewDecoder(response.Body)
		decoder.UseNumber()
		err := decoder.Decode(&bulkResponse)
		if err != nil {
			return nil, err
		}
		return &bulkResponse, nil
	} else {
		log.Info("Got unexpected status code %d", response.StatusCode)
	}

	err := NewElasticSearchError(response)
	return nil, err
}

func (i *BulkEveIndexer) Run() error {

	for {
		if i.done {
			return nil
		}

		response, err := i.es.HttpClient.Post("_bulk",
			"application/json", i.pipeReader)
		if err != nil {
			return err
		}

		bulkResponse, err := i.DecodeResponse(response)
		response.Body.Close()

		// Sending done signal.
		if err != nil {
			i.channel <- err
		} else {
			i.channel <- bulkResponse
		}

		// Create new pipes for the next round...
		pipeReader, pipeWriter := io.Pipe()
		i.pipeReader = pipeReader
		i.pipeWriter = pipeWriter
	}

	return nil
}

func (i *BulkEveIndexer) IndexRawEvent(event eve.RawEveEvent) error {

	timestamp, err := event.GetTimestamp()
	if err != nil {
		return err
	}
	event["@timestamp"] = timestamp.UTC().Format(AtTimestampFormat)
	index := fmt.Sprintf("%s-%s", i.es.EventBaseIndex, timestamp.UTC().Format("2006.01.02"))

	header := BulkCreateHeader{}
	header.Create.Index = index
	header.Create.Type = "log"
	header.Create.Id = uuid.NewV1().String()

	encoder := json.NewEncoder(i.pipeWriter)

	encoder.Encode(&header)
	encoder.Encode(event)

	i.queued++

	return nil
}

func (i *BulkEveIndexer) Stop() {
	i.done = true
	i.FlushConnection()
}

func (i *BulkEveIndexer) FlushConnection() (*BulkResponse, error) {
	if i.queued == 0 {
		// Just return, there are no events left.
		return nil, nil
	}

	i.pipeWriter.Close()
	i.queued = 0

	for {
		result := <-i.channel
		switch result := result.(type) {
		case error:
			return nil, result.(error)
		case *BulkResponse:
			return result, nil
		}
		break
	}

	return nil, nil
}
