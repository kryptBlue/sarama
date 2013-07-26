/*
Package protocol provides the low-level primitives necessary for communicating with a Kafka 0.8 cluster.

The core of the package is the Broker. It represents a connection to a single Kafka broker service, and
has methods for querying the broker.

The other types are mostly Request types or Response types. Most of the Broker methods take a Request of a
specific type and return a Response of the appropriate type.

The objects and properties in this package are mostly undocumented, as they line up exactly with the
protocol fields documented by Kafka at https://cwiki.apache.org/confluence/display/KAFKA/A+Guide+To+The+Kafka+Protocol
*/
package protocol

import enc "sarama/encoding"
import "sarama/types"
import (
	"io"
	"net"
	"sync"
)

// Broker represents a single Kafka broker connection. All operations on this object are entirely concurrency-safe.
type Broker struct {
	id   int32
	host string
	port int32

	correlation_id int32
	conn           net.Conn
	lock           sync.Mutex

	responses chan responsePromise
	done      chan bool
}

type responsePromise struct {
	correlation_id int32
	packets        chan []byte
	errors         chan error
}

// NewBroker creates and returns a Broker targetting the given host:port address.
// This does not attempt to actually connect, you have to call Connect() for that.
func NewBroker(host string, port int32) *Broker {
	b := new(Broker)
	b.id = -1 // don't know it yet
	b.host = host
	b.port = port
	return b
}

func (b *Broker) Connect() error {
	b.lock.Lock()
	defer b.lock.Unlock()

	if b.conn != nil {
		return AlreadyConnected
	}

	addr, err := net.ResolveIPAddr("ip", b.host)
	if err != nil {
		return err
	}

	b.conn, err = net.DialTCP("tcp", nil, &net.TCPAddr{IP: addr.IP, Port: int(b.port), Zone: addr.Zone})
	if err != nil {
		return err
	}

	b.done = make(chan bool)

	// permit a few outstanding requests before we block waiting for responses
	b.responses = make(chan responsePromise, 4)

	go b.responseReceiver()

	return nil
}

func (b *Broker) Close() error {
	b.lock.Lock()
	defer b.lock.Unlock()

	if b.conn == nil {
		return NotConnected
	}

	close(b.responses)
	<-b.done

	err := b.conn.Close()

	b.conn = nil
	b.done = nil
	b.responses = nil

	return err
}

// ID returns the broker ID retrieved from Kafka's metadata, or -1 if that is not known.
func (b *Broker) ID() int32 {
	return b.id
}

// Equals compares two brokers. Two brokers are considered equal if they have the same host, port, and id,
// or if they are both nil.
func (b *Broker) Equals(a *Broker) bool {
	switch {
	case a == nil && b == nil:
		return true
	case (a == nil && b != nil) || (a != nil && b == nil):
		return false
	}
	return a.id == b.id && a.host == b.host && a.port == b.port
}

func (b *Broker) GetMetadata(clientID string, request *MetadataRequest) (*MetadataResponse, error) {
	response := new(MetadataResponse)

	err := b.sendAndReceive(clientID, request, response)

	if err != nil {
		return nil, err
	}

	return response, nil
}

func (b *Broker) GetAvailableOffsets(clientID string, request *OffsetRequest) (*OffsetResponse, error) {
	response := new(OffsetResponse)

	err := b.sendAndReceive(clientID, request, response)

	if err != nil {
		return nil, err
	}

	return response, nil
}

func (b *Broker) Produce(clientID string, request *ProduceRequest) (*ProduceResponse, error) {
	var response *ProduceResponse
	var err error

	if request.RequiredAcks == types.NO_RESPONSE {
		err = b.sendAndReceive(clientID, request, nil)
	} else {
		response = new(ProduceResponse)
		err = b.sendAndReceive(clientID, request, response)
	}

	if err != nil {
		return nil, err
	}

	return response, nil
}

func (b *Broker) Fetch(clientID string, request *FetchRequest) (*FetchResponse, error) {
	response := new(FetchResponse)

	err := b.sendAndReceive(clientID, request, response)

	if err != nil {
		return nil, err
	}

	return response, nil
}

func (b *Broker) CommitOffset(clientID string, request *OffsetCommitRequest) (*OffsetCommitResponse, error) {
	response := new(OffsetCommitResponse)

	err := b.sendAndReceive(clientID, request, response)

	if err != nil {
		return nil, err
	}

	return response, nil
}

func (b *Broker) FetchOffset(clientID string, request *OffsetFetchRequest) (*OffsetFetchResponse, error) {
	response := new(OffsetFetchResponse)

	err := b.sendAndReceive(clientID, request, response)

	if err != nil {
		return nil, err
	}

	return response, nil
}

func (b *Broker) send(clientID string, req requestEncoder, promiseResponse bool) (*responsePromise, error) {
	b.lock.Lock()
	defer b.lock.Unlock()

	if b.conn == nil {
		return nil, NotConnected
	}

	fullRequest := request{b.correlation_id, clientID, req}
	buf, err := enc.Encode(&fullRequest)
	if err != nil {
		return nil, err
	}

	_, err = b.conn.Write(buf)
	if err != nil {
		return nil, err
	}
	b.correlation_id++

	if !promiseResponse {
		return nil, nil
	}

	promise := responsePromise{fullRequest.correlation_id, make(chan []byte), make(chan error)}
	b.responses <- promise

	return &promise, nil
}

func (b *Broker) sendAndReceive(clientID string, req requestEncoder, res enc.Decoder) error {
	promise, err := b.send(clientID, req, res != nil)

	if err != nil {
		return err
	}

	if promise == nil {
		return nil
	}

	select {
	case buf := <-promise.packets:
		return enc.Decode(buf, res)
	case err = <-promise.errors:
		return err
	}
}

func (b *Broker) Decode(pd enc.PacketDecoder) (err error) {
	b.id, err = pd.GetInt32()
	if err != nil {
		return err
	}

	b.host, err = pd.GetString()
	if err != nil {
		return err
	}

	b.port, err = pd.GetInt32()
	if err != nil {
		return err
	}

	return nil
}

func (b *Broker) responseReceiver() {
	header := make([]byte, 8)
	for response := range b.responses {
		_, err := io.ReadFull(b.conn, header)
		if err != nil {
			response.errors <- err
			continue
		}

		decodedHeader := responseHeader{}
		err = enc.Decode(header, &decodedHeader)
		if err != nil {
			response.errors <- err
			continue
		}
		if decodedHeader.correlation_id != response.correlation_id {
			response.errors <- enc.DecodingError
			continue
		}

		buf := make([]byte, decodedHeader.length-4)
		_, err = io.ReadFull(b.conn, buf)
		if err != nil {
			response.errors <- err
			continue
		}

		response.packets <- buf
	}
	close(b.done)
}
