package server

import (
	"bytes"

	"github.com/micro/go-micro/codec"
	raw "github.com/micro/go-micro/codec/bytes"
	"github.com/micro/go-micro/codec/grpc"
	"github.com/micro/go-micro/codec/json"
	"github.com/micro/go-micro/codec/jsonrpc"
	"github.com/micro/go-micro/codec/proto"
	"github.com/micro/go-micro/codec/protorpc"
	"github.com/micro/go-micro/transport"
	"github.com/pkg/errors"
)

type rpcCodec struct {
	socket transport.Socket
	codec  codec.Codec
	first  bool

	req *transport.Message
	buf *readWriteCloser
}

type readWriteCloser struct {
	wbuf *bytes.Buffer
	rbuf *bytes.Buffer
}

var (
	DefaultContentType = "application/protobuf"

	DefaultCodecs = map[string]codec.NewCodec{
		"application/grpc":         grpc.NewCodec,
		"application/grpc+json":    grpc.NewCodec,
		"application/grpc+proto":   grpc.NewCodec,
		"application/json":         json.NewCodec,
		"application/json-rpc":     jsonrpc.NewCodec,
		"application/protobuf":     proto.NewCodec,
		"application/proto-rpc":    protorpc.NewCodec,
		"application/octet-stream": raw.NewCodec,
	}

	// TODO: remove legacy codec list
	defaultCodecs = map[string]codec.NewCodec{
		"application/json":         jsonrpc.NewCodec,
		"application/json-rpc":     jsonrpc.NewCodec,
		"application/protobuf":     protorpc.NewCodec,
		"application/proto-rpc":    protorpc.NewCodec,
		"application/octet-stream": protorpc.NewCodec,
	}
)

func (rwc *readWriteCloser) Read(p []byte) (n int, err error) {
	return rwc.rbuf.Read(p)
}

func (rwc *readWriteCloser) Write(p []byte) (n int, err error) {
	return rwc.wbuf.Write(p)
}

func (rwc *readWriteCloser) Close() error {
	rwc.rbuf.Reset()
	rwc.wbuf.Reset()
	return nil
}

// setupProtocol sets up the old protocol
func setupProtocol(msg *transport.Message) codec.NewCodec {
	service := msg.Header["X-Micro-Service"]
	method := msg.Header["X-Micro-Method"]
	endpoint := msg.Header["X-Micro-Endpoint"]
	protocol := msg.Header["X-Micro-Protocol"]
	target := msg.Header["X-Micro-Target"]

	// if the protocol exists (mucp) do nothing
	if len(protocol) > 0 {
		return nil
	}

	// if no service/method/endpoint then it's the old protocol
	if len(service) == 0 && len(method) == 0 && len(endpoint) == 0 {
		return defaultCodecs[msg.Header["Content-Type"]]
	}

	// old target method specified
	if len(target) > 0 {
		return defaultCodecs[msg.Header["Content-Type"]]
	}

	// no method then set to endpoint
	if len(method) == 0 {
		msg.Header["X-Micro-Method"] = method
	}

	// no endpoint then set to method
	if len(endpoint) == 0 {
		msg.Header["X-Micro-Endpoint"] = method
	}

	return nil
}

func newRpcCodec(req *transport.Message, socket transport.Socket, c codec.NewCodec) codec.Codec {
	rwc := &readWriteCloser{
		rbuf: bytes.NewBuffer(req.Body),
		wbuf: bytes.NewBuffer(nil),
	}
	r := &rpcCodec{
		first:  true,
		buf:    rwc,
		codec:  c(rwc),
		req:    req,
		socket: socket,
	}
	return r
}

func (c *rpcCodec) ReadHeader(r *codec.Message, t codec.MessageType) error {
	// the initieal message
	m := codec.Message{
		Header: c.req.Header,
		Body:   c.req.Body,
	}

	// if its a follow on request read it
	if !c.first {
		var tm transport.Message

		// read off the socket
		if err := c.socket.Recv(&tm); err != nil {
			return err
		}
		// reset the read buffer
		c.buf.rbuf.Reset()

		// write the body to the buffer
		if _, err := c.buf.rbuf.Write(tm.Body); err != nil {
			return err
		}

		// set the message header
		m.Header = tm.Header
		// set the message body
		m.Body = tm.Body

		// set req
		c.req = &tm
	}

	// no longer first read
	c.first = false

	// set some internal things
	m.Target = m.Header["X-Micro-Service"]
	m.Method = m.Header["X-Micro-Method"]
	m.Endpoint = m.Header["X-Micro-Endpoint"]
	m.Id = m.Header["X-Micro-Id"]

	// read header via codec
	err := c.codec.ReadHeader(&m, codec.Request)

	// set the method/id
	r.Method = m.Method
	r.Endpoint = m.Endpoint
	r.Id = m.Id

	// TODO: remove the old legacy cruft
	if len(r.Endpoint) == 0 {
		r.Endpoint = r.Method
	}

	return err
}

func (c *rpcCodec) ReadBody(b interface{}) error {
	// don't read empty body
	if len(c.req.Body) == 0 {
		return nil
	}
	// read raw data
	if v, ok := b.(*raw.Frame); ok {
		v.Data = c.req.Body
		return nil
	}
	// decode the usual way
	return c.codec.ReadBody(b)
}

func (c *rpcCodec) Write(r *codec.Message, b interface{}) error {
	c.buf.wbuf.Reset()

	// create a new message
	m := &codec.Message{
		Target:   r.Target,
		Method:   r.Method,
		Endpoint: r.Endpoint,
		Id:       r.Id,
		Error:    r.Error,
		Type:     r.Type,
		Header:   r.Header,
	}

	if m.Header == nil {
		m.Header = map[string]string{}
	}

	// set request id
	if len(r.Id) > 0 {
		m.Header["X-Micro-Id"] = r.Id
	}

	// set target
	if len(r.Target) > 0 {
		m.Header["X-Micro-Service"] = r.Target
	}

	// set request method
	if len(r.Method) > 0 {
		m.Header["X-Micro-Method"] = r.Method
	}

	// set request endpoint
	if len(r.Endpoint) > 0 {
		m.Header["X-Micro-Endpoint"] = r.Endpoint
	}

	if len(r.Error) > 0 {
		m.Header["X-Micro-Error"] = r.Error
	}

	// the body being sent
	var body []byte

	// is it a raw frame?
	if v, ok := b.(*raw.Frame); ok {
		body = v.Data
		// if we have encoded data just send it
	} else if len(r.Body) > 0 {
		body = r.Body
		// write the body to codec
	} else if err := c.codec.Write(m, b); err != nil {
		c.buf.wbuf.Reset()

		// write an error if it failed
		m.Error = errors.Wrapf(err, "Unable to encode body").Error()
		m.Header["X-Micro-Error"] = m.Error
		// no body to write
		if err := c.codec.Write(m, nil); err != nil {
			return err
		}
	} else {
		// set the body
		body = c.buf.wbuf.Bytes()
	}

	// Set content type if theres content
	if len(body) > 0 {
		m.Header["Content-Type"] = c.req.Header["Content-Type"]
	}

	// send on the socket
	return c.socket.Send(&transport.Message{
		Header: m.Header,
		Body:   body,
	})
}

func (c *rpcCodec) Close() error {
	c.buf.Close()
	c.codec.Close()
	return c.socket.Close()
}

func (c *rpcCodec) String() string {
	return "rpc"
}
