package qmp

import (
	"encoding/json"
	"errors"
	"net"
)

type Conn struct {
	socket        string // path to unix domain socket to connect to
	conn          net.Conn
	dec           *json.Decoder
	enc           *json.Encoder
	message_sync  chan map[string]interface{}
	message_async chan map[string]interface{}
}

// return an asynchronous message, blocking until one shows up
func (q *Conn) Message() map[string]interface{} {
	return <-q.message_async
}

func Dial(s string) (Conn, error) {
	var q Conn
	err := q.connect(s)
	return q, err
}

func (q *Conn) connect(s string) error {
	q.socket = s
	conn, err := net.Dial("unix", q.socket)
	if err != nil {
		return err
	}
	q.conn = conn
	q.dec = json.NewDecoder(q.conn)
	q.enc = json.NewEncoder(q.conn)
	q.message_sync = make(chan map[string]interface{}, 1024)
	q.message_async = make(chan map[string]interface{}, 1024)

	// upon connecting we should get the qmp version etc.
	v, err := q.read()
	if err != nil {
		return err
	}

	v = map[string]interface{}{
		"execute": "qmp_capabilities",
	}
	q.write(v)

	v, err = q.read()
	if err != nil {
		return err
	}
	if !success(v) {
		return errors.New("failed success")
	}

	go q.reader()

	return nil
}

func success(v map[string]interface{}) bool {
	for k, e := range v {
		if k != "return" {
			return false
		}
		switch t := e.(type) {
		case map[string]interface{}:
			if len(t) != 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (q *Conn) read() (map[string]interface{}, error) {
	var v map[string]interface{}
	err := q.dec.Decode(&v)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (q *Conn) write(v map[string]interface{}) error {
	err := q.enc.Encode(&v)
	return err
}

func (q *Conn) Status() (map[string]interface{}, error) {
	s := map[string]interface{}{
		"execute": "query-status",
	}
	q.write(s)
	v := <-q.message_sync
	status := v["return"]
	if status == nil {
		return nil, errors.New("received nil status")
	}
	return status.(map[string]interface{}), nil
}

func (q *Conn) Start() error {
	s := map[string]interface{}{
		"execute": "cont",
	}
	q.write(s)
	v := <-q.message_sync
	if !success(v) {
		return errors.New("could not start VM")
	}
	return nil
}

func (q *Conn) Stop() error {
	s := map[string]interface{}{
		"execute": "stop",
	}
	q.write(s)
	v := <-q.message_sync
	if !success(v) {
		return errors.New("could not stop VM")
	}
	return nil
}

func (q *Conn) Pmemsave(path string, size uint64) error {
	s := map[string]interface{}{
		"execute": "pmemsave",
		"arguments": map[string]interface{}{
			"val":      0,
			"size":     size,
			"filename": path,
		},
	}
	q.write(s)
	v := <-q.message_sync
	if !success(v) {
		return errors.New("pmemsave")
	}
	return nil
}

func (q *Conn) Blockdev_snapshot(path, device string) error {
	s := map[string]interface{}{
		"execute": "blockdev-snapshot",
		"arguments": map[string]interface{}{
			"device":        device,
			"snapshot-file": path,
			"format":        "raw",
		},
	}
	q.write(s)
	v := <-q.message_sync
	if !success(v) {
		return errors.New("blockdev_snapshot")
	}
	return nil
}

func (q *Conn) reader() {
	for {
		v, err := q.read()
		if err != nil {
			break
		}
		// split asynchronous and synchronous events.
		if v["event"] != nil {
			q.message_async <- v
		} else {
			q.message_sync <- v
		}
	}
}
